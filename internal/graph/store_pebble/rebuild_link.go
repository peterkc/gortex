package store_pebble

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/cockroachdb/pebble/v2/sstable"
	"github.com/zzet/gortex/internal/graph"
)

const linkInFlight = 16

var (
	// ErrRebuildLink marks a failed candidate pass; partial results are invalid.
	ErrRebuildLink = errors.New("rebuild link candidate pass failed")
	errLinkClosed  = errors.New("rebuild link pass closed")
)

// RebuildLinkOptions identifies the re-exec command, its private scratch
// parent, and the result buffer limit. The child receives the partition path
// and decimal spill threshold as the last two arguments after WorkerArgs.
type RebuildLinkOptions struct {
	Binary        string
	WorkerArgs    []string
	ScratchParent string
	Partitions    int
	SpillBytes    int
	reverse       bool // tests launch in the other slot order without changing key ownership
}

// RebuildLink owns one immutable candidate pass and its exiting child processes.
// Concurrent callers may select from that snapshot until Close or failure.
type RebuildLink struct {
	ctx     context.Context
	cancel  context.CancelCauseFunc
	dir     string
	parts   int
	workers [linkPartitionLimit]*linkWorker
	active  chan struct{}
	done    chan struct{}
	cleanup error // read only after done closes
}

type linkWorker struct {
	part    int
	path    string
	cmd     *exec.Cmd
	input   io.WriteCloser
	output  *os.File
	ready   chan LinkPageStats
	done    chan struct{}
	waitErr error // read only after done closes

	// Serialize frame writes, not round trips. The separate pending lock
	// lets the reader dispatch responses even while a write is blocked.
	writeMu   sync.Mutex
	nextID    uint64
	pendingMu sync.Mutex
	pending   map[uint64]chan LinkPageStats
}

// StartRebuildLink streams immutable node facts from source in candidate order.
// source must invoke yield serially, stop at the first error, and propagate it.
// The result owns a fresh directory inside an existing private build lease.
// ctx owns the whole pass; the caller must Close it once finished.
func StartRebuildLink(ctx context.Context, opts RebuildLinkOptions, source func(yield func(*graph.Node) error) error) (*RebuildLink, error) {
	if opts.Partitions < 1 || opts.Partitions > linkPartitionLimit || opts.SpillBytes < 1 || opts.SpillBytes > linkResultBytes ||
		!filepath.IsAbs(opts.Binary) || !filepath.IsAbs(opts.ScratchParent) || source == nil {
		return nil, fmt.Errorf("%w: invalid command, scratch lease, source or limits", ErrRebuildLink)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRebuildLink, err)
	}
	dir, err := os.MkdirTemp(opts.ScratchParent, "link-")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRebuildLink, err)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	s := &RebuildLink{ctx: ctx, cancel: cancel, dir: dir, parts: opts.Partitions, active: make(chan struct{}, linkInFlight), done: make(chan struct{})}
	err = s.start(opts, source)
	go s.reap()
	if err != nil {
		return nil, s.fail(err)
	}
	for _, worker := range s.workers[:s.parts] {
		select {
		case <-ctx.Done():
			return nil, s.fail(context.Cause(ctx))
		case stats := <-worker.ready:
			if stats != (LinkPageStats{}) {
				return nil, s.fail(fmt.Errorf("partition %d sent counts at startup", worker.part))
			}
		}
	}
	if err := context.Cause(ctx); err != nil {
		return nil, s.fail(err)
	}
	return s, nil
}

func (s *RebuildLink) start(opts RebuildLinkOptions, source func(func(*graph.Node) error) error) error {
	if err := buildLinkTables(s.ctx, s.dir, s.parts, source); err != nil {
		return fmt.Errorf("write candidate partitions: %w", err)
	}
	for slot := range s.parts {
		part := slot
		if opts.reverse {
			part = s.parts - 1 - slot
		}
		path := linkPartitionPath(s.dir, part)
		args := append(append([]string(nil), opts.WorkerArgs...), path, strconv.Itoa(opts.SpillBytes))
		cmd := exec.CommandContext(s.ctx, opts.Binary, args...)
		input, err := cmd.StdinPipe()
		if err != nil {
			return fmt.Errorf("partition %d stdin: %w", part, err)
		}
		output, childOutput, err := os.Pipe()
		if err != nil {
			return errors.Join(err, input.Close())
		}
		cmd.Stdout, cmd.Stderr = childOutput, os.Stderr
		if err := cmd.Start(); err != nil {
			return errors.Join(fmt.Errorf("start partition %d: %w", part, err), input.Close(), output.Close(), childOutput.Close())
		}
		ready := make(chan LinkPageStats, 1)
		w := &linkWorker{
			part: part, path: path, cmd: cmd, input: input, output: output, ready: ready,
			pending: map[uint64]chan LinkPageStats{0: ready}, done: make(chan struct{}),
		}
		s.workers[part] = w
		go w.read(s.cancel)
		if err := childOutput.Close(); err != nil {
			return fmt.Errorf("close partition %d parent write descriptor: %w", part, err)
		}
	}
	return nil
}

type linkPartitionError struct {
	part int
	err  error
}

func (e *linkPartitionError) Error() string { return fmt.Sprintf("partition %d: %v", e.part, e.err) }
func (e *linkPartitionError) Unwrap() error { return e.err }

func (w *linkWorker) read(cancel context.CancelCauseFunc) {
	defer close(w.done)
	err := w.readReplies()
	cancel(&linkPartitionError{part: w.part, err: err})
	w.output.Close()
	// The context stops the child if it is still alive. Wait is done once,
	// after the reader can no longer consume an error frame from the child.
	w.waitErr = w.cmd.Wait()
}

func (w *linkWorker) readReplies() error {
	for {
		id, body, err := readLinkFrame(w.output)
		if err != nil {
			return fmt.Errorf("read link reply: %w", err)
		}
		w.pendingMu.Lock()
		ch := w.pending[id]
		delete(w.pending, id)
		w.pendingMu.Unlock()
		if ch == nil {
			return fmt.Errorf("unknown link reply ID %d", id)
		}
		stats, err := readLinkReply(body)
		if err != nil {
			return fmt.Errorf("link reply %d: %w", id, err)
		}
		ch <- stats
	}
}

func (w *linkWorker) send(ctx context.Context, raw []byte) (uint64, <-chan LinkPageStats, error) {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	if err := context.Cause(ctx); err != nil {
		return 0, nil, err
	}
	w.nextID++
	id := w.nextID
	ch := make(chan LinkPageStats, 1)
	w.pendingMu.Lock()
	w.pending[id] = ch
	w.pendingMu.Unlock()
	if err := writeLinkFrame(w.input, id, raw); err != nil {
		// A child that closed its input need not close its output. Refuse
		// now so the pass owner cancels and reaps it.
		return 0, nil, fmt.Errorf("write link request %d: %w", id, err)
	}
	return id, ch, nil
}

func (s *RebuildLink) reap() {
	<-s.ctx.Done()
	for _, worker := range s.workers[:s.parts] {
		if worker != nil {
			<-worker.done
		}
	}
	// SelectPage releases its slot after closing reply readers and before
	// waiting for reap. Take all slots before removing the leased files.
	for range cap(s.active) {
		s.active <- struct{}{}
	}
	s.cleanup = os.RemoveAll(s.dir)
	close(s.done)
}

func (s *RebuildLink) fail(err error) error {
	s.cancel(err)
	<-s.done
	cause := context.Cause(s.ctx)
	var partition *linkPartitionError
	if errors.As(cause, &partition) {
		cause = errors.Join(cause, s.workers[partition.part].waitErr)
	}
	return fmt.Errorf("%w: %w", ErrRebuildLink, errors.Join(cause, s.cleanup))
}

// Close stops intake, cancels and reaps the children, and removes only the
// directory this pass created. It returns any earlier pass failure.
func (s *RebuildLink) Close() error {
	s.cancel(errLinkClosed)
	<-s.done
	if context.Cause(s.ctx) == errLinkClosed && s.cleanup == nil {
		return nil
	}
	return s.fail(context.Cause(s.ctx))
}

// SelectPage streams candidates in request-position order, then source order
// within each position. Duplicate positions in the input remain separate
// requests; a miss has no visits. No visit occurs before every partition has
// replied successfully. Visits are tentative: the caller must not apply them
// unless the whole page succeeds (e.g. a result SST can fail mid-read).
// The caller keeps requests immutable until return. Any page error, including
// a canceled request or a visitor error, stops and reaps the whole pass.
func (s *RebuildLink) SelectPage(ctx context.Context, requests []LinkRequest, visit func(position int, candidate *graph.Node) error) (stats LinkPageStats, err error) {
	stop := context.AfterFunc(ctx, func() { s.cancel(context.Cause(ctx)) })
	defer stop()
	defer func() {
		if err == nil {
			err = context.Cause(ctx)
		}
		if err == nil {
			err = context.Cause(s.ctx)
		}
		if err != nil {
			stats, err = LinkPageStats{}, s.fail(err)
		}
	}()
	select {
	case <-ctx.Done():
		return stats, context.Cause(ctx)
	case <-s.ctx.Done():
		return stats, context.Cause(s.ctx)
	case s.active <- struct{}{}:
		defer func() { <-s.active }()
	}
	if err := context.Cause(s.ctx); err != nil {
		return stats, err
	}
	if len(requests) > LinkPageRequests || visit == nil {
		return stats, errors.New("invalid link page length or visitor")
	}
	parts, pages, err := s.routePage(requests)
	if err != nil {
		return stats, err
	}
	replies, err := s.requestPage(pages)
	if err != nil {
		return stats, err
	}
	for _, reply := range replies {
		stats.ScannedRows += reply.stats.ScannedRows
		stats.ResultRows += reply.stats.ResultRows
		stats.ResultBytes += reply.stats.ResultBytes
		stats.SpillRows += reply.stats.SpillRows
		stats.SpillBytes += reply.stats.SpillBytes
	}
	if stats.ResultRows > linkResultRows || stats.ResultBytes > linkResultBytes {
		return stats, errors.New("merged candidate page exceeds row or byte bound")
	}
	if err := mergeLinkReplies(s.ctx, requests, parts, replies, func(pos int, n *graph.Node) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return visit(pos, n)
	}); err != nil {
		return stats, err
	}
	for _, reply := range replies {
		if reply.path != "" {
			if err := os.Remove(reply.path); err != nil {
				return stats, err
			}
		}
	}
	return stats, nil
}

func (s *RebuildLink) routePage(requests []LinkRequest) ([]int, [linkPartitionLimit][]byte, error) {
	var pages [linkPartitionLimit][]byte
	parts := make([]int, len(requests))
	remaining := linkFrameBytes
	for pos, req := range requests {
		if !req.valid() {
			return nil, pages, fmt.Errorf("invalid link request at position %d", pos)
		}
		if len(req.Key) > remaining {
			return nil, pages, errors.New("link request key exceeds wire byte bound")
		}
		part := linkPartition(linkPrefix(req.Kind, req.Key), s.parts)
		parts[pos] = part
		page, err := appendLinkRequest(pages[part], req, pos, remaining)
		if err != nil {
			return nil, pages, err
		}
		remaining -= len(page) - len(pages[part])
		pages[part] = page
	}
	return parts, pages, nil
}

type linkReplyFile struct {
	path  string
	stats LinkPageStats
}

func (s *RebuildLink) requestPage(pages [linkPartitionLimit][]byte) ([linkPartitionLimit]linkReplyFile, error) {
	var files [linkPartitionLimit]linkReplyFile
	var waits [linkPartitionLimit]<-chan LinkPageStats
	var ids [linkPartitionLimit]uint64
	for part, page := range pages[:s.parts] {
		if len(page) == 0 {
			continue
		}
		id, ch, err := s.workers[part].send(s.ctx, page)
		if err != nil {
			return files, err
		}
		waits[part], ids[part] = ch, id
	}
	for part, ch := range waits {
		if ch == nil {
			continue
		}
		select {
		case <-s.ctx.Done():
			return files, context.Cause(s.ctx)
		case stats := <-ch:
			files[part].stats = stats
			if stats.ResultRows != 0 {
				files[part].path = linkReplyPath(s.workers[part].path, ids[part])
			}
		}
	}
	return files, nil
}

type linkReplyCursor struct {
	it          sstable.Iterator
	current     row
	rows, bytes int
}

func (c *linkReplyCursor) advance(first bool) error {
	next := c.it.Next
	if first {
		next = c.it.First
	}
	kv := next()
	if kv == nil {
		c.current = row{}
		return c.it.Error()
	}
	if len(kv.K.UserKey) != linkResultKeyBytes {
		return errors.New("invalid link result key")
	}
	raw, _, err := kv.Value(nil)
	if err != nil {
		return err
	}
	if len(raw)+linkResultKeyBytes > linkRecordBytes {
		return errors.New("oversized link result record")
	}
	c.current = row{kv.K.UserKey, raw}
	c.rows++
	c.bytes += len(raw) + linkResultKeyBytes
	return nil
}

func mergeLinkReplies(ctx context.Context, requests []LinkRequest, parts []int, replies [linkPartitionLimit]linkReplyFile, visit func(int, *graph.Node) error) (err error) {
	var cursors [linkPartitionLimit]linkReplyCursor
	for part, reply := range replies {
		if reply.path == "" {
			continue
		}
		table, e := openLinkTable(ctx, reply.path)
		if e != nil {
			return fmt.Errorf("open partition %d result: %w", part, e)
		}
		defer func() { err = errors.Join(err, table.Close()) }()
		it, e := table.NewIter(sstable.NoTransforms, nil, nil, sstable.TableBlobContext{})
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, it.Close()) }()
		cursors[part].it = it
		if e := cursors[part].advance(true); e != nil {
			return e
		}
	}
	for pos, req := range requests {
		part := parts[pos]
		c := &cursors[part]
		for c.current.key != nil && binary.BigEndian.Uint32(c.current.key) == uint32(pos) {
			if err := ctx.Err(); err != nil {
				return err
			}
			if c.rows > replies[part].stats.ResultRows || c.bytes > replies[part].stats.ResultBytes {
				return fmt.Errorf("partition %d exceeded reported result counts", part)
			}
			n, err := decodeNode(c.current.value)
			if err != nil {
				return err
			}
			if !req.match(n) {
				return fmt.Errorf("partition %d result does not match request position %d", part, pos)
			}
			if err := visit(pos, n); err != nil {
				return err
			}
			if err := c.advance(false); err != nil {
				return err
			}
		}
	}
	for part, c := range cursors {
		if c.current.key != nil || c.rows != replies[part].stats.ResultRows || c.bytes != replies[part].stats.ResultBytes {
			return fmt.Errorf("partition %d result position or count mismatch", part)
		}
	}
	return nil
}

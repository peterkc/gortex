package store_pebble

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/zzet/gortex/internal/graph"
)

const linkInFlight = 16

var (
	// ErrRebuildLink marks a failed candidate pass; partial results are invalid.
	ErrRebuildLink = errors.New("rebuild link candidate pass failed")
	errLinkClosed  = errors.New("rebuild link pass closed")
)

// RebuildLinkOptions identifies the private scratch lease and logical
// result run threshold. The decoded cache budget is fixed by the reader.
type RebuildLinkOptions struct {
	ScratchParent string
	SpillBytes    int
}

// RebuildLink owns one immutable ordered candidate file and its decoded cache.
// The main coordinator selects serially until Close or failure.
type RebuildLink struct {
	ctx                context.Context
	cancel             context.CancelCauseFunc
	dir                string
	active             chan struct{}
	done               chan struct{}
	cleanup            error // read only after done closes
	v3                 *v3Reader
	candidateCalls     atomic.Uint64
	candidatePositions atomic.Uint64
}

// LinkRequestProgress counts logical candidate page calls and admitted
// request positions, including checked misses. Reducer tests use the counts
// to check conditional calls and batch splitting.
type LinkRequestProgress struct {
	CandidateCalls, CandidatePositions uint64
}

func (s *RebuildLink) RequestProgress() LinkRequestProgress {
	return LinkRequestProgress{CandidateCalls: s.candidateCalls.Load(), CandidatePositions: s.candidatePositions.Load()}
}

// StartRebuildLink opens the ordered candidate file and its fixed-budget
// decoded row cache. The working view supplies the initial index sizing facts.
func StartRebuildLink(ctx context.Context, opts RebuildLinkOptions, view RebuildView, source func(func(*graph.Node) error) error) (*RebuildLink, error) {
	return startRebuildLink(ctx, opts, view, rebuildCandidateCacheBytes, source)
}

// A small budget is used by cache-boundary tests, not product configuration.
func startRebuildLink(ctx context.Context, opts RebuildLinkOptions, view RebuildView, budget int64, source func(func(*graph.Node) error) error) (*RebuildLink, error) {
	if opts.SpillBytes < 1 || opts.SpillBytes > linkResultBytes || !filepath.IsAbs(opts.ScratchParent) || source == nil {
		return nil, fmt.Errorf("%w: invalid scratch lease, source or limits", ErrRebuildLink)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRebuildLink, err)
	}
	dir, err := os.MkdirTemp(opts.ScratchParent, "link-")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRebuildLink, err)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	s := &RebuildLink{ctx: ctx, cancel: cancel, dir: dir, active: make(chan struct{}, linkInFlight), done: make(chan struct{})}
	s.v3, err = openV3Reader(ctx, dir, view, budget, source)
	go s.reap()
	if err != nil {
		return nil, s.fail(fmt.Errorf("write candidate file: %w", err))
	}
	if err := context.Cause(ctx); err != nil {
		return nil, s.fail(err)
	}
	return s, nil
}

func (s *RebuildLink) reap() {
	<-s.ctx.Done()
	// A candidate call releases its slot before waiting for reap.
	for range cap(s.active) {
		s.active <- struct{}{}
	}
	if s.v3 != nil { // reader construction can fail before ownership transfers
		s.cleanup = s.v3.close()
	}
	s.cleanup = errors.Join(s.cleanup, os.RemoveAll(s.dir))
	close(s.done)
}

func (s *RebuildLink) fail(err error) error {
	s.cancel(err)
	<-s.done
	return fmt.Errorf("%w: %w", ErrRebuildLink, errors.Join(context.Cause(s.ctx), s.cleanup))
}

// Close stops intake, closes the ordered reader, and removes only the
// directory this pass created. It returns any earlier pass failure.
func (s *RebuildLink) Close() error {
	s.cancel(errLinkClosed)
	<-s.done
	if context.Cause(s.ctx) == errLinkClosed && s.cleanup == nil {
		return nil
	}
	return s.fail(context.Cause(s.ctx))
}

package store_pebble

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cockroachdb/pebble/v2/sstable"
	"github.com/zzet/gortex/internal/graph"
)

// CandidatePageStats counts coordinator results and threshold-triggered runs.
// ResultBytes counts serialized keys and values. SpillBytes counts on-disk
// bytes of overflow runs, not the final run written to drain the buffer.
type CandidatePageStats struct {
	ResultRows  int
	ResultBytes int
	SpillRows   int
	SpillRuns   int
	SpillBytes  int64
}

// SelectCandidatePage visits one coordinator-owned page in request-position
// order, then native candidate order. Only AllRepos exact-name requests read
// the live working view. Qualified-name and scoped name requests use the
// immutable pass, whose SelectPage transport remains in source order.
//
// The coordinator calls this serially and keeps requests and the working view
// unchanged until it returns. Visits are tentative: stage decisions only and
// apply nothing unless the call returns nil. Any error fails and reaps the pass.
func (s *RebuildLink) SelectCandidatePage(ctx context.Context, view RebuildView, requests []LinkRequest, spillBytes int, visit func(int, *graph.Node) error) (stats CandidatePageStats, err error) {
	pageCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stop := context.AfterFunc(s.ctx, func() { cancel(context.Cause(s.ctx)) })
	defer stop()

	var dir string
	defer func() {
		var cleanup error
		if dir != "" {
			cleanup = os.RemoveAll(dir)
		}
		if err == nil {
			err = errors.Join(context.Cause(ctx), context.Cause(s.ctx), cleanup)
			cleanup = nil
		}
		if err != nil {
			stats, err = CandidatePageStats{}, errors.Join(s.fail(err), cleanup)
		}
	}()
	if err := errors.Join(context.Cause(ctx), context.Cause(s.ctx)); err != nil {
		return stats, err
	}
	if len(requests) > LinkPageRequests || spillBytes < 1 || spillBytes > linkResultBytes || visit == nil {
		return stats, errors.New("invalid candidate page length, spill limit or visitor")
	}
	immutable, positions, err := splitCandidateRequests(requests)
	if err != nil || len(requests) == 0 {
		return stats, err
	}
	if len(immutable) != len(requests) && view.build == nil {
		return stats, errors.New("live candidate page requires a working view")
	}
	// SelectPage can reap its own directory before returning an error.
	// Page runs have an independent lifetime under the same private lease.
	dir, err = os.MkdirTemp(filepath.Dir(s.dir), "candidate-page-")
	if err != nil {
		return stats, err
	}
	page := candidatePageOrder{runs: sortedRuns{
		dir: dir, name: "order", limit: spillBytes,
		options: headOptions(), format: sstable.TableFormatPebblev4,
	}}
	if len(immutable) > 0 {
		if _, err := s.SelectPage(pageCtx, immutable, func(i int, n *graph.Node) error {
			pos := positions[i]
			return page.add(requests[pos], pos, n)
		}); err != nil {
			return stats, err
		}
	}
	for pos, req := range requests {
		if !liveCandidateRequest(req) {
			continue
		}
		if err := view.VisitNameCandidates(pageCtx, req.Key, req.Languages, func(n *graph.Node) error {
			return page.add(req, pos, n)
		}); err != nil {
			return stats, err
		}
	}
	return page.finish(pageCtx, visit)
}

func liveCandidateRequest(req LinkRequest) bool { return req.Kind == LinkName && req.AllRepos }

func splitCandidateRequests(requests []LinkRequest) ([]LinkRequest, []int, error) {
	immutable := make([]LinkRequest, 0, len(requests))
	positions := make([]int, 0, len(requests))
	var wire []byte
	remaining := linkFrameBytes
	for pos, req := range requests {
		if !req.valid() {
			return nil, nil, fmt.Errorf("invalid candidate request at position %d", pos)
		}
		// Enforce the same aggregate byte bound on both routes. Reuse only
		// one record's frame storage; the split copies no strings or lists.
		var err error
		wire, err = appendLinkRequest(wire[:0], req, pos, remaining)
		if err != nil {
			return nil, nil, fmt.Errorf("candidate request at position %d: %w", pos, err)
		}
		remaining -= len(wire)
		if !liveCandidateRequest(req) {
			immutable = append(immutable, req)
			positions = append(positions, pos)
		}
	}
	return immutable, positions, nil
}

type candidatePageOrder struct {
	runs  sortedRuns
	stats CandidatePageStats
}

func (p *candidatePageOrder) add(req LinkRequest, pos int, n *graph.Node) error {
	// This tuple mirrors graph.resolverNameScopeSort and the P3 qualified
	// lookup's ID order. Keep source order for ties in the complete tuple.
	key := binary.BigEndian.AppendUint32(nil, uint32(pos))
	if req.Kind == LinkName {
		if req.AllRepos {
			key = appendKeyString(key, n.RepoPrefix)
		}
		if len(req.Languages) > 0 {
			key = appendKeyString(key, n.Language)
		}
	}
	key = appendKeyString(key, n.ID)
	key = binary.BigEndian.AppendUint64(key, uint64(p.stats.ResultRows))
	value, err := encodeNode(n)
	if err != nil {
		return err
	}
	size := len(key) + len(value)
	if size > linkRecordBytes || p.stats.ResultRows >= linkResultRows || p.stats.ResultBytes+size > linkResultBytes {
		return errors.New("candidate order exceeds link record or page bound")
	}
	priorRows, priorRuns := len(p.runs.rows), len(p.runs.paths)
	if err := p.runs.add(key, value); err != nil {
		return err
	}
	if len(p.runs.paths) != priorRuns {
		p.stats.SpillRows += priorRows
	}
	p.stats.ResultRows++
	p.stats.ResultBytes += size
	return nil
}

func (p *candidatePageOrder) finish(ctx context.Context, visit func(int, *graph.Node) error) (CandidatePageStats, error) {
	p.stats.SpillRuns = len(p.runs.paths)
	for _, path := range p.runs.paths {
		if err := ctx.Err(); err != nil {
			return CandidatePageStats{}, err
		}
		info, err := os.Stat(path)
		if err != nil {
			return CandidatePageStats{}, err
		}
		p.stats.SpillBytes += info.Size()
	}
	err := p.runs.merge(ctx, func(r row) error {
		n, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		return visit(int(binary.BigEndian.Uint32(r.key)), n)
	})
	return p.stats, err
}

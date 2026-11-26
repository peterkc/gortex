package store_pebble

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
)

// CandidatePageStats counts complete logical coordinator results and bytes,
// including repeated positions and hits in the decoded cache.
type CandidatePageStats struct {
	ResultRows  int
	ResultBytes int
}

// SelectCandidatePage visits one coordinator-owned page in request-position
// order, then native candidate order. Only AllRepos exact-name requests read
// the live working view. Qualified-name and scoped name requests use the
// immutable ordered file.
//
// The coordinator calls this serially and keeps requests and the working view
// unchanged until it returns. Visits are tentative: stage decisions only and
// apply nothing unless the call returns nil. Any error fails and reaps the pass.
func (s *RebuildLink) SelectCandidatePage(ctx context.Context, view RebuildView, requests []LinkRequest, spillBytes int, visit func(int, *graph.Node) error) (stats CandidatePageStats, err error) {
	s.candidateCalls.Add(1)
	pageCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stop := context.AfterFunc(s.ctx, func() { cancel(context.Cause(s.ctx)) })
	defer stop()

	defer func() {
		if err == nil {
			err = errors.Join(context.Cause(ctx), context.Cause(s.ctx))
		}
		if err != nil {
			stats, err = CandidatePageStats{}, s.fail(err)
		}
	}()
	if err := errors.Join(context.Cause(ctx), context.Cause(s.ctx)); err != nil {
		return stats, err
	}
	if len(requests) > LinkPageRequests || spillBytes < 1 || spillBytes > linkResultBytes || visit == nil {
		return stats, errors.New("invalid candidate page length, spill limit or visitor")
	}
	live, err := checkCandidateRequests(requests)
	if err != nil || len(requests) == 0 {
		return stats, err
	}
	if live && view.build == nil {
		return stats, errors.New("live candidate page requires a working view")
	}
	select {
	case <-pageCtx.Done():
		return stats, context.Cause(pageCtx)
	case s.active <- struct{}{}:
	}
	defer func() { <-s.active }()
	s.candidatePositions.Add(uint64(len(requests)))
	return s.v3.selectPage(pageCtx, view, requests, spillBytes, visit)
}

func liveCandidateRequest(req LinkRequest) bool { return req.Kind == LinkName && req.AllRepos }

func checkCandidateRequests(requests []LinkRequest) (live bool, err error) {
	var wire []byte
	remaining := linkFrameBytes
	for pos, req := range requests {
		if !req.valid() {
			return false, fmt.Errorf("invalid candidate request at position %d", pos)
		}
		// Preserve the historical aggregate wire bound. Reuse only one
		// record's encoding buffer and do not retain request copies.
		var err error
		wire, err = appendLinkRequest(wire[:0], req, pos, remaining)
		if err != nil {
			return false, fmt.Errorf("candidate request at position %d: %w", pos, err)
		}
		remaining -= len(wire)
		live = live || liveCandidateRequest(req)
	}
	return live, nil
}

func candidateOrderKey(req LinkRequest, pos int, n *graph.Node, ordinal uint64) []byte {
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
	return binary.BigEndian.AppendUint64(key, ordinal)
}

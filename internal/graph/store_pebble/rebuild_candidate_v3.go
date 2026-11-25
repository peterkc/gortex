package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sort"
	"sync"

	"github.com/cockroachdb/pebble/v2/sstable"
	"github.com/zzet/gortex/internal/graph"
)

type v3Reader struct {
	mu         sync.Mutex // serial cache and summary access
	summaries  map[string]v3Summary
	buildEpoch uint64
	buildView  *BuildStore
	table      *sstable.Reader
	cache      *v3Cache
}

type v3LiveRow struct {
	key, raw []byte
}

func (v *v3Reader) close() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.summaries = nil
	return errors.Join(v.cache.close(), v.table.Close())
}

// ResetCandidatePage drops page-local summaries. The immutable decoded cache
// spans the pass; working-view charges and live rows never enter either cache.
func (s *RebuildLink) ResetCandidatePage() {
	s.v3.mu.Lock()
	defer s.v3.mu.Unlock()
	s.v3.summaries = nil
}

func (s *RebuildLink) ChargeCandidateRequest(ctx context.Context, view RebuildView, req LinkRequest) (CandidateBatchCharge, error) {
	v := s.v3
	select {
	case <-s.ctx.Done():
		return CandidateBatchCharge{}, context.Cause(s.ctx)
	case <-ctx.Done():
		return CandidateBatchCharge{}, ctx.Err()
	case s.active <- struct{}{}:
	}
	defer func() { <-s.active }()
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := context.Cause(s.ctx); err != nil {
		return CandidateBatchCharge{}, err
	}
	if !req.valid() || req.Kind == LinkQualName && (!req.AllRepos || len(req.Languages) != 0) {
		return CandidateBatchCharge{Oversize: true}, nil
	}
	if liveCandidateRequest(req) {
		return view.ChargeCandidateRequest(ctx, req)
	}
	if view.build == nil {
		return view.ChargeCandidateRequest(ctx, req)
	}
	view.build.mu.Lock()
	changed := v.buildView != view.build || v.buildEpoch != view.build.v3NodeWrites
	mutationErr := view.build.first
	view.build.mu.Unlock()
	if changed || mutationErr != nil {
		return view.ChargeCandidateRequest(ctx, req)
	}
	sum, err := v.requestSummary(ctx, req)
	if err != nil {
		return CandidateBatchCharge{}, err
	}
	if sum.unsized || sum.maxOrder > linkRecordBytes || sum.rows > linkResultRows || sum.orderBytes > linkResultBytes {
		// H98 accumulates a partial charge even when it routes the request to a
		// singleton. Read the original compact index to preserve those exact
		// partial counters; the singleton still uses the same ordered reader.
		return view.ChargeCandidateRequest(ctx, req)
	}
	return CandidateBatchCharge{Rows: sum.rows, Bytes: sum.orderBytes}, nil
}

type v3Checks struct {
	stats CandidatePageStats
	runs  v3LogicalRuns
	exact bool
	spill int
}

func (c *v3Checks) add(size int) error {
	if size > linkRecordBytes || c.stats.ResultRows >= linkResultRows || size > linkResultBytes-c.stats.ResultBytes {
		return errors.New("candidate order exceeds link record or page bound")
	}
	if c.exact {
		if err := c.runs.add(size, c.spill); err != nil {
			return err
		}
	}
	c.stats.ResultRows++
	c.stats.ResultBytes += size
	return nil
}
func (c *v3Checks) finish() error {
	if c.exact {
		if err := c.runs.finish(); err != nil {
			return err
		}
	}
	return nil
}

type v3StageRow struct {
	size    int
	ordinal uint64
	err     error
}

func (v *v3Reader) checkImmutable(ctx context.Context, req LinkRequest, sum v3Summary, c *v3Checks) error {
	// With globally admissible row/byte totals, the only reason to replay
	// source order is an unsized/failed codec or the ambiguous run boundary.
	replay := c.exact || sum.unsized || sum.maxOrder > linkRecordBytes || sum.rows > linkResultRows-c.stats.ResultRows || sum.orderBytes > linkResultBytes-c.stats.ResultBytes
	var source []v3StageRow
	rows, size := 0, 0
	err := v.walk(ctx, req, func(r v3CachedRow) error {
		rows++
		size += r.orderBytes
		if replay {
			source = append(source, v3StageRow{r.orderBytes, r.ordinal, r.err})
			return nil
		}
		if r.err != nil {
			return r.err
		}
		return c.add(r.orderBytes)
	})
	if err != nil {
		return err
	}
	if replay {
		sort.Slice(source, func(i, j int) bool { return source[i].ordinal < source[j].ordinal })
		for _, r := range source {
			if r.err != nil {
				return r.err
			}
			if err := c.add(r.size); err != nil {
				return err
			}
		}
	}
	if rows != sum.rows || (!sum.unsized && size != sum.orderBytes) {
		return invalid("v3 summary differs from candidate rows")
	}
	return nil
}

// Each live request scans the working view anew. Stage raw canonical values
// only within the original logical result cap. The wildcard reorder happens
// after all immutable and live checks succeed, never before a refusal.
func (v *v3Reader) checkLive(ctx context.Context, view RebuildView, req LinkRequest, c *v3Checks) ([]v3LiveRow, error) {
	var rows []v3LiveRow
	err := view.VisitNameCandidates(ctx, req.Key, req.Languages, func(n *graph.Node) error {
		raw, err := encodeNode(n)
		if err != nil {
			return err
		}
		key := candidateOrderKey(req, 0, n, uint64(len(rows)))
		if err := c.add(len(key) + len(raw)); err != nil {
			return err
		}
		rows = append(rows, v3LiveRow{key: key, raw: raw})
		return nil
	})
	return rows, err
}

func (v *v3Reader) visitLive(ctx context.Context, req LinkRequest, rows []v3LiveRow, visit func(*graph.Node) error) error {
	if len(req.Languages) == 0 && len(rows) > 1 {
		slices.SortFunc(rows, func(a, b v3LiveRow) int { return bytes.Compare(a.key, b.key) })
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := decodeNode(row.raw)
		if err != nil {
			return err
		}
		if err := visit(n); err != nil {
			return err
		}
	}
	return nil
}

func (v *v3Reader) selectPage(ctx context.Context, view RebuildView, requests []LinkRequest, spill int, visit func(int, *graph.Node) error) (CandidatePageStats, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	sums := make([]v3Summary, len(requests))
	totalRows, totalBytes := 0, 0
	// Stage 2: the original one-partition result limits apply to all
	// immutable requests, including duplicates. Check before coordinator or live errors.
	for i, req := range requests {
		if liveCandidateRequest(req) {
			continue
		}
		sum, err := v.requestSummary(ctx, req)
		if err != nil {
			return CandidatePageStats{}, err
		}
		sums[i] = sum
		if sum.maxChild > linkRecordBytes || sum.rows > linkResultRows-totalRows || sum.childBytes > linkResultBytes-totalBytes {
			return CandidatePageStats{}, errors.New("candidate result exceeds link record or page bound")
		}
		totalRows += sum.rows
		totalBytes += sum.childBytes
	}
	// The original merged cap is identical to the preceding one-partition cap.
	// Stage 4: exact original immutable-then-live check order. The worst-case
	// total accepted order payload bounds run count without a size replay.
	checks := v3Checks{spill: spill, exact: min(linkResultRows, 2*(linkResultBytes+48*linkResultRows)/spill+1) > goldFileCap}
	for i, req := range requests {
		if liveCandidateRequest(req) {
			continue
		}
		if err := v.checkImmutable(ctx, req, sums[i], &checks); err != nil {
			return CandidatePageStats{}, err
		}
	}
	lives := make([][]v3LiveRow, len(requests))
	for i, req := range requests {
		if !liveCandidateRequest(req) {
			continue
		}
		run, err := v.checkLive(ctx, view, req, &checks)
		if err != nil {
			return CandidatePageStats{}, err
		}
		lives[i] = run
	}
	if err := checks.finish(); err != nil {
		return CandidatePageStats{}, err
	}
	// Stage 5: all checks succeeded before the first visitor. File order is
	// native order, including duplicate positions and checked misses.
	for pos, req := range requests {
		emit := func(n *graph.Node) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return visit(pos, n)
		}
		var err error
		if liveCandidateRequest(req) {
			err = v.visitLive(ctx, req, lives[pos], emit)
		} else {
			err = v.walk(ctx, req, func(r v3CachedRow) error {
				if r.err != nil {
					return r.err
				}
				return emit(r.node)
			})
		}
		if err != nil {
			return CandidatePageStats{}, err
		}
	}
	return checks.stats, nil
}

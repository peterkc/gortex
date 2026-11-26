package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"slices"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

var (
	_ graph.KindAdjacencyReader      = (*Snapshot)(nil)
	_ graph.KindAdjacencyBatchReader = (*Snapshot)(nil)
	_ graph.FrontierExpander         = (*Snapshot)(nil)
	_ graph.Walker                   = (*Snapshot)(nil)
)

func (s *Snapshot) ReadKindAdjacency(ctx context.Context, id string, direction graph.Direction, kinds []graph.EdgeKind) (graph.KindAdjacency, error) {
	rows, _, err := s.ReadKindAdjacencyBatch(ctx, []string{id}, direction, kinds, 0)
	return rows[id], err
}

func (s *Snapshot) ReadKindAdjacencyBatch(ctx context.Context, ids []string, direction graph.Direction, kinds []graph.EdgeKind, limit int) (map[string]graph.KindAdjacency, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if direction != graph.DirectionForward && direction != graph.DirectionBackward {
		return nil, false, invalid("adjacency direction")
	}
	out := make(map[string]graph.KindAdjacency, len(ids))
	seen := make(map[string]struct{}, len(ids))
	emitted := 0
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		label := "out total"
		if direction == graph.DirectionBackward {
			label = "in total"
		}
		total, err := point(s, countKey(label, id), decodeCount)
		if err != nil {
			return nil, false, err
		}
		row := graph.KindAdjacency{Total: total}
		var capped bool
		err = s.visitKindEdges(ctx, id, direction, kinds, func(e *graph.Edge) (bool, error) {
			if limit > 0 && emitted == limit {
				capped = true
				return false, nil
			}
			row.Edges = append(row.Edges, e)
			emitted++
			return true, nil
		})
		if err != nil {
			return nil, false, err
		}
		out[id] = row
		if capped {
			return out, true, nil
		}
	}
	return out, false, ctx.Err()
}

func (s *Snapshot) visitKindEdges(ctx context.Context, id string, direction graph.Direction, kinds []graph.EdgeKind, visit func(*graph.Edge) (bool, error)) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	type rangeCursor struct {
		i         snapshotIterator
		prefixLen int
	}
	ordered := slices.Clone(kinds)
	slices.Sort(ordered)
	var cursors []rangeCursor
	for _, kind := range slices.Compact(ordered) {
		if !s.profile.keepsEdge(kind) {
			return groupOffError{edgeGroup(kind), s.profile}
		}
		tag := edgeOutKind
		if direction == graph.DirectionBackward {
			tag = edgeIn
		}
		prefix := keyStrings([]byte{familyEdge, tag}, id, string(kind))
		i, err := s.snap.NewIterWithContext(ctx, &pebble.IterOptions{LowerBound: prefix, UpperBound: prefixEnd(prefix)})
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, i.Close()) }()
		if i.First() {
			cursors = append(cursors, rangeCursor{i, len(prefix)})
		} else if err := i.Error(); err != nil {
			return err
		}
	}
	for len(cursors) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		best := 0
		if direction == graph.DirectionForward {
			// Outgoing single reads order by signed line then identity,
			// independent of the kind component in these per-kind keys.
			for j := 1; j < len(cursors); j++ {
				if bytes.Compare(cursors[j].i.Key()[cursors[j].prefixLen:], cursors[best].i.Key()[cursors[best].prefixLen:]) < 0 {
					best = j
				}
			}
		}
		i := cursors[best].i
		raw, err := i.ValueAndErr()
		if err != nil {
			return err
		}
		e, err := decodeEdge(raw)
		if err != nil {
			return err
		}
		more, err := visit(e)
		if err != nil || !more {
			return err
		}
		if !i.Next() {
			if err := i.Error(); err != nil {
				return err
			}
			cursors = slices.Delete(cursors, best, best+1)
		}
	}
	return nil
}

type walkEdge struct {
	edge     *graph.Edge
	neighbor string
}

func (s *Snapshot) visitFrontier(ctx context.Context, ids []string, direction graph.Direction, kinds []graph.EdgeKind, visit func(graph.FrontierHop) (bool, error)) error {
	const batchSize = 32
	pending := make([]walkEdge, 0, batchSize)
	nextIDs := make([]string, 0, batchSize)
	flush := func() (bool, error) {
		if len(pending) == 0 {
			return true, nil
		}
		nodes, err := s.GetNodesByIDsContext(ctx, nextIDs)
		if err != nil {
			return false, err
		}
		for _, row := range pending {
			if n := nodes[row.neighbor]; n != nil {
				more, err := visit(graph.FrontierHop{Edge: row.edge, Neighbor: n})
				if err != nil || !more {
					return more, err
				}
			}
		}
		pending = pending[:0]
		nextIDs = nextIDs[:0]
		return true, nil
	}
	for _, id := range ids {
		directions := []graph.Direction{direction}
		if direction == graph.DirectionBoth {
			directions = []graph.Direction{graph.DirectionForward, graph.DirectionBackward}
		}
		for _, dir := range directions {
			stopped := false
			err := s.visitKindEdges(ctx, id, dir, kinds, func(e *graph.Edge) (bool, error) {
				neighbor := e.To
				if dir == graph.DirectionBackward {
					neighbor = e.From
				}
				pending = append(pending, walkEdge{e, neighbor})
				nextIDs = append(nextIDs, neighbor)
				if len(pending) < batchSize {
					return true, nil
				}
				more, err := flush()
				stopped = !more
				return more, err
			})
			if err != nil || stopped {
				return err
			}
		}
	}
	_, err := flush()
	return err
}

func (s *Snapshot) ExpandFrontier(ids []string, forward bool, kinds []graph.EdgeKind, limit int) []graph.FrontierHop {
	direction := graph.DirectionBackward
	if forward {
		direction = graph.DirectionForward
	}
	var out []graph.FrontierHop
	err := s.visitFrontier(context.Background(), ids, direction, kinds, func(h graph.FrontierHop) (bool, error) {
		out = append(out, h)
		return limit <= 0 || len(out) < limit, nil
	})
	s.retain(err)
	if err != nil {
		return nil
	}
	return out
}

func (s *Snapshot) Walk(ctx context.Context, seeds []string, direction graph.Direction, kinds []graph.EdgeKind, maxDepth, nodeCap, hopCap int) (graph.WalkResult, error) {
	if err := ctx.Err(); err != nil {
		return graph.WalkResult{}, err
	}
	if direction != graph.DirectionForward && direction != graph.DirectionBackward && direction != graph.DirectionBoth {
		return graph.WalkResult{}, invalid("walk direction")
	}
	if len(seeds) == 0 || len(kinds) == 0 || maxDepth <= 0 {
		return graph.WalkResult{}, nil
	}
	var out graph.WalkResult
	seen := make(map[string]struct{})
	var frontier []string
	for _, id := range seeds {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		if nodeCap > 0 && len(seen) == nodeCap {
			return graph.WalkResult{StoppedBy: graph.WalkNodeCap}, nil
		}
		seen[id] = struct{}{}
		frontier = append(frontier, id)
	}
	hops := 0
	for depth := 0; depth < maxDepth && len(frontier) > 0; depth++ {
		var level []graph.FrontierHop
		var next []string
		err := s.visitFrontier(ctx, frontier, direction, kinds, func(h graph.FrontierHop) (bool, error) {
			if hopCap > 0 && hops == hopCap {
				out.StoppedBy = graph.WalkHopCap
				return false, nil
			}
			if id := h.Neighbor.ID; id != "" {
				if _, ok := seen[id]; !ok {
					if nodeCap > 0 && len(seen) == nodeCap {
						out.StoppedBy = graph.WalkNodeCap
						return false, nil
					}
					seen[id] = struct{}{}
					next = append(next, id)
				}
			}
			level = append(level, h)
			hops++
			return true, nil
		})
		if err != nil {
			return graph.WalkResult{}, err
		}
		if len(level) != 0 {
			out.Levels = append(out.Levels, level)
		}
		if out.StoppedBy != "" {
			break
		}
		frontier = next
	}
	if err := ctx.Err(); err != nil {
		return graph.WalkResult{}, err
	}
	return out, nil
}

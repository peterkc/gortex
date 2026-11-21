package store_pebble

import (
	"cmp"
	"context"
	"slices"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
)

var (
	_ graph.BFSCapable               = (*Snapshot)(nil)
	_ graph.DeadCodeCandidator       = (*Snapshot)(nil)
	_ graph.ExtractCandidatesScanner = (*Snapshot)(nil)
)

func (s *Snapshot) BFS(seeds []string, dir graph.Direction, kinds []graph.EdgeKind, depth, limit int) ([]graph.BFSHop, error) {
	kinds = slices.Collect(uniqueNonempty(kinds))
	frontier := slices.Collect(uniqueNonempty(seeds))
	if len(frontier) == 0 {
		return nil, nil
	}
	slices.Sort(frontier)
	seen := make(map[string]bool, len(frontier))
	var out []graph.BFSHop
	for _, id := range frontier {
		out = append(out, graph.BFSHop{NodeID: id})
		seen[id] = true
	}
	if limit > 0 && len(out) >= limit {
		return out[:limit], nil
	}
	if dir != graph.DirectionBackward {
		dir = graph.DirectionForward
	}
	for level := 1; level <= depth && len(frontier) != 0 && len(kinds) != 0; level++ {
		next := make(map[string]graph.BFSHop)
		for _, id := range frontier {
			adj, err := s.ReadKindAdjacency(context.Background(), id, dir, kinds)
			if err != nil {
				return nil, err
			}
			for _, e := range adj.Edges {
				target := e.To
				if dir == graph.DirectionBackward {
					target = e.From
				}
				if seen[target] {
					continue
				}
				if prev, ok := next[target]; ok && (prev.ParentID < id || (prev.ParentID == id && prev.EdgeKind <= e.Kind)) {
					continue
				}
				n, err := point(s, keyStrings([]byte{familyNode}, target), decodeNodeLight)
				if err != nil {
					return nil, err
				}
				if n != nil {
					next[target] = graph.BFSHop{NodeID: target, Depth: level, ParentID: id, EdgeKind: e.Kind}
				}
			}
		}
		frontier = frontier[:0]
		for id := range next {
			frontier = append(frontier, id)
		}
		slices.Sort(frontier)
		for _, id := range frontier {
			seen[id] = true
			out = append(out, next[id])
			if limit > 0 && len(out) == limit {
				return out, nil
			}
		}
	}
	return out, nil
}

func (s *Snapshot) DeadCodeCandidates(kinds []graph.NodeKind, allowed map[graph.NodeKind][]graph.EdgeKind) []*graph.Node {
	var out []*graph.Node
	for kind := range uniqueNonempty(kinds) {
		useKinds := slices.Collect(uniqueNonempty(allowed[kind]))
		prefix := keyStrings([]byte{familyNodeKind}, string(kind))
		decodeRows(s, prefix, func(raw []byte) (string, error) { return string(raw), nil }, func(id string) bool {
			n := 0
			if len(useKinds) == 0 {
				n = s.count("in total", id)
			} else {
				n = s.countEdgeKinds("in kind", id, useKinds)
			}
			if n == 0 {
				if candidate := s.GetNode(id); candidate != nil {
					out = append(out, candidate)
				}
			}
			return true
		})
	}
	return out
}

func (s *Snapshot) ExtractCandidates(kinds []graph.EdgeKind, minLines, minCallers, minFanOut int, pathPrefix string) []graph.ExtractCandidateRow {
	kinds = slices.Collect(uniqueNonempty(kinds))
	if len(kinds) == 0 {
		return nil
	}
	var out []graph.ExtractCandidateRow
	for _, id := range s.NodeIDsByKinds([]graph.NodeKind{graph.KindFunction, graph.KindMethod}) {
		n := s.lightNode(id)
		if n == nil || n.StartLine <= 0 || n.EndLine <= 0 || !graphpath.HasPrefix(n.FilePath, pathPrefix) {
			continue
		}
		lines := n.EndLine - n.StartLine + 1
		if lines < minLines {
			continue
		}
		callers := s.distinctNeighbors(id, graph.DirectionBackward, kinds)
		if callers < minCallers {
			continue
		}
		fan := s.distinctNeighbors(id, graph.DirectionForward, kinds)
		if fan < minFanOut {
			continue
		}
		out = append(out, graph.ExtractCandidateRow{NodeID: id, Name: n.Name, FilePath: n.FilePath, StartLine: n.StartLine, EndLine: n.EndLine, LineCount: lines, CallerCount: callers, FanOut: fan})
	}
	slices.SortFunc(out, func(a, b graph.ExtractCandidateRow) int { return cmp.Compare(a.NodeID, b.NodeID) })
	return out
}

func (s *Snapshot) distinctNeighbors(id string, dir graph.Direction, kinds []graph.EdgeKind) int {
	rows, err := s.ReadKindAdjacency(context.Background(), id, dir, kinds)
	if err != nil {
		s.retain(err)
		return 0
	}
	seen := make(map[string]struct{})
	for _, e := range rows.Edges {
		target := e.To
		if dir == graph.DirectionBackward {
			target = e.From
		}
		seen[target] = struct{}{}
	}
	return len(seen)
}

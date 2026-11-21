package store_pebble

import (
	"context"
	"slices"

	"github.com/zzet/gortex/internal/graph"
)

var (
	_ graph.ClassHierarchyTraverser  = (*Snapshot)(nil)
	_ graph.CommunityCrossingsByKind = (*Snapshot)(nil)
	_ graph.CrossRepoCandidates      = (*Snapshot)(nil)
	_ graph.CrossRepoEdgeAggregator  = (*Snapshot)(nil)
)

func (s *Snapshot) ClassHierarchyTraverse(seed, direction string, kinds []graph.EdgeKind, maxDepth int) []graph.ClassHierarchyRow {
	kinds = slices.Collect(uniqueNonempty(kinds))
	if maxDepth <= 0 || len(kinds) == 0 || seed == "" || (direction != "up" && direction != "down") || s.lightNode(seed) == nil {
		return nil
	}
	dir := graph.DirectionForward
	if direction == "down" {
		dir = graph.DirectionBackward
	}
	// The queue retains the path of each first discovery, including an
	// endpoint without a node payload. The caller only gets IDs and kinds.
	queue := []graph.ClassHierarchyRow{{Path: []string{seed}}}
	seen := map[string]bool{seed: true}
	var out []graph.ClassHierarchyRow
	for len(queue) > 0 {
		step := queue[0]
		queue = queue[1:]
		if len(step.EdgeKinds) >= maxDepth {
			continue
		}
		adj, err := s.ReadKindAdjacency(context.Background(), step.Path[len(step.Path)-1], dir, kinds)
		if err != nil {
			s.retain(err)
			return nil
		}
		for _, e := range adj.Edges {
			target := e.To
			if dir == graph.DirectionBackward {
				target = e.From
			}
			if target == "" || seen[target] {
				continue
			}
			seen[target] = true
			path := step.Path
			if len(step.EdgeKinds) == 0 {
				path = nil
			}
			next := graph.ClassHierarchyRow{Path: append(slices.Clone(path), target), EdgeKinds: append(slices.Clone(step.EdgeKinds), e.Kind)}
			out = append(out, next)
			queue = append(queue, next)
		}
	}
	return out
}

func (s *Snapshot) CommunityCrossingsByKind(kinds []graph.EdgeKind, comm map[string]string) map[string]int {
	if len(comm) == 0 {
		return nil
	}
	var out map[string]int
	for kind := range uniqueNonempty(kinds) {
		s.scanEdgeKind(kind, decodeEdgeLight, func(e *graph.Edge) bool {
			from, fromOK := comm[e.From]
			to, toOK := comm[e.To]
			if fromOK && toOK && from != to {
				if out == nil {
					out = make(map[string]int)
				}
				out[e.From]++
			}
			return true
		})
	}
	return out
}

func (s *Snapshot) CrossRepoCandidates(kinds []graph.EdgeKind) []graph.CrossRepoCandidateRow {
	var rows []graph.CrossRepoCandidateRow
	for kind := range uniqueNonempty(kinds) {
		s.scanEdgeKind(kind, decodeEdgeLight, func(e *graph.Edge) bool {
			from, to := s.lightNode(e.From), s.lightNode(e.To)
			if from != nil && to != nil && from.RepoPrefix != "" && to.RepoPrefix != "" && from.RepoPrefix != to.RepoPrefix {
				// Match the candidate projection; route-specific metadata is not part
				// of this legacy endpoint join.
				projected := &graph.Edge{From: e.From, To: e.To, Kind: e.Kind, FilePath: e.FilePath, Line: e.Line, Confidence: e.Confidence, ConfidenceLabel: e.ConfidenceLabel, Origin: e.Origin, Tier: e.Tier, CrossRepo: e.CrossRepo}
				rows = append(rows, graph.CrossRepoCandidateRow{Edge: projected, FromRepo: from.RepoPrefix, ToRepo: to.RepoPrefix})
			}
			return true
		})
	}
	return rows
}

func (s *Snapshot) CrossRepoEdgeCounts() []graph.CrossRepoEdgeRow {
	type scope struct {
		kind     graph.EdgeKind
		from, to string
	}
	counts := make(map[scope]int)
	for _, kind := range []graph.EdgeKind{graph.EdgeCrossRepoCalls, graph.EdgeCrossRepoImplements, graph.EdgeCrossRepoExtends, graph.EdgeCrossRepoMotivates} {
		s.scanEdgeKind(kind, decodeEdgeLight, func(e *graph.Edge) bool {
			from, to := s.lightNode(e.From), s.lightNode(e.To)
			if from != nil && to != nil && from.RepoPrefix != to.RepoPrefix {
				counts[scope{kind, from.RepoPrefix, to.RepoPrefix}]++
			}
			return true
		})
	}
	var rows []graph.CrossRepoEdgeRow
	for k, count := range counts {
		rows = append(rows, graph.CrossRepoEdgeRow{Kind: k.kind, FromRepo: k.from, ToRepo: k.to, Count: count})
	}
	return rows
}

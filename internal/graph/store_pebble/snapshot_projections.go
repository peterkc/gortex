package store_pebble

import (
	"iter"
	"slices"

	"github.com/zzet/gortex/internal/graph"
)

var (
	_ graph.LightEdgeScanner    = (*Snapshot)(nil)
	_ graph.EdgesByKindsScanner = (*Snapshot)(nil)
	_ graph.NodesByKindsScanner = (*Snapshot)(nil)
	_ graph.NodeIDsByKinds      = (*Snapshot)(nil)
	_ graph.EdgeKindCounter     = (*Snapshot)(nil)
	_ graph.InDegreeForNodes    = (*Snapshot)(nil)
)

func uniqueNonempty[K ~string](items []K) iter.Seq[K] {
	return func(yield func(K) bool) {
		seen := make(map[K]struct{}, len(items))
		for _, item := range items {
			if item == "" {
				continue
			}
			if _, exists := seen[item]; exists {
				continue
			}
			seen[item] = struct{}{}
			if !yield(item) {
				return
			}
		}
	}
}

func (s *Snapshot) AllEdgesLight(kinds ...graph.EdgeKind) []*graph.Edge {
	if len(kinds) == 0 {
		return listRows(s, []byte{familyEdge, edgeOut}, decodeEdgeLight)
	}
	var out []*graph.Edge
	for kind := range uniqueNonempty(kinds) {
		s.scanEdgeKind(kind, decodeEdgeLight, func(e *graph.Edge) bool {
			out = append(out, e)
			return true
		})
	}
	return out
}

func (s *Snapshot) EdgesByKinds(kinds []graph.EdgeKind) iter.Seq[*graph.Edge] {
	return func(yield func(*graph.Edge) bool) {
		for kind := range uniqueNonempty(kinds) {
			for e := range s.EdgesByKind(kind) {
				if !yield(e) {
					return
				}
			}
		}
	}
}

func (s *Snapshot) NodesByKinds(kinds []graph.NodeKind) []*graph.Node {
	var out []*graph.Node
	for kind := range uniqueNonempty(kinds) {
		out = slices.AppendSeq(out, s.NodesByKind(kind))
	}
	return out
}

func (s *Snapshot) NodeIDsByKinds(kinds []graph.NodeKind) []string {
	var out []string
	for kind := range uniqueNonempty(kinds) {
		decodeRows(s, keyStrings([]byte{familyNodeKind}, string(kind)), func(raw []byte) (string, error) { return string(raw), nil }, func(id string) bool {
			out = append(out, id)
			return true
		})
	}
	return out
}

func (s *Snapshot) EdgeKindCounts() map[graph.EdgeKind]int {
	counts := s.countLabels(countKey("edge kind"))
	out := make(map[graph.EdgeKind]int, len(counts))
	for kind, count := range counts {
		out[graph.EdgeKind(kind)] = count
	}
	return out
}

func (s *Snapshot) InDegreeForNodes(ids []string) map[string]int {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string]int)
	for id := range uniqueNonempty(ids) {
		if count := s.count("in total", id); count != 0 {
			out[id] = count
		}
	}
	return out
}

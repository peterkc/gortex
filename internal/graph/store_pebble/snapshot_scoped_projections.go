package store_pebble

import (
	"cmp"
	"iter"
	"slices"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
)

var (
	_ graph.EdgeAdjacencyForKinds    = (*Snapshot)(nil)
	_ graph.InEdgeCounter            = (*Snapshot)(nil)
	_ graph.NodeDegreeAggregator     = (*Snapshot)(nil)
	_ graph.NodeDegreeByKinds        = (*Snapshot)(nil)
	_ graph.NodeFanAggregator        = (*Snapshot)(nil)
	_ graph.NodesInFilesByKindFinder = (*Snapshot)(nil)
	_ graph.FileSymbolNamesByPaths   = (*Snapshot)(nil)
)

func (s *Snapshot) lightNode(id string) *graph.Node {
	n, err := point(s, keyStrings([]byte{familyNode}, id), decodeNodeLight)
	s.retain(err)
	return n
}

func (s *Snapshot) EdgeAdjacencyForKinds(edgeKinds []graph.EdgeKind, nodeKinds []graph.NodeKind) iter.Seq[[2]string] {
	return func(yield func([2]string) bool) {
		if err := s.requireEdgeGroups(edgeKinds...); err != nil {
			s.retain(err)
			return
		}
		if err := s.requireNodeGroups(nodeKinds...); err != nil {
			s.retain(err)
			return
		}
		if len(nodeKinds) == 0 {
			return
		}
		for kind := range uniqueNonempty(edgeKinds) {
			stopped := false
			s.scanEdgeKind(kind, decodeEdgeLight, func(e *graph.Edge) bool {
				from := s.lightNode(e.From)
				if from == nil || !slices.Contains(nodeKinds, from.Kind) {
					return true
				}
				to := s.lightNode(e.To)
				if to == nil || !slices.Contains(nodeKinds, to.Kind) {
					return true
				}
				stopped = !yield([2]string{e.From, e.To})
				return !stopped
			})
			if stopped {
				return
			}
		}
	}
}

func (s *Snapshot) InEdgeCountsByKind(kinds []graph.EdgeKind) map[string]int {
	if err := s.requireEdgeGroups(kinds...); err != nil {
		s.retain(err)
		return nil
	}
	var out map[string]int
	for kind := range uniqueNonempty(kinds) {
		if out == nil {
			out = make(map[string]int)
		}
		s.scanEdgeKind(kind, decodeEdgeLight, func(e *graph.Edge) bool {
			out[e.To]++
			return true
		})
	}
	return out
}

func (s *Snapshot) nodeDegree(id string) graph.NodeDegreeRow {
	return graph.NodeDegreeRow{NodeID: id, InCount: s.count("in total", id), OutCount: s.count("out total", id)}
}

func (s *Snapshot) countEdgeKinds(dir, id string, kinds []graph.EdgeKind) int {
	n := 0
	for _, kind := range kinds {
		n += s.count(dir, id, string(kind))
	}
	return n
}

func (s *Snapshot) NodeDegreeCounts(ids []string, usage []graph.EdgeKind) []graph.NodeDegreeRow {
	usage = slices.Collect(uniqueNonempty(usage))
	var out []graph.NodeDegreeRow
	for id := range uniqueNonempty(ids) {
		if s.lightNode(id) == nil {
			continue
		}
		row := s.nodeDegree(id)
		row.UsageInCount = s.countEdgeKinds("in kind", id, usage)
		out = append(out, row)
	}
	return out
}

func (s *Snapshot) NodeFanCounts(ids []string, in, out []graph.EdgeKind) []graph.NodeFanRow {
	in = slices.Collect(uniqueNonempty(in))
	out = slices.Collect(uniqueNonempty(out))
	var rows []graph.NodeFanRow
	for id := range uniqueNonempty(ids) {
		if s.lightNode(id) != nil {
			rows = append(rows, graph.NodeFanRow{NodeID: id, FanIn: s.countEdgeKinds("in kind", id, in), FanOut: s.countEdgeKinds("out kind", id, out)})
		}
	}
	return rows
}

func (s *Snapshot) NodeDegreeByKinds(kinds []graph.NodeKind, pathPrefix string) []graph.NodeDegreeRow {
	var rows []graph.NodeDegreeRow
	for _, id := range s.NodeIDsByKinds(kinds) {
		n := s.lightNode(id)
		if n != nil && (pathPrefix == "" || graphpath.HasPrefix(n.FilePath, pathPrefix)) {
			rows = append(rows, s.nodeDegree(id))
		}
	}
	slices.SortFunc(rows, func(a, b graph.NodeDegreeRow) int { return cmp.Compare(a.NodeID, b.NodeID) })
	return rows
}

func (s *Snapshot) NodesInFilesByKind(paths []string, kinds []graph.NodeKind) []*graph.Node {
	if err := s.requireNodeGroups(kinds...); err != nil {
		s.retain(err)
		return nil
	}
	if len(kinds) == 0 {
		return nil
	}
	var out []*graph.Node
	for path := range uniqueNonempty(paths) {
		for _, n := range s.GetFileNodes(path) {
			if slices.Contains(kinds, n.Kind) {
				out = append(out, n)
			}
		}
	}
	return out
}

func (s *Snapshot) FileSymbolNamesByPaths(paths []string, kinds []graph.NodeKind) []graph.FileSymbolNameRow {
	type nameRow = graph.FileSymbolNameRow
	var rows []nameRow
	seen := make(map[nameRow]struct{})
	for _, n := range s.NodesInFilesByKind(paths, kinds) {
		r := nameRow{FilePath: n.FilePath, Name: n.Name}
		if _, ok := seen[r]; !ok {
			seen[r] = struct{}{}
			rows = append(rows, r)
		}
	}
	slices.SortFunc(rows, func(a, b nameRow) int {
		if path := cmp.Compare(a.FilePath, b.FilePath); path != 0 {
			return path
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return rows
}

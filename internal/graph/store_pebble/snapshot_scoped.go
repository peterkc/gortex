package store_pebble

import (
	"context"
	"iter"
	"slices"

	"github.com/zzet/gortex/internal/graph"
)

var (
	_ graph.ScopedOutlineAggregator         = (*Snapshot)(nil)
	_ graph.FilteredScopedOutlineAggregator = (*Snapshot)(nil)
	_ graph.ScopedDeadCodeReader            = (*Snapshot)(nil)
	_ graph.FilteredScopedDeadCodeReader    = (*Snapshot)(nil)
	_ graph.FileEditingContext              = (*Snapshot)(nil)
)

func (s *Snapshot) FileEditingContext(path string, kinds []graph.NodeKind) *graph.FileEditingContextResult {
	if err := s.requireNodeGroups(kinds...); err != nil {
		s.retain(err)
		return nil
	}
	return graph.FileEditingContextOf(s, path, kinds)
}

func (s *Snapshot) ScopedOutlineCounts(scope graph.NodeScope) (graph.OutlineCounts, error) {
	return s.ScopedOutlineCountsFiltered(scope, graph.OutlineEdgeFilter{})
}

func (s *Snapshot) ScopedOutlineCountsFiltered(scope graph.NodeScope, filter graph.OutlineEdgeFilter) (graph.OutlineCounts, error) {
	out := graph.OutlineCounts{ByLanguage: make(map[string]int)}
	decodeRows(s, []byte{familyNode}, decodeNodeLight, func(n *graph.Node) bool {
		if scope.Allows(n) {
			out.TotalNodes++
			if n.Language != "" {
				out.ByLanguage[n.Language]++
			}
		}
		return true
	})
	imports := make(map[string]int)
	decode := decodeEdgeLight
	if filter.ExcludeVia != "" {
		decode = decodeEdge
	}
	decodeRows(s, []byte{familyEdge, edgeOut}, decode, func(e *graph.Edge) bool {
		from, to := s.lightNode(e.From), s.lightNode(e.To)
		if to == nil || !scope.Allows(to) || !filter.Allows(e, from, to) {
			return true
		}
		if from != nil && scope.Allows(from) {
			out.TotalEdges++
		}
		if e.Kind == graph.EdgeImports {
			path := to.FilePath
			if path == "" {
				path = to.ID
			}
			imports[path]++
		}
		return true
	})
	if err := s.ReadError(); err != nil {
		return graph.OutlineCounts{}, err
	}
	for path, count := range imports {
		out.FileImports = append(out.FileImports, graph.FileImportCountRow{FilePath: path, Count: count})
	}
	return out, nil
}

func (s *Snapshot) NodesByKindInRepos(kind graph.NodeKind, repos []string) iter.Seq[*graph.Node] {
	if repos == nil {
		return s.NodesByKind(kind)
	}
	return func(yield func(*graph.Node) bool) {
		if err := s.requireNodeGroups(kind); err != nil {
			s.retain(err)
			return
		}
		seen := make(map[string]bool, len(repos))
		for _, repo := range repos {
			if seen[repo] {
				continue
			}
			seen[repo] = true
			stopped := false
			s.indexedNodes(keyStrings([]byte{familyRepo}, repo), func(n *graph.Node) bool {
				stopped = n.Kind == kind && !yield(n)
				return !stopped
			})
			if stopped {
				return
			}
		}
	}
}

func (s *Snapshot) DeadCodeCandidatesInRepos(kinds []graph.NodeKind, incoming map[graph.NodeKind][]graph.EdgeKind, repos []string) []*graph.Node {
	out, _ := s.DeadCodeCandidatesInReposFiltered(kinds, incoming, repos, graph.OutlineEdgeFilter{})
	return out
}

func (s *Snapshot) DeadCodeCandidatesInReposFiltered(kinds []graph.NodeKind, incoming map[graph.NodeKind][]graph.EdgeKind, repos []string, filter graph.OutlineEdgeFilter) ([]*graph.Node, error) {
	var out []*graph.Node
	for kind := range uniqueNonempty(kinds) {
		useKinds := slices.Collect(uniqueNonempty(incoming[kind]))
		if err := s.requireDeadCodeGroups(kind, useKinds); err != nil {
			s.retain(err)
			return nil, err
		}
		for n := range s.NodesByKindInRepos(kind, repos) {
			used := false
			prefix := keyStrings([]byte{familyEdge, edgeIn}, n.ID)
			err := scan(context.Background(), s, prefix, func(_, raw []byte) (bool, error) {
				e, err := decodeEdge(raw)
				if err != nil {
					return false, err
				}
				if len(useKinds) != 0 && !slices.Contains(useKinds, e.Kind) {
					return true, nil
				}
				used = filter.Allows(e, s.lightNode(e.From), n)
				return !used, nil
			})
			if err != nil {
				s.retain(err)
				return nil, err
			}
			if !used {
				out = append(out, n)
			}
		}
	}
	return out, s.ReadError()
}

func (s *Snapshot) IfaceImplementsRowsInRepos(repos []string) []graph.IfaceImplementsRow {
	out, _ := s.IfaceImplementsRowsInReposFiltered(repos, graph.OutlineEdgeFilter{})
	return out
}

func (s *Snapshot) IfaceImplementsRowsInReposFiltered(repos []string, filter graph.OutlineEdgeFilter) ([]graph.IfaceImplementsRow, error) {
	if repos != nil && len(repos) == 0 {
		return nil, nil
	}
	var out []graph.IfaceImplementsRow
	for e := range s.EdgesByKind(graph.EdgeImplements) {
		from := s.lightNode(e.From)
		if repos != nil && (from == nil || !slices.Contains(repos, from.RepoPrefix)) {
			continue
		}
		iface := s.GetNode(e.To)
		if iface == nil || iface.Kind != graph.KindInterface || !filter.Allows(e, from, iface) {
			continue
		}
		if _, ok := iface.Meta["methods"]; ok {
			out = append(out, graph.IfaceImplementsRow{TypeID: e.From, IfaceID: iface.ID, IfaceMeta: iface.Meta})
		}
	}
	return out, s.ReadError()
}

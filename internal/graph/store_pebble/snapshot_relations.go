package store_pebble

import (
	"cmp"
	"slices"

	"github.com/zzet/gortex/internal/graph"
)

var (
	_ graph.IfaceImplementsScanner  = (*Snapshot)(nil)
	_ graph.MemberMethodsByType     = (*Snapshot)(nil)
	_ graph.StructuralParentEdges   = (*Snapshot)(nil)
	_ graph.FileImporters           = (*Snapshot)(nil)
	_ graph.FileImportAggregator    = (*Snapshot)(nil)
	_ graph.ReachableForwardByKinds = (*Snapshot)(nil)
)

func (s *Snapshot) IfaceImplementsRows() []graph.IfaceImplementsRow {
	var rows []graph.IfaceImplementsRow
	s.scanEdgeKind(graph.EdgeImplements, decodeEdgeLight, func(e *graph.Edge) bool {
		iface := s.GetNode(e.To)
		if iface != nil && iface.Kind == graph.KindInterface {
			if _, ok := iface.Meta["methods"]; ok {
				rows = append(rows, graph.IfaceImplementsRow{TypeID: e.From, IfaceID: iface.ID, IfaceMeta: iface.Meta})
			}
		}
		return true
	})
	return rows
}

func (s *Snapshot) MemberMethodsByType() map[string][]graph.MemberMethodInfo {
	var rows map[string][]graph.MemberMethodInfo
	seen := make(map[[2]string]struct{})
	s.scanEdgeKind(graph.EdgeMemberOf, decodeEdgeLight, func(e *graph.Edge) bool {
		pair := [2]string{e.From, e.To}
		if _, ok := seen[pair]; ok {
			return true
		}
		seen[pair] = struct{}{}
		n := s.lightNode(e.From)
		if n != nil && n.Kind == graph.KindMethod {
			if rows == nil {
				rows = make(map[string][]graph.MemberMethodInfo)
			}
			rows[e.To] = append(rows[e.To], graph.MemberMethodInfo{MethodID: n.ID, Name: n.Name, FilePath: n.FilePath, StartLine: n.StartLine, RepoPrefix: n.RepoPrefix})
		}
		return true
	})
	return rows
}

func (s *Snapshot) StructuralParentEdges() []graph.StructuralParentEdgeRow {
	var rows []graph.StructuralParentEdgeRow
	isType := func(n *graph.Node) bool {
		return n != nil && (n.Kind == graph.KindType || n.Kind == graph.KindInterface)
	}
	for _, kind := range []graph.EdgeKind{graph.EdgeExtends, graph.EdgeImplements, graph.EdgeComposes} {
		s.scanEdgeKind(kind, decodeEdgeLight, func(e *graph.Edge) bool {
			from, to := s.lightNode(e.From), s.lightNode(e.To)
			if isType(from) && isType(to) {
				rows = append(rows, graph.StructuralParentEdgeRow{FromID: e.From, ToID: e.To, FromKind: from.Kind, ToKind: to.Kind, Origin: e.Origin})
			}
			return true
		})
	}
	return rows
}

func (s *Snapshot) FileImporters(path string) []graph.FileImporterRow {
	if path == "" {
		return nil
	}
	var rows []graph.FileImporterRow
	targets := []string{path}
	for _, n := range s.GetFileNodes(path) {
		targets = append(targets, n.ID)
	}
	for id := range uniqueNonempty(targets) {
		if s.lightNode(id) == nil {
			continue
		}
		prefix := keyStrings([]byte{familyEdge, edgeIn}, id, string(graph.EdgeImports))
		decodeRows(s, prefix, decodeEdgeLight, func(e *graph.Edge) bool {
			if from := s.lightNode(e.From); from != nil {
				rows = append(rows, graph.FileImporterRow{FromFile: from.FilePath, FromID: from.ID, FromName: from.Name, FromKind: from.Kind})
			}
			return true
		})
	}
	slices.SortStableFunc(rows, func(a, b graph.FileImporterRow) int { return cmp.Compare(a.FromFile, b.FromFile) })
	return rows
}

func (s *Snapshot) FileImportCounts(scope []string) []graph.FileImportCountRow {
	counts := make(map[string]int)
	add := func(id string, count int) {
		n := s.lightNode(id)
		if n == nil || count == 0 {
			return
		}
		path := n.FilePath
		if path == "" {
			path = n.ID
		}
		counts[path] += count
	}
	if scope == nil {
		s.scanEdgeKind(graph.EdgeImports, decodeEdgeLight, func(e *graph.Edge) bool { add(e.To, 1); return true })
	} else {
		for id := range uniqueNonempty(scope) {
			add(id, s.count("in kind", id, string(graph.EdgeImports)))
		}
	}
	var rows []graph.FileImportCountRow
	for path, count := range counts {
		rows = append(rows, graph.FileImportCountRow{FilePath: path, Count: count})
	}
	slices.SortFunc(rows, func(a, b graph.FileImportCountRow) int { return cmp.Compare(a.FilePath, b.FilePath) })
	return rows
}

func (s *Snapshot) ReachableForwardByKinds(seeds []string, kinds []graph.EdgeKind) map[string]bool {
	if err := s.requireEdgeGroups(kinds...); err != nil {
		s.retain(err)
		return nil
	}
	if len(seeds) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	kinds = slices.Collect(uniqueNonempty(kinds))
	frontier := slices.Collect(uniqueNonempty(seeds))
	for _, seed := range frontier {
		seen[seed] = true
	}
	if len(kinds) == 0 {
		return seen
	}
	for len(frontier) != 0 {
		var next []string
		for _, id := range frontier {
			for _, kind := range kinds {
				prefix := keyStrings([]byte{familyEdge, edgeOutKind}, id, string(kind))
				decodeRows(s, prefix, decodeEdgeLight, func(e *graph.Edge) bool {
					if !seen[e.To] {
						seen[e.To] = true
						next = append(next, e.To)
					}
					return true
				})
			}
		}
		frontier = next
	}
	return seen
}

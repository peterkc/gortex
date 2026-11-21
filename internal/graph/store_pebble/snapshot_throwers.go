package store_pebble

import (
	"slices"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
)

var _ graph.ThrowerErrorSurfacer = (*Snapshot)(nil)

func (s *Snapshot) ThrowerErrorSurface(pathPrefix string) []graph.ThrowerErrorRow {
	type summary struct {
		row     graph.ThrowerErrorRow
		targets map[string]bool
	}
	byID := make(map[string]*summary)
	var order []*summary
	// The kind index orders each source by line, then canonical identity.
	s.scanEdgeKind(graph.EdgeThrows, decodeEdgeLight, func(e *graph.Edge) bool {
		if !graphpath.HasPrefix(e.FilePath, pathPrefix) {
			return true
		}
		found := byID[e.From]
		if found == nil {
			path, line := e.FilePath, e.Line
			if path == "" || line == 0 {
				if n := s.lightNode(e.From); n != nil {
					if path == "" {
						path = n.FilePath
					}
					if line == 0 {
						line = n.StartLine
					}
				}
			}
			found = &summary{row: graph.ThrowerErrorRow{ThrowerID: e.From, FilePath: path, Line: line}, targets: make(map[string]bool)}
			byID[e.From] = found
			order = append(order, found)
		}
		found.row.Throws++
		if !found.targets[e.To] {
			found.targets[e.To] = true
			found.row.ErrorTargets = append(found.row.ErrorTargets, e.To)
		}
		return true
	})
	var rows []graph.ThrowerErrorRow
	for _, found := range order {
		row := found.row
		seen := make(map[string]bool)
		prefix := keyStrings([]byte{familyEdge, edgeOutKind}, row.ThrowerID, string(graph.EdgeEmits))
		decodeRows(s, prefix, decodeEdgeLight, func(e *graph.Edge) bool {
			if n := s.GetNode(e.To); n != nil && n.Kind == graph.KindString && n.Meta["context"] == "error_msg" && !seen[n.Name] {
				seen[n.Name] = true
				row.ErrorMsgs = append(row.ErrorMsgs, n.Name)
			}
			return true
		})
		slices.Sort(row.ErrorTargets)
		slices.Sort(row.ErrorMsgs)
		rows = append(rows, row)
	}
	return rows
}

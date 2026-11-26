package store_pebble

import (
	"cmp"
	"context"
	"iter"
	"slices"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

var (
	_ graph.FileSubGraphReader            = (*Snapshot)(nil)
	_ graph.FileSubGraphCountReader       = (*Snapshot)(nil)
	_ graph.RepoLightNodeReader           = (*Snapshot)(nil)
	_ graph.ScopedNodeProjectionSequencer = (*Snapshot)(nil)
	_ graph.ScopedKindSummarySequencer    = (*Snapshot)(nil)
	_ graph.ScopedFileLocationSequencer   = (*Snapshot)(nil)
)

func (s *Snapshot) GetFileSubGraph(path string) ([]*graph.Node, []*graph.Edge) {
	var edges []*graph.Edge
	nodes, _ := s.fileSubGraph(path, func(e *graph.Edge) { edges = append(edges, e) })
	if nodes == nil {
		return nil, nil
	}
	return nodes, edges
}

func (s *Snapshot) GetFileSubGraphCounts(path string) ([]*graph.Node, int) {
	return s.fileSubGraph(path, nil)
}

func (s *Snapshot) fileSubGraph(path string, appendEdge func(*graph.Edge)) ([]*graph.Node, int) {
	if path == "" {
		return nil, 0
	}
	nodes := s.GetFileNodes(path)
	slices.SortFunc(nodes, func(a, b *graph.Node) int { return cmp.Compare(a.ID, b.ID) })
	seen := make(map[graph.TypedEdgeEndpoint]struct{})
	for _, n := range nodes {
		for _, dir := range []byte{edgeOut, edgeIn} {
			prefix := keyStrings([]byte{familyEdge, dir}, n.ID)
			err := scan(context.Background(), s, prefix, func(_, raw []byte) (bool, error) {
				e, err := decodeEdgeLight(raw)
				if err != nil {
					return false, err
				}
				endpoint := graph.TypedEdgeEndpoint{From: e.From, To: e.To, Kind: e.Kind}
				if _, ok := seen[endpoint]; ok {
					return true, nil
				}
				seen[endpoint] = struct{}{}
				if appendEdge != nil {
					e, err = decodeEdge(raw)
					if err != nil {
						return false, err
					}
					appendEdge(e)
				}
				return true, nil
			})
			if err != nil {
				s.retain(err)
				return nil, 0
			}
		}
	}
	return nodes, len(seen)
}

func (s *Snapshot) RepoNodesLight(repos []string) []*graph.Node {
	var out []*graph.Node
	for n := range s.NodesLightInScopeSeq(repos, nil) {
		out = append(out, &graph.Node{ID: n.ID, Kind: n.Kind, Name: n.Name, FilePath: n.FilePath, Language: n.Language, RepoPrefix: n.RepoPrefix})
	}
	return out
}

func (s *Snapshot) NodesInScopeSeq(repos, files []string, kinds ...graph.NodeKind) iter.Seq[*graph.Node] {
	return s.nodesInScope(repos, files, kinds, decodeNode)
}

func (s *Snapshot) NodesLightInScopeSeq(repos, files []string) iter.Seq[*graph.Node] {
	return s.nodesInScope(repos, files, nil, decodeNodeSummary)
}

func (s *Snapshot) NodesLightByKindsInScopeSeq(repos, files []string, kinds ...graph.NodeKind) iter.Seq[*graph.Node] {
	return s.nodesInScope(repos, files, kinds, decodeNodeSummary)
}

func (s *Snapshot) FileLocationsInScopeSeq(repos []string) iter.Seq[*graph.Node] {
	return func(yield func(*graph.Node) bool) {
		for n := range s.NodesLightByKindsInScopeSeq(repos, nil, graph.KindFile) {
			if !yield(&graph.Node{ID: n.ID, Kind: n.Kind, FilePath: n.FilePath, Language: n.Language, RepoPrefix: n.RepoPrefix, WorkspaceID: n.WorkspaceID}) {
				return
			}
		}
	}
}

func decodeNodeSummary(raw []byte) (*graph.Node, error) {
	n, err := decodeNodeLight(raw)
	if err != nil {
		return nil, err
	}
	return nodeSummary(n), nil
}

func nodeSummary(n *graph.Node) *graph.Node {
	return &graph.Node{
		ID: n.ID, Kind: n.Kind, Name: n.Name, QualName: n.QualName, FilePath: n.FilePath,
		StartLine: n.StartLine, EndLine: n.EndLine, StartColumn: n.StartColumn, EndColumn: n.EndColumn,
		Language: n.Language, RepoPrefix: n.RepoPrefix, WorkspaceID: n.WorkspaceID, ProjectID: n.ProjectID,
	}
}

// Keep one ID cursor per selected index prefix. Merge heads by ID before
// fetching rows, so scope does not materialize a whole graph or filter an
// already capped response. Cursor lifetime is owned by this iteration.
func (s *Snapshot) nodesInScope(repos, files []string, kinds []graph.NodeKind, decode decodeRow[*graph.Node]) iter.Seq[*graph.Node] {
	return func(yield func(*graph.Node) bool) {
		if err := s.requireNodeGroups(kinds...); err != nil {
			s.retain(err)
			return
		}
		if len(repos) == 0 && len(files) == 0 {
			return
		}
		selected := slices.Clone(repos)
		if len(selected) == 0 {
			selected = s.repos()
		}
		slices.Sort(selected)
		selected = slices.Compact(selected)
		var prefixes [][]byte
		for _, repo := range selected {
			if len(files) == 0 {
				prefixes = append(prefixes, keyStrings([]byte{familyRepo}, repo))
				continue
			}
			for path := range uniqueNonempty(files) {
				prefixes = append(prefixes, keyStrings([]byte{familyFile}, repo, path))
			}
		}
		stream := func(kind graph.NodeKind) bool {
			return s.mergeScopedNodes(prefixes, kind, decode, yield)
		}
		if len(kinds) == 0 {
			stream("")
			return
		}
		for kind := range uniqueNonempty(kinds) {
			if !stream(kind) {
				return
			}
		}
	}
}

func (s *Snapshot) mergeScopedNodes(prefixes [][]byte, kind graph.NodeKind, decode decodeRow[*graph.Node], yield func(*graph.Node) bool) (more bool) {
	defer func() { more = more && s.ReadError() == nil }()
	type cursor struct {
		id   string
		next func() (string, bool)
	}
	cursors := make([]cursor, 0, len(prefixes))
	for _, prefix := range prefixes {
		ids := iter.Seq[string](func(yield func(string) bool) {
			s.retain(scan(context.Background(), s, prefix, func(_, raw []byte) (bool, error) { return yield(string(raw)), nil }))
		})
		next, stop := iter.Pull(ids)
		defer stop()
		if id, ok := next(); ok {
			cursors = append(cursors, cursor{id, next})
		}
	}
	if len(cursors) == 0 {
		return true
	}
	// Seek the global kind posting to the current scope ID rather than
	// stepping through unrelated repositories before fetching node payloads.
	var kindID string
	var kindOK bool
	var kindCursor snapshotIterator
	if kind != "" {
		prefix := keyStrings([]byte{familyNodeKind}, string(kind))
		var err error
		kindCursor, err = s.snap.NewIterWithContext(context.Background(), &pebble.IterOptions{LowerBound: prefix, UpperBound: prefixEnd(prefix)})
		if err != nil {
			s.retain(err)
			return false
		}
		defer func() { s.retain(kindCursor.Close()) }()
	}
	for len(cursors) > 0 {
		i := 0
		for j := 1; j < len(cursors); j++ {
			if cursors[j].id < cursors[i].id {
				i = j
			}
		}
		id := cursors[i].id
		if kindCursor != nil && (!kindOK || kindID < id) {
			if !kindCursor.SeekGE(keyStrings([]byte{familyNodeKind}, string(kind), id)) {
				s.retain(kindCursor.Error())
				break
			}
			raw, err := kindCursor.ValueAndErr()
			if err != nil {
				s.retain(err)
				return false
			}
			kindID, kindOK = string(raw), true
		}
		if kindCursor == nil || kindID == id {
			n, err := point(s, keyStrings([]byte{familyNode}, id), decode)
			if err == nil && n == nil {
				err = invalid("missing scoped node")
			}
			if err != nil {
				s.retain(err)
				return false
			}
			if (kind == "" || kind == n.Kind) && !yield(n) {
				return false
			}
		}
		if next, ok := cursors[i].next(); ok {
			cursors[i].id = next
		} else {
			cursors[i] = cursors[len(cursors)-1]
			cursors = cursors[:len(cursors)-1]
		}
	}
	return true
}

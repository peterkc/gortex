package store_pebble

import (
	"context"
	"iter"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

var (
	_ graph.CallAncestorReader               = (*Snapshot)(nil)
	_ graph.FilteredCallAncestorReader       = (*Snapshot)(nil)
	_ graph.NodeScopedDeadCodeReader         = (*Snapshot)(nil)
	_ graph.FilteredNodeScopedDeadCodeReader = (*Snapshot)(nil)
	_ graph.FilteredBFSAdmissionReader       = (*Snapshot)(nil)
)

func (s *Snapshot) CallAncestorsContext(ctx context.Context, candidates []string) ([]graph.CallAncestorRow, error) {
	return s.CallAncestorsFilteredContext(ctx, candidates, graph.OutlineEdgeFilter{})
}

func (s *Snapshot) CallAncestorsFilteredContext(ctx context.Context, candidates []string, filter graph.OutlineEdgeFilter) ([]graph.CallAncestorRow, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	queue := slices.Collect(uniqueNonempty(candidates))
	seen := make(map[string]bool, len(queue))
	for _, id := range queue {
		seen[id] = true
	}
	var out []graph.CallAncestorRow
	for i := 0; i < len(queue); i++ {
		id := queue[i]
		var to *graph.Node
		if len(filter.ExcludeNodeKinds) > 0 {
			var err error
			to, err = point(s, keyStrings([]byte{familyNode}, id), decodeNodeLight)
			if err != nil {
				return nil, err
			}
		}
		err := s.visitKindEdges(ctx, id, graph.DirectionBackward, []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences}, func(e *graph.Edge) (bool, error) {
			from, err := point(s, keyStrings([]byte{familyNode}, e.From), decodeNodeLight)
			if err != nil {
				return false, err
			}
			if !filter.Allows(e, from, to) {
				return true, nil
			}
			row := graph.CallAncestorRow{From: e.From, To: e.To}
			if from != nil {
				row.FromKind, row.FromFile = from.Kind, from.FilePath
			}
			out = append(out, row)
			if e.From != "" && !seen[e.From] {
				seen[e.From] = true
				queue = append(queue, e.From)
			}
			return true, nil
		})
		if err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Snapshot) NodesByKindInScope(kind graph.NodeKind, repos []string, scope graph.NodeScope) iter.Seq[*graph.Node] {
	return func(yield func(*graph.Node) bool) {
		if repos == nil {
			for n := range s.NodesByKind(kind) {
				if scope.Allows(n) && !yield(n) {
					return
				}
			}
			return
		}
		ordered := slices.Clone(repos)
		slices.Sort(ordered)
		for _, repo := range slices.Compact(ordered) {
			for n := range s.NodesInScopeSeq([]string{repo}, nil, kind) {
				if scope.Allows(n) && !yield(n) {
					return
				}
			}
		}
	}
}

func (s *Snapshot) DeadCodeCandidatesInScope(kinds []graph.NodeKind, incoming map[graph.NodeKind][]graph.EdgeKind, repos []string, scope graph.NodeScope, exclude graph.DeadCodeCandidateExclusions) []*graph.Node {
	nodes, err := s.DeadCodeCandidatesInScopeFiltered(kinds, incoming, repos, scope, graph.OutlineEdgeFilter{}, exclude)
	s.retain(err)
	return nodes
}

func (s *Snapshot) DeadCodeCandidatesInScopeFiltered(kinds []graph.NodeKind, incoming map[graph.NodeKind][]graph.EdgeKind, repos []string, scope graph.NodeScope, filter graph.OutlineEdgeFilter, exclude graph.DeadCodeCandidateExclusions) ([]*graph.Node, error) {
	var out []*graph.Node
	ctx := context.Background()
	for _, kind := range kinds {
		start := len(out)
		allow := slices.Collect(uniqueNonempty(incoming[kind]))
		for n := range s.NodesByKindInScope(kind, repos, scope) {
			if !exclude.Allows(n) {
				continue
			}
			used := false
			inspect := func(e *graph.Edge) (bool, error) {
				var from *graph.Node
				if len(filter.ExcludeNodeKinds) > 0 {
					var err error
					from, err = point(s, keyStrings([]byte{familyNode}, e.From), decodeNodeLight)
					if err != nil {
						return false, err
					}
				}
				used = filter.Allows(e, from, n)
				return !used, nil
			}
			var err error
			if len(allow) > 0 {
				err = s.visitKindEdges(ctx, n.ID, graph.DirectionBackward, allow, inspect)
			} else {
				err = scan(ctx, s, keyStrings([]byte{familyEdge, edgeIn}, n.ID), func(_, raw []byte) (bool, error) {
					e, err := decodeEdge(raw)
					if err != nil {
						return false, err
					}
					return inspect(e)
				})
			}
			if err != nil {
				return nil, err
			}
			if !used {
				out = append(out, n)
			}
		}
		slices.SortFunc(out[start:], func(a, b *graph.Node) int { return strings.Compare(a.ID, b.ID) })
	}
	if err := s.ReadError(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Snapshot) BFSAdmissionFilteredContext(ctx context.Context, req graph.BFSAdmissionRequest, filter graph.OutlineEdgeFilter) (graph.BFSAdmissionSnapshot, bool, error) {
	var zero graph.BFSAdmissionSnapshot
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return zero, false, err
	}
	stateBudget, edgeBudget, supported := graph.BFSAdmissionBudgets(req)
	if !supported || (req.Direction != graph.DirectionForward && req.Direction != graph.DirectionBackward) {
		return zero, false, nil
	}
	kinds := slices.Collect(uniqueNonempty(req.Kinds))
	if len(kinds) == 0 {
		return zero, false, nil
	}
	backward := req.Direction == graph.DirectionBackward
	index := edgeOut
	neighbor := func(e *graph.Edge) string { return e.To }
	if backward {
		index = edgeIn
		neighbor = func(e *graph.Edge) string { return e.From }
	}
	boundary := func(id string) bool {
		return strings.HasPrefix(id, "unresolved::") || strings.Contains(id, "::unresolved::") || strings.HasPrefix(id, "external::")
	}
	seed, err := point(s, keyStrings([]byte{familyNode}, req.Seed), decodeNodeLight)
	if err != nil {
		return zero, false, err
	}
	out := graph.BFSAdmissionSnapshot{Nodes: make(map[string]*graph.Node), Adjacency: make(map[string]graph.KindAdjacency)}
	if seed != nil && slices.Contains(filter.ExcludeNodeKinds, seed.Kind) {
		return out, true, nil
	}
	headers := map[string]*graph.Node{req.Seed: seed}
	header := func(id string) (*graph.Node, error) {
		if n, ok := headers[id]; ok {
			return n, nil
		}
		n, err := point(s, keyStrings([]byte{familyNode}, id), decodeNodeLight)
		if err == nil {
			headers[id] = n
		}
		return n, err
	}
	type state struct {
		id    string
		depth int
	}
	work := []state{{req.Seed, 0}}
	states := map[state]bool{work[0]: true}
	seen := map[string]bool{req.Seed: true}
	walked := make([]string, 0)
	visited := 0
	for i := 0; i < len(work); i++ {
		here := work[i]
		if here.depth == req.Depth {
			continue
		}
		if _, ok := out.Adjacency[here.id]; !ok {
			walked = append(walked, here.id)
			out.Adjacency[here.id] = graph.KindAdjacency{}
		}
		anchor := headers[here.id]
		err := s.visitKindEdges(ctx, here.id, req.Direction, kinds, func(e *graph.Edge) (bool, error) {
			visited++
			if visited > edgeBudget {
				return false, nil
			}
			id := neighbor(e)
			if id == req.Seed || boundary(id) {
				return true, nil
			}
			n, err := header(id)
			if err != nil {
				return false, err
			}
			if n == nil {
				return true, nil
			}
			from, to := anchor, n
			if backward {
				from, to = n, anchor
			}
			if !filter.Allows(e, from, to) {
				return true, nil
			}
			if len(req.Scope.RepoAllow) > 0 {
				if n.RepoPrefix != "" && !req.Scope.RepoAllow[n.RepoPrefix] {
					return true, nil
				}
			} else if req.Scope.WorkspaceID != "" && (seed == nil || n.RepoPrefix != seed.RepoPrefix) && !req.Scope.Allows(n) {
				return true, nil
			}
			next := state{id, here.depth + 1}
			if !states[next] {
				work = append(work, next)
				states[next] = true
				seen[id] = true
			}
			return len(work) <= stateBudget, nil
		})
		if err != nil {
			return zero, false, err
		}
		if len(work) > stateBudget || visited > edgeBudget {
			return zero, false, nil
		}
	}
	clear(out.Adjacency)
	totalEdges := 0
	room := req.Limit - len(seen)
	for _, id := range walked {
		anchor := headers[id]
		rows := graph.KindAdjacency{}
		var err error
		if backward {
			rows.Total, err = point(s, countKey("in total", id), decodeCount)
		} else {
			rows.Total, err = point(s, countKey("out total", id), decodeCount)
		}
		if err != nil {
			return zero, false, err
		}
		rows.Total = min(rows.Total, req.Limit)
		if rows.Total >= room {
			rows.Total = 0
			err = scan(ctx, s, keyStrings([]byte{familyEdge, index}, id), func(_, raw []byte) (bool, error) {
				e, err := decodeEdge(raw)
				if err != nil {
					return false, err
				}
				n, err := header(neighbor(e))
				if err != nil {
					return false, err
				}
				from, to := anchor, n
				if backward {
					from, to = n, anchor
				}
				if filter.Allows(e, from, to) {
					rows.Total++
				}
				return rows.Total < req.Limit, nil
			})
			if err != nil {
				return zero, false, err
			}
		}
		err = s.visitKindEdges(ctx, id, req.Direction, kinds, func(e *graph.Edge) (bool, error) {
			far := neighbor(e)
			n, err := header(far)
			if err != nil {
				return false, err
			}
			from, to := anchor, n
			if backward {
				from, to = n, anchor
			}
			if !filter.Allows(e, from, to) || (!seen[far] && !boundary(far) && n != nil) {
				return true, nil
			}
			totalEdges++
			if totalEdges > edgeBudget {
				return false, nil
			}
			rows.Edges = append(rows.Edges, e)
			if seen[far] && far != req.Seed && out.Nodes[far] == nil {
				full, err := s.GetNodeContext(ctx, far)
				if err != nil {
					return false, err
				}
				out.Nodes[far] = full
			}
			return true, nil
		})
		if err != nil {
			return zero, false, err
		}
		if totalEdges > edgeBudget {
			return zero, false, nil
		}
		if len(rows.Edges) > 0 {
			out.Adjacency[id] = rows
		}
	}
	if err := ctx.Err(); err != nil {
		return zero, false, err
	}
	return out, true, nil
}

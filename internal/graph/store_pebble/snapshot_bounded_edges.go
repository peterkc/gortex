package store_pebble

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/zzet/gortex/internal/graph"
)

var (
	_ graph.BoundedEdgeExistenceReader            = (*Snapshot)(nil)
	_ graph.BoundedIncomingSourceReader           = (*Snapshot)(nil)
	_ graph.BoundedOutgoingEdgeIdentityReader     = (*Snapshot)(nil)
	_ graph.BoundedIncomingEdgeIdentityReader     = (*Snapshot)(nil)
	_ graph.BoundedOutgoingSiteEdgeIdentityReader = (*Snapshot)(nil)
	_ graph.CallReferenceOutgoingReader           = (*Snapshot)(nil)
	_ graph.OutgoingMetadataReader                = (*Snapshot)(nil)
)

func (s *Snapshot) FindExistingEdgeEndpoints(ctx context.Context, endpoints []graph.TypedEdgeEndpoint, limit int) (map[graph.TypedEdgeEndpoint]struct{}, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	const resource = "edge-existence predicates"
	if limit < 1 || limit > graph.MaxBoundedEdgeExistencePredicates {
		return nil, &graph.BoundedLocalizationLimitError{Resource: resource, Limit: graph.MaxBoundedEdgeExistencePredicates}
	}
	seen := make(map[graph.TypedEdgeEndpoint]struct{})
	for _, e := range endpoints {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if e.From == "" || e.To == "" || e.Kind == "" {
			continue
		}
		seen[e] = struct{}{}
		if len(seen) > limit {
			return nil, &graph.BoundedLocalizationLimitError{Resource: resource, Limit: limit}
		}
		if err := s.requireEdgeGroups(e.Kind); err != nil {
			return nil, err
		}
	}
	found := make(map[graph.TypedEdgeEndpoint]struct{})
	for key := range seen {
		prefix := keyStrings([]byte{familyEdge, edgeOutKind}, key.From, string(key.Kind))
		err := scan(ctx, s, prefix, func(_, raw []byte) (bool, error) {
			e, err := decodeEdgeLight(raw)
			if err != nil {
				return false, err
			}
			if e.To == key.To {
				found[key] = struct{}{}
				return false, nil
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
	return found, nil
}

func (s *Snapshot) FindIncomingSourcesBounded(ctx context.Context, ids []string, kind graph.EdgeKind, limit int) (graph.BoundedIncomingSourceProjection, error) {
	out := graph.BoundedIncomingSourceProjection{Sources: make(map[string][]string), Truncated: make(map[string]bool)}
	ids = slices.Collect(uniqueNonempty(ids))
	if len(ids) == 0 {
		return out, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return graph.BoundedIncomingSourceProjection{}, err
	}
	const maxInt = int(^uint(0) >> 1)
	if err := s.requireEdgeGroups(kind); err != nil {
		return graph.BoundedIncomingSourceProjection{}, err
	}
	if limit == maxInt {
		return graph.BoundedIncomingSourceProjection{}, &graph.BoundedLocalizationLimitError{Resource: "incoming-source sentinel", Limit: maxInt - 1}
	}
	for _, id := range ids {
		if limit <= 0 {
			out.Truncated[id] = true
			continue
		}
		seen := make(map[string]struct{})
		prefix := keyStrings([]byte{familyEdge, edgeIn}, id, string(kind))
		err := scan(ctx, s, prefix, func(_, raw []byte) (bool, error) {
			e, err := decodeEdgeLight(raw)
			if err != nil {
				return false, err
			}
			if e.From != "" {
				seen[e.From] = struct{}{}
			}
			return len(seen) <= limit, nil
		})
		if err != nil {
			return graph.BoundedIncomingSourceProjection{}, err
		}
		if len(seen) > limit {
			out.Truncated[id] = true
		} else if len(seen) > 0 {
			out.Sources[id] = slices.Sorted(maps.Keys(seen))
		}
	}
	if err := ctx.Err(); err != nil {
		return graph.BoundedIncomingSourceProjection{}, err
	}
	return out, nil
}

type adjacencyBudget struct{ inspected, matched int }

func checkAdjacency(ctx context.Context, keys, kinds, limit int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch {
	case limit < 1 || limit > graph.MaxBoundedAdjacencyRowsPerKey:
		return &graph.BoundedLocalizationLimitError{Resource: "adjacency rows per key", Limit: graph.MaxBoundedAdjacencyRowsPerKey}
	case kinds > graph.MaxBoundedAdjacencyKinds:
		return &graph.BoundedLocalizationLimitError{Resource: "adjacency kinds", Limit: graph.MaxBoundedAdjacencyKinds}
	case keys > graph.MaxBoundedAdjacencyKeys:
		return &graph.BoundedLocalizationLimitError{Resource: "adjacency keys", Limit: graph.MaxBoundedAdjacencyKeys}
	default:
		return nil
	}
}

func (s *Snapshot) boundedIdentities(ctx context.Context, kinds []graph.EdgeKind, limit int, budget *adjacencyBudget, prefix func(graph.EdgeKind) []byte) ([]graph.EdgeIdentity, bool, error) {
	if err := s.requireEdgeGroups(kinds...); err != nil {
		return nil, false, err
	}
	seen := make(map[graph.EdgeIdentity]struct{})
	for kind := range uniqueNonempty(kinds) {
		err := scan(ctx, s, prefix(kind), func(_, raw []byte) (bool, error) {
			budget.inspected++
			if budget.inspected > graph.MaxBoundedAdjacencyInspectedEdges {
				return false, &graph.BoundedLocalizationLimitError{Resource: "adjacency inspected edges", Limit: graph.MaxBoundedAdjacencyInspectedEdges}
			}
			e, err := decodeEdgeLight(raw)
			if err != nil {
				return false, err
			}
			ident := graph.EdgeIdentity{From: e.From, To: e.To, Kind: e.Kind, FilePath: e.FilePath, Line: e.Line}
			if _, duplicate := seen[ident]; duplicate {
				return true, nil
			}
			seen[ident] = struct{}{}
			budget.matched++
			if budget.matched > graph.MaxBoundedAdjacencyTotalIdentities {
				return false, &graph.BoundedLocalizationLimitError{Resource: "adjacency matching identities", Limit: graph.MaxBoundedAdjacencyTotalIdentities}
			}
			return len(seen) <= limit, nil
		})
		if err != nil {
			return nil, false, err
		}
		if len(seen) > limit {
			return nil, true, ctx.Err()
		}
	}
	if len(seen) == 0 {
		return nil, false, ctx.Err()
	}
	items := slices.Collect(maps.Keys(seen))
	slices.SortFunc(items, func(a, b graph.EdgeIdentity) int {
		return cmp.Or(cmp.Compare(a.From, b.From), cmp.Compare(a.To, b.To), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.FilePath, b.FilePath), cmp.Compare(a.Line, b.Line))
	})
	return items, false, ctx.Err()
}

func (s *Snapshot) boundedEndpointIdentities(ctx context.Context, ids []string, kinds []graph.EdgeKind, limit int, index byte) (graph.BoundedEdgeIdentityProjection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := checkAdjacency(ctx, len(ids), len(kinds), limit); err != nil {
		return graph.BoundedEdgeIdentityProjection{}, err
	}
	out := graph.BoundedEdgeIdentityProjection{ByEndpoint: make(map[string][]graph.EdgeIdentity), Truncated: make(map[string]bool)}
	var budget adjacencyBudget
	for _, id := range slices.Sorted(uniqueNonempty(ids)) {
		rows, truncated, err := s.boundedIdentities(ctx, kinds, limit, &budget, func(kind graph.EdgeKind) []byte { return keyStrings([]byte{familyEdge, index}, id, string(kind)) })
		if err != nil {
			return graph.BoundedEdgeIdentityProjection{}, err
		}
		if truncated {
			out.Truncated[id] = true
		} else if len(rows) > 0 {
			out.ByEndpoint[id] = rows
		}
	}
	return out, nil
}

func (s *Snapshot) FindOutgoingEdgeIdentitiesBounded(ctx context.Context, ids []string, kinds []graph.EdgeKind, limit int) (graph.BoundedEdgeIdentityProjection, error) {
	return s.boundedEndpointIdentities(ctx, ids, kinds, limit, edgeOutKind)
}

func (s *Snapshot) FindIncomingEdgeIdentitiesBounded(ctx context.Context, ids []string, kinds []graph.EdgeKind, limit int) (graph.BoundedEdgeIdentityProjection, error) {
	return s.boundedEndpointIdentities(ctx, ids, kinds, limit, edgeIn)
}

func (s *Snapshot) FindOutgoingSiteEdgeIdentitiesBounded(ctx context.Context, sites []graph.EdgeSourceSite, kinds []graph.EdgeKind, limit int) (graph.BoundedSiteEdgeIdentityProjection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := checkAdjacency(ctx, len(sites), len(kinds), limit); err != nil {
		return graph.BoundedSiteEdgeIdentityProjection{}, err
	}
	out := graph.BoundedSiteEdgeIdentityProjection{BySite: make(map[graph.EdgeSourceSite][]graph.EdgeIdentity), Truncated: make(map[graph.EdgeSourceSite]bool)}
	ordered := slices.Clone(sites)
	slices.SortFunc(ordered, func(a, b graph.EdgeSourceSite) int {
		return cmp.Or(cmp.Compare(a.From, b.From), cmp.Compare(a.Line, b.Line))
	})
	var budget adjacencyBudget
	for _, site := range slices.Compact(ordered) {
		if site.From == "" {
			continue
		}
		rows, truncated, err := s.boundedIdentities(ctx, kinds, limit, &budget, func(kind graph.EdgeKind) []byte {
			return appendKeyLine(keyStrings([]byte{familyEdge, edgeOutKind}, site.From, string(kind)), site.Line)
		})
		if err != nil {
			return graph.BoundedSiteEdgeIdentityProjection{}, err
		}
		if truncated {
			out.Truncated[site] = true
		} else if len(rows) > 0 {
			out.BySite[site] = rows
		}
	}
	return out, nil
}

func (s *Snapshot) GetCallReferenceOutEdgesContext(ctx context.Context, ids []string) (map[string][]*graph.Edge, error) {
	if ctx == nil {
		return nil, fmt.Errorf("call/reference outgoing edges: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ids = slices.Collect(uniqueNonempty(ids))
	if len(ids) == 0 {
		return nil, nil
	}
	out := make(map[string][]*graph.Edge)
	for _, id := range ids {
		// Use the canonical outgoing order before applying the CSR's kind filter.
		err := scan(ctx, s, keyStrings([]byte{familyEdge, edgeOut}, id), func(_, raw []byte) (bool, error) {
			light, err := decodeEdgeLight(raw)
			if err != nil {
				return false, err
			}
			if light.Kind == graph.EdgeCalls || light.Kind == graph.EdgeReferences {
				e, err := decodeEdge(raw)
				if err != nil {
					return false, err
				}
				out[id] = append(out[id], e)
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

func (s *Snapshot) GetOutEdgesByNodeIDsWithMetadataContext(ctx context.Context, ids []string, limit int) (map[string][]*graph.Edge, bool, error) {
	if ctx == nil {
		return nil, false, fmt.Errorf("bounded metadata outgoing edges: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	ids = slices.Sorted(uniqueNonempty(ids))
	if len(ids) == 0 {
		return nil, false, nil
	}
	if limit <= 0 {
		return nil, true, nil
	}
	out := make(map[string][]*graph.Edge)
	total := 0
	for _, id := range ids {
		var truncated bool
		err := scan(ctx, s, keyStrings([]byte{familyEdge, edgeOut}, id), func(_, raw []byte) (bool, error) {
			if total == limit {
				truncated = true
				return false, nil
			}
			e, err := decodeEdge(raw)
			if err != nil {
				return false, err
			}
			out[id] = append(out[id], e)
			total++
			return true, nil
		})
		if err != nil {
			return nil, true, err
		}
		if err := ctx.Err(); err != nil {
			return nil, true, err
		}
		if truncated {
			return out, true, nil
		}
	}
	return out, false, nil
}

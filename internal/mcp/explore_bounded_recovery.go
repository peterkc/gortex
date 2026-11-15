package mcp

import (
	"context"
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

type exploreContextNodesReader interface {
	GetNodesByIDsContext(context.Context, []string) (map[string]*graph.Node, error)
}

func exploreBoundedNodeIDs(ids []string, limit int) ([]string, bool) {
	if limit < 0 {
		return nil, false
	}
	seen := make(map[string]struct{}, min(len(ids), limit))
	out := make([]string, 0, min(len(ids), limit))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		if len(out) >= limit {
			return nil, false
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, true
}

// exploreNodesByIDsBounded exact-refetches only an already-bounded identity
// cohort. SQLite receives the request context; overlay and in-memory readers
// retain their exact-ID batch path with cancellation checked on both sides.
// False means unusable optional evidence, not proof that the IDs are absent.
func exploreNodesByIDsBounded(
	ctx context.Context,
	reader graph.Reader,
	ids []string,
	limit int,
) (map[string]*graph.Node, bool) {
	if reader == nil {
		return nil, false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	boundedIDs, ok := exploreBoundedNodeIDs(ids, limit)
	if !ok || ctx.Err() != nil {
		return nil, false
	}
	if len(boundedIDs) == 0 {
		return map[string]*graph.Node{}, true
	}
	var (
		nodes map[string]*graph.Node
		err   error
	)
	if contextual, supported := reader.(exploreContextNodesReader); supported {
		nodes, err = contextual.GetNodesByIDsContext(ctx, boundedIDs)
	} else {
		nodes = reader.GetNodesByIDs(boundedIDs)
	}
	if err != nil || ctx.Err() != nil || len(nodes) != len(boundedIDs) {
		return nil, false
	}
	for _, id := range boundedIDs {
		if nodes[id] == nil {
			return nil, false
		}
	}
	return nodes, true
}

// exploreIncomingSourcesBounded reads optional recovery evidence. False means
// unknown, not an exhaustive empty incoming set; callers still check truncation.
func exploreIncomingSourcesBounded(
	ctx context.Context,
	reader graph.Reader,
	targetIDs []string,
	kind graph.EdgeKind,
	perTargetLimit int,
) (graph.BoundedIncomingSourceProjection, bool) {
	bounded, ok := reader.(graph.BoundedIncomingSourceReader)
	if !ok || perTargetLimit <= 0 {
		return graph.BoundedIncomingSourceProjection{}, false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return graph.BoundedIncomingSourceProjection{}, false
	}
	projection, err := bounded.FindIncomingSourcesBounded(ctx, targetIDs, kind, perTargetLimit)
	if err != nil || ctx.Err() != nil {
		return graph.BoundedIncomingSourceProjection{}, false
	}
	return projection, true
}

type exploreBoundedAdjacencyDirection uint8

const (
	exploreBoundedOutgoing exploreBoundedAdjacencyDirection = iota
	exploreBoundedIncoming
)

// exploreBoundedEdgeIdentities reads one complete metadata-free adjacency
// cohort. Optional capabilities fail closed: operational errors, sticky request
// cancellation, truncated keys, foreign endpoints, and malformed identities
// discard the whole projection rather than exposing a usable prefix. False
// does not certify empty adjacency or invalidate independently ranked evidence.
func exploreBoundedEdgeIdentities(
	ctx context.Context,
	reader graph.Reader,
	endpointIDs []string,
	kinds []graph.EdgeKind,
	perEndpointLimit int,
	direction exploreBoundedAdjacencyDirection,
) (map[string][]graph.EdgeIdentity, bool) {
	if reader == nil || perEndpointLimit <= 0 {
		return nil, false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return nil, false
	}

	requested := make(map[string]struct{}, len(endpointIDs))
	for _, id := range endpointIDs {
		if id == "" {
			return nil, false
		}
		requested[id] = struct{}{}
	}
	allowedKinds := make(map[graph.EdgeKind]struct{}, len(kinds))
	for _, kind := range kinds {
		allowedKinds[kind] = struct{}{}
	}
	if len(allowedKinds) == 0 {
		return nil, false
	}

	var (
		projection graph.BoundedEdgeIdentityProjection
		err        error
	)
	switch direction {
	case exploreBoundedOutgoing:
		bounded, ok := reader.(graph.BoundedOutgoingEdgeIdentityReader)
		if !ok {
			return nil, false
		}
		projection, err = bounded.FindOutgoingEdgeIdentitiesBounded(ctx, endpointIDs, kinds, perEndpointLimit)
	case exploreBoundedIncoming:
		bounded, ok := reader.(graph.BoundedIncomingEdgeIdentityReader)
		if !ok {
			return nil, false
		}
		projection, err = bounded.FindIncomingEdgeIdentitiesBounded(ctx, endpointIDs, kinds, perEndpointLimit)
	default:
		return nil, false
	}
	if err != nil || ctx.Err() != nil {
		return nil, false
	}
	for endpoint, truncated := range projection.Truncated {
		if _, ok := requested[endpoint]; !ok || truncated {
			return nil, false
		}
	}
	ordered := make(map[string][]graph.EdgeIdentity, len(projection.ByEndpoint))
	for endpoint, identities := range projection.ByEndpoint {
		if _, ok := requested[endpoint]; !ok || len(identities) > perEndpointLimit {
			return nil, false
		}
		identities = append([]graph.EdgeIdentity(nil), identities...)
		sort.Slice(identities, func(i, j int) bool {
			left, right := identities[i], identities[j]
			if left.From != right.From {
				return left.From < right.From
			}
			if left.To != right.To {
				return left.To < right.To
			}
			if left.Kind != right.Kind {
				return left.Kind < right.Kind
			}
			if left.FilePath != right.FilePath {
				return left.FilePath < right.FilePath
			}
			return left.Line < right.Line
		})
		seen := make(map[graph.EdgeIdentity]struct{}, len(identities))
		for _, identity := range identities {
			if _, ok := allowedKinds[identity.Kind]; !ok {
				return nil, false
			}
			if direction == exploreBoundedOutgoing {
				if identity.From != endpoint || identity.To == "" {
					return nil, false
				}
			} else if identity.To != endpoint || identity.From == "" {
				return nil, false
			}
			if _, duplicate := seen[identity]; duplicate {
				return nil, false
			}
			seen[identity] = struct{}{}
		}
		ordered[endpoint] = identities
	}
	if ctx.Err() != nil {
		return nil, false
	}
	return ordered, true
}

package resolver

import (
	"github.com/zzet/gortex/internal/graph"
)

// rtkQueryVia is the Meta["via"] tag the JS/TS extractors stamp on a
// generated-hook → endpoint placeholder. RTK Query's createApi declares
// data endpoints and auto-generates a React hook per endpoint
// (`useGetUserQuery`) that has no source node — the extractor synthesizes
// both nodes and this placeholder, which this pass binds.
const rtkQueryVia = "rtk-query"

// ResolveRTKQueryCalls binds the two RTK Query edges: the synthesized
// generated-hook → endpoint placeholder (by endpoint name, gated to the
// same createApi file), and any still-unresolved component → generated-hook
// call (so a component's `useGetUserQuery()` reaches the endpoint's query
// body). The naming convention is RTK-contractual, so these land at the
// typed framework tier (ConfidenceTyped / ProvenanceFramework).
//
// Returns the number of edges landed on a real node.
func ResolveRTKQueryCalls(g graph.Store) int {
	if g == nil {
		return 0
	}
	// endpointByFileName: createApi file → endpoint name → endpoint node.
	endpointByFileName := map[string]map[string]*graph.Node{}
	// hookByName: generated-hook name → nodes.
	hookByName := map[string][]*graph.Node{}
	for _, n := range nodesByKindsOrAll(g, graph.KindFunction) {
		ep, generated := rtkQueryNode(n)
		if ep != "" {
			if endpointByFileName[n.FilePath] == nil {
				endpointByFileName[n.FilePath] = map[string]*graph.Node{}
			}
			endpointByFileName[n.FilePath][ep] = n
		}
		if generated {
			hookByName[n.Name] = append(hookByName[n.Name], n)
		}
	}
	if len(endpointByFileName) == 0 {
		return 0
	}

	resolved := 0
	var reindex []graph.EdgeReindex
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e == nil {
			continue
		}
		// (1) generated-hook → endpoint placeholder.
		name, tagged := rtkQueryCall(e)
		if tagged {
			hookFile := ""
			if hn := g.GetNode(e.From); hn != nil {
				hookFile = hn.FilePath
			}
			var target *graph.Node
			if m := endpointByFileName[hookFile]; m != nil {
				target = m[name]
			}
			rtkRebind(e, target, &reindex, &resolved)
			continue
		}
		// (2) component → generated-hook call the generic resolver left
		// unresolved (e.g. the hook is imported cross-file).
		if !graph.IsUnresolvedTarget(e.To) {
			continue
		}
		cands := hookByName[name]
		if len(cands) == 0 {
			continue
		}
		if target := pickStoreAction(g, e, sameBoundaryCandidates(g, e.From, cands)); target != nil {
			rtkRebind(e, target, &reindex, &resolved)
		}
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	return resolved
}

func rtkQueryNode(n *graph.Node) (endpoint string, generated bool) {
	if n == nil {
		return "", false
	}
	endpoint, _ = n.Meta["rtk_endpoint"].(string)
	generated, _ = n.Meta["rtk_generated_hook"].(bool)
	return endpoint, generated
}

// rtkQueryCall distinguishes an endpoint carrier from a generic unresolved hook call.
func rtkQueryCall(e *graph.Edge) (name string, tagged bool) {
	if e == nil {
		return "", false
	}
	if via, _ := e.Meta["via"].(string); via == rtkQueryVia {
		name, _ = e.Meta["rtk_endpoint"].(string)
		return name, true
	}
	if graph.IsUnresolvedTarget(e.To) {
		return graph.UnresolvedName(e.To), false
	}
	return "", false
}

// rtkRebind records a target change and counts stationary matches too.
func rtkRebind(e *graph.Edge, target *graph.Node, reindex *[]graph.EdgeReindex, resolved *int) {
	oldTo := e.To
	var id string
	if target != nil {
		id = target.ID
		*resolved++
	}
	if rebindRTKQueryCall(e, id, target != nil) {
		*reindex = append(*reindex, graph.EdgeReindex{Edge: e, OldTo: oldTo})
	}
}

// rebindRTKQueryCall returns true only when the target changes and needs reindexing.
func rebindRTKQueryCall(e *graph.Edge, target string, matched bool) bool {
	if !matched {
		name, tagged := rtkQueryCall(e)
		if !tagged {
			return false
		}
		target = "unresolved::*." + name
	}
	if e.To == target {
		return false
	}
	e.To = target
	e.Origin = graph.OriginASTInferred
	if matched {
		e.Confidence = ConfidenceTyped
		e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, ConfidenceTyped)
		StampSynthesizedTyped(e, SynthRTKQuery)
	} else {
		e.Confidence = 0
		e.ConfidenceLabel = ""
		UnstampSynthesized(e)
	}
	return true
}

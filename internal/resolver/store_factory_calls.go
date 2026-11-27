package resolver

import (
	"github.com/zzet/gortex/internal/graph"
)

// storeFactoryVia is the Meta["via"] tag the JS/TS extractors stamp on a
// store-factory call placeholder the same-file pass could not bind (the store
// lives in another file).
const storeFactoryVia = "store-factory"

// ResolveStoreFactoryCalls binds Zustand/Redux/Pinia/MobX indirect action
// calls — `useStore.getState().fetchUser()` and `const {fetchUser} =
// useStore.getState(); fetchUser()` — to the precise action node, across files.
//
// The extractors stamp each store-action node with Meta["store_factory"]=<binding>
// + Meta["store_member"]=<member>, and stamp the cross-file call placeholder
// with Meta["via"]="store-factory" + store_binding + store_action. This pass
// joins them by (binding, member). It is strictly more precise than the
// competitor's bare-name matching: two stores that each define `reset` do not
// collide, because the placeholder carries the specific store binding and the
// pass prefers the candidate in the caller's file (then a unique binding match,
// then a singleton). Returns the number of call edges landed on an action.
func ResolveStoreFactoryCalls(g graph.Store) int {
	return resolveStoreFactoryCalls(g, nil)
}

// resolveStoreFactoryCalls is the census-multiplexed form: cands, when
// non-nil, supplies the pre-matched via=store-factory EdgeCalls candidates
// and the shared function-node snapshot, replacing this pass's own
// whole-stream decode and whole-kind node scan.
func resolveStoreFactoryCalls(g graph.Store, cands *frameworkPassCandidates) int {
	if g == nil {
		return 0
	}
	var functionNodes []*graph.Node
	if cands != nil {
		functionNodes = cands.nodes.kind(g, graph.KindFunction)
	} else {
		functionNodes = nodesByKindsOrAll(g, graph.KindFunction)
	}
	// index: binding → member → action nodes (multiple files may reuse the
	// same binding name, hence a slice).
	index := map[string]map[string][]*graph.Node{}
	for _, n := range functionNodes {
		binding, member := storeFactoryAction(n)
		if binding == "" {
			continue
		}
		if index[binding] == nil {
			index[binding] = map[string][]*graph.Node{}
		}
		index[binding][member] = append(index[binding][member], n)
	}
	if len(index) == 0 {
		return 0
	}

	stream := g.EdgesByKind(graph.EdgeCalls)
	if cands != nil {
		stream = frameworkEdgeSeq(cands.calls)
	}
	resolved := 0
	var reindex []graph.EdgeReindex
	for e := range stream {
		binding, action := storeFactoryCall(e)
		if binding == "" || action == "" {
			continue
		}
		cands := sameBoundaryCandidates(g, e.From, index[binding][action])
		target := pickStoreAction(g, e, cands)

		oldTo := e.To
		var id string
		if target != nil {
			id = target.ID
			resolved++
		}
		if rebindStoreFactoryCall(e, action, id, target != nil) {
			reindex = append(reindex, graph.EdgeReindex{Edge: e, OldTo: oldTo})
		}
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	return resolved
}

// pickStoreAction disambiguates action candidates for a call: prefer the
// candidate defined in the same file as the `useXStore` getter the call's
// store binding resolves to, then the candidate in the caller's file, then a
// singleton. Returns nil when the choice is ambiguous (never guesses).
func pickStoreAction(g graph.Store, call *graph.Edge, cands []*graph.Node) *graph.Node {
	switch len(cands) {
	case 0:
		return nil
	case 1:
		return cands[0]
	}
	// Multiple stores reuse this binding name across files. The strongest
	// signal is the store DEFINITION file: the call's `store_binding` names a
	// `useXStore` getter, which lives in exactly one file. Prefer the candidate
	// action defined there — it is the store the binding actually refers to,
	// regardless of where the call sits.
	binding, _ := call.Meta["store_binding"].(string)
	if getterFile := storeGetterFile(g, binding); getterFile != "" {
		var inGetterFile *graph.Node
		for _, c := range cands {
			if c.FilePath == getterFile {
				if inGetterFile != nil {
					inGetterFile = nil // two candidates in the getter's file: no win
					break
				}
				inGetterFile = c
			}
		}
		if inGetterFile != nil {
			return inGetterFile
		}
	}
	// No definition-file signal. Fall back to the caller's file: prefer the
	// candidate co-located with the call; otherwise it's ambiguous.
	callerFile := ""
	if cn := g.GetNode(call.From); cn != nil {
		callerFile = cn.FilePath
	}
	var sameFile *graph.Node
	for _, c := range cands {
		if c.FilePath == callerFile && callerFile != "" {
			if sameFile != nil {
				return nil // two same-file candidates: ambiguous
			}
			sameFile = c
		}
	}
	return sameFile
}

// storeGetterFile returns the file that defines the `useXStore` getter named by
// binding — the variable/function/const a `defineStore` / `create` call is
// assigned to. The getter is the store DEFINITION anchor, so its file pins which
// store a binding refers to even when the same binding name is reused across
// files. Returns "" when no getter, or more than one, carries the name (no
// unambiguous definition site).
func storeGetterFile(g graph.Store, binding string) string {
	if g == nil || binding == "" {
		return ""
	}
	file := ""
	for _, n := range nodesByKindsOrAll(g, graph.KindVariable, graph.KindFunction, graph.KindConstant) {
		if !isStoreFactoryGetter(n, binding) {
			continue
		}
		if file != "" && file != n.FilePath {
			return "" // binding defined in two files: ambiguous
		}
		file = n.FilePath
	}
	return file
}

func storeFactoryAction(n *graph.Node) (binding, member string) {
	if n == nil {
		return "", ""
	}
	binding, _ = n.Meta["store_factory"].(string)
	if binding == "" {
		return "", ""
	}
	member, _ = n.Meta["store_member"].(string)
	if member == "" {
		member = n.Name
	}
	return binding, member
}

func storeFactoryCall(e *graph.Edge) (binding, action string) {
	if e == nil || e.Meta["via"] != storeFactoryVia {
		return "", ""
	}
	binding, _ = e.Meta["store_binding"].(string)
	action, _ = e.Meta["store_action"].(string)
	return binding, action
}

func isStoreFactoryGetter(n *graph.Node, binding string) bool {
	if n == nil || n.Name != binding || n.FilePath == "" {
		return false
	}
	// The getter is the factory assignment target, never an action.
	_, isAction := n.Meta["store_factory"]
	return !isAction
}

func rebindStoreFactoryCall(e *graph.Edge, action, target string, matched bool) bool {
	if !matched {
		target = "unresolved::*." + action
	}
	if e.To == target {
		return false
	}
	e.To = target
	e.Origin = graph.OriginASTInferred
	if matched {
		e.Confidence = 0.75
		e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, 0.75)
		StampSynthesized(e, SynthStoreFactory)
	} else {
		e.Confidence = 0
		e.ConfidenceLabel = ""
		UnstampSynthesized(e)
	}
	return true
}

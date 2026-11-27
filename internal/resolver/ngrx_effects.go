package resolver

import "github.com/zzet/gortex/internal/graph"

// ngrxEffectVia is the Meta["via"] tag the JS/TS extractors stamp on an NgRx
// effect's ofType placeholder -- `createEffect(() => actions$.pipe(ofType(X)))`
// links the effect to the action X it reacts to, which the static call graph
// cannot see because effects are registered with the EffectsModule, not called.
const ngrxEffectVia = "ngrx-effect"

// ResolveNgRxEffects binds NgRx effect dispatch: a `createEffect(() =>
// this.actions$.pipe(ofType(LoadUsers), ...))` effect -> the LoadUsers action it
// reacts to. The extractor tags the effect node with Meta["ngrx_effect"] and
// stamps each ofType as a placeholder EdgeCalls from the effect to
// `unresolved::*.<action>` with Meta["via"]="ngrx-effect" +
// Meta["ngrx_action"]=<action>. This pass resolves each action against (1) a
// same-file action node, then (2) a unique action match by name (the
// createAction const / action creator / type). NgRx-gated: a no-op when no effect
// placeholder edges exist.
//
// Returns the number of effect placeholders landed on a real action.
func ResolveNgRxEffects(g graph.Store) int {
	if g == nil {
		return 0
	}
	var placeholders []*graph.Edge
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if !isNgRxEffectCall(e) {
			continue
		}
		placeholders = append(placeholders, e)
	}
	if len(placeholders) == 0 {
		return 0
	}

	// Index candidate action nodes by name (createAction const / action creator
	// / action-type class).
	actionByName := map[string][]*graph.Node{}
	for _, n := range nodesByKindsOrAll(g, graph.KindVariable, graph.KindConstant, graph.KindFunction, graph.KindType) {
		if n == nil || n.Name == "" {
			continue
		}
		actionByName[n.Name] = append(actionByName[n.Name], n)
	}

	resolved := 0
	var reindex []graph.EdgeReindex
	for _, e := range placeholders {
		action := ngRxEffectAction(e)
		if action == "" {
			continue
		}
		target := pickStoreAction(g, e, sameBoundaryCandidates(g, e.From, actionByName[action]))

		oldTo := e.To
		var id string
		if target != nil {
			id = target.ID
			resolved++
		}
		if rebindNgRxEffectCall(e, action, id, target != nil) {
			reindex = append(reindex, graph.EdgeReindex{Edge: e, OldTo: oldTo})
		}
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	return resolved
}

func isNgRxEffectCall(e *graph.Edge) bool {
	return e != nil && e.Meta["via"] == ngrxEffectVia
}

func ngRxEffectAction(e *graph.Edge) string {
	if !isNgRxEffectCall(e) {
		return ""
	}
	action, _ := e.Meta["ngrx_action"].(string)
	return action
}

// rebindNgRxEffectCall returns true only when the target changes and needs reindexing.
func rebindNgRxEffectCall(e *graph.Edge, action, target string, matched bool) bool {
	if !matched {
		target = "unresolved::*." + action
	}
	if e.To == target {
		return false
	}
	e.To = target
	e.Origin = graph.OriginASTInferred
	if matched {
		e.Confidence = ConfidenceHeuristic
		e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, ConfidenceHeuristic)
		StampSynthesized(e, SynthNgRxEffect)
	} else {
		e.Confidence = 0
		e.ConfidenceLabel = ""
		UnstampSynthesized(e)
	}
	return true
}

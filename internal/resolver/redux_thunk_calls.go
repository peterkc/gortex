package resolver

import (
	"github.com/zzet/gortex/internal/graph"
)

// reduxThunkVia is the Meta["via"] tag the JS/TS extractors stamp on a
// createAsyncThunk dispatch placeholder — a `dispatch(action())` inside a
// thunk's payload-creator body that the static call graph cannot see
// because the thunk is registered, not directly called.
const reduxThunkVia = "redux-thunk"

// ResolveReduxThunkCalls binds Redux Toolkit thunk dispatch chains:
// `const fetchUser = createAsyncThunk(type, (arg, {dispatch}) => {
// dispatch(setLoading()); dispatch(slice.actions.set(...)) })` links
// fetchUser → setLoading and fetchUser → set.
//
// The extractor tags the thunk node with Meta["redux_thunk"]=<name> and
// stamps each inner dispatch as a placeholder EdgeCalls from the thunk to
// `unresolved::*.<callee>` with Meta["via"]="redux-thunk" +
// Meta["thunk_dispatch"]=<callee>. This pass resolves each callee against
// (1) a same-file thunk, (2) a unique thunk match, then (3) a store-factory
// action node (cross-linking the store-factory pass — so a thunk
// dispatching a createSlice reducer reaches the reducer). Register after
// SynthStoreFactory so the store-action nodes are present.
//
// Returns the number of dispatch placeholders landed on a real target.
func ResolveReduxThunkCalls(g graph.Store) int {
	if g == nil {
		return 0
	}
	// index: thunk nodes by name, store-factory action nodes by member.
	thunkByName := map[string][]*graph.Node{}
	actionByMember := map[string][]*graph.Node{}
	for _, n := range nodesByKindsOrAll(g, graph.KindFunction, graph.KindMethod, graph.KindVariable, graph.KindConstant) {
		if name := reduxThunkName(n); name != "" {
			thunkByName[name] = append(thunkByName[name], n)
		}
		if binding, member := storeFactoryAction(n); binding != "" {
			actionByMember[member] = append(actionByMember[member], n)
		}
	}
	if len(thunkByName) == 0 {
		return 0
	}

	resolved := 0
	var reindex []graph.EdgeReindex
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		callee := reduxThunkCallee(e)
		if callee == "" {
			continue
		}
		target := pickReduxThunkTarget(g, e, callee, thunkByName, actionByMember)

		oldTo := e.To
		var id string
		if target != nil {
			id = target.ID
			resolved++
		}
		if rebindReduxThunkCall(e, callee, id, target != nil) {
			reindex = append(reindex, graph.EdgeReindex{Edge: e, OldTo: oldTo})
		}
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	return resolved
}

// pickReduxThunkTarget resolves a dispatched callee. It prefers a thunk
// of that name (a thunk→thunk chain) — same-file first, then a unique
// match — then falls back to a store-factory action of that name, with
// the same same-file-then-unique disambiguation. Returns nil when the
// choice is ambiguous (never guesses).
func pickReduxThunkTarget(g graph.Store, call *graph.Edge, callee string, thunks, actions map[string][]*graph.Node) *graph.Node {
	if t := pickStoreAction(g, call, sameBoundaryCandidates(g, call.From, thunks[callee])); t != nil {
		return t
	}
	return pickStoreAction(g, call, sameBoundaryCandidates(g, call.From, actions[callee]))
}

func reduxThunkName(n *graph.Node) string {
	if n == nil {
		return ""
	}
	name, _ := n.Meta["redux_thunk"].(string)
	return name
}

func reduxThunkCallee(e *graph.Edge) string {
	if e == nil || e.Meta["via"] != reduxThunkVia {
		return ""
	}
	callee, _ := e.Meta["thunk_dispatch"].(string)
	return callee
}

// rebindReduxThunkCall returns true only when the target changes and needs reindexing.
func rebindReduxThunkCall(e *graph.Edge, callee, target string, matched bool) bool {
	if !matched {
		target = "unresolved::*." + callee
	}
	if e.To == target {
		return false
	}
	e.To = target
	e.Origin = graph.OriginASTInferred
	if matched {
		e.Confidence = ConfidenceHeuristic
		e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, ConfidenceHeuristic)
		StampSynthesized(e, SynthReduxThunk)
	} else {
		e.Confidence = 0
		e.ConfidenceLabel = ""
		UnstampSynthesized(e)
	}
	return true
}

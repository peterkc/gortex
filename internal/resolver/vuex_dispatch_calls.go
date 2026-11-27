package resolver

import (
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// vuexDispatchVia is the Meta["via"] tag the JS/TS extractors stamp on a
// Vuex dispatch/commit placeholder — a `dispatch('user/login')` /
// `commit('user/SET_TOKEN')` string-keyed call the static graph cannot
// resolve because the action is named by a runtime string.
const vuexDispatchVia = "vuex-dispatch"

// ResolveVuexDispatchCalls binds Vuex string-keyed dispatch/commit calls to
// the action/mutation node named by the key, with namespace
// disambiguation: `dispatch('user/login')` reaches the `user` module's
// login, never a root-level login. The extractor tags each action/mutation
// node with vuex_action + vuex_namespace + vuex_kind and stamps the
// placeholder with vuex_key (the full namespaced string) + vuex_kind. This
// is a string-keyed inference, so it lands at the heuristic tier.
//
// Returns the number of placeholders landed on a real action/mutation.
func ResolveVuexDispatchCalls(g graph.Store) int {
	if g == nil {
		return 0
	}
	// index: kind → "namespace\x00name" → nodes.
	index := map[string]map[string][]*graph.Node{"action": {}, "mutation": {}}
	for _, n := range nodesByKindsOrAll(g, graph.KindFunction, graph.KindMethod) {
		kind, key := vuexDispatchNode(n)
		if kind != "" {
			index[kind][key] = append(index[kind][key], n)
		}
	}
	if len(index["action"]) == 0 && len(index["mutation"]) == 0 {
		return 0
	}

	// dyn_shape sites, indexed for claimed-site speculative pre-emption.
	dynBySite := map[string][]*graph.Edge{}
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e == nil || e.Meta == nil {
			continue
		}
		if shape, _ := e.Meta["dyn_shape"].(string); shape == "" {
			continue
		}
		dynBySite[vuexSiteKey(e.From, e.Line)] = append(dynBySite[vuexSiteKey(e.From, e.Line)], e)
	}

	resolved := 0
	claimed := map[string]bool{}
	var reindex []graph.EdgeReindex
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		kind, key, name := vuexDispatchCall(e)
		if kind == "" {
			continue
		}
		target := pickStoreAction(g, e, sameBoundaryCandidates(g, e.From, index[kind][key]))
		oldTo := e.To
		var id string
		if target != nil {
			id = target.ID
			resolved++
			claimed[vuexSiteKey(e.From, e.Line)] = true
		}
		if rebindVuexDispatchCall(e, name, id, target != nil) {
			reindex = append(reindex, graph.EdgeReindex{Edge: e, OldTo: oldTo})
		}
	}
	for site := range claimed {
		for _, de := range dynBySite[site] {
			if de.Meta == nil {
				de.Meta = map[string]any{}
			}
			de.Meta["registry_claimed"] = true
		}
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	return resolved
}

func vuexDispatchNode(n *graph.Node) (kind, key string) {
	if n == nil {
		return "", ""
	}
	name, _ := n.Meta["vuex_action"].(string)
	kind, _ = n.Meta["vuex_kind"].(string)
	if name == "" || (kind != "action" && kind != "mutation") {
		return "", ""
	}
	ns, _ := n.Meta["vuex_namespace"].(string)
	return kind, ns + "\x00" + name
}

func vuexDispatchCall(e *graph.Edge) (kind, key, name string) {
	if e == nil {
		return "", "", ""
	}
	if via, _ := e.Meta["via"].(string); via != vuexDispatchVia {
		return "", "", ""
	}
	full, _ := e.Meta["vuex_key"].(string)
	kind, _ = e.Meta["vuex_kind"].(string)
	if full == "" || (kind != "action" && kind != "mutation") {
		return "", "", ""
	}
	ns, name := "", full
	if i := strings.LastIndex(full, "/"); i >= 0 {
		ns, name = full[:i], full[i+1:]
	}
	return kind, ns + "\x00" + name, name
}

func rebindVuexDispatchCall(e *graph.Edge, name, target string, matched bool) bool {
	if !matched {
		target = "unresolved::*." + name
	}
	if e.To == target {
		return false
	}
	e.To = target
	e.Origin = graph.OriginASTInferred
	if matched {
		e.Confidence = ConfidenceHeuristic
		e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, ConfidenceHeuristic)
		StampSynthesized(e, SynthVuexDispatch)
	} else {
		e.Confidence = 0
		e.ConfidenceLabel = ""
		UnstampSynthesized(e)
	}
	return true
}

func vuexSiteKey(from string, line int) string {
	return from + "\x00" + strconv.Itoa(line)
}

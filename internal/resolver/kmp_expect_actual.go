package resolver

import (
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// kmpExpectActualVia marks a synthesized Kotlin Multiplatform expect↔actual
// pairing edge.
const kmpExpectActualVia = "kmp.expect-actual"

// ResolveKMPExpectActual is the framework-dispatch synthesizer for Kotlin
// Multiplatform expect/actual declarations. An `expect` declaration in common
// code is fulfilled by one `actual` per platform; the link is implicit — the
// compiler matches them by signature — so the static graph leaves the expect
// declaration looking unreferenced and the per-platform actuals looking
// orphaned. The Kotlin extractor stamps Meta["kmp_role"]="expect"|"actual"; this
// pass pairs declarations of the same name and kind and synthesizes an
// implements edge from each actual to its expect (so find_implementations on
// the expect returns every platform actual) plus a references edge back for
// navigation.
//
// One expect fans out to several actuals (android / ios / jvm) by design. Full
// recompute and idempotent; edges ride at ast_inferred with synthesizer
// provenance. Returns the number of expect↔actual pairs linked.
func ResolveKMPExpectActual(g graph.Store) int {
	if g == nil {
		return 0
	}

	type key struct {
		name string
		kind graph.NodeKind
	}
	expects := map[key][]*graph.Node{}
	actuals := map[key][]*graph.Node{}
	for _, n := range nodesByKindsOrAll(g, graph.KMPExpectActualKinds()...) {
		switch graph.KMPExpectActualRole(n) {
		case "expect":
			k := key{n.Name, n.Kind}
			expects[k] = append(expects[k], n)
		case "actual":
			k := key{n.Name, n.Kind}
			actuals[k] = append(actuals[k], n)
		}
	}
	if len(expects) == 0 || len(actuals) == 0 {
		return 0
	}

	keys := make([]key, 0, len(expects))
	for k := range expects {
		if len(actuals[k]) > 0 {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].name != keys[j].name {
			return keys[i].name < keys[j].name
		}
		return keys[i].kind < keys[j].kind
	})

	var batch []*graph.Edge
	paired := 0
	for _, k := range keys {
		for _, exp := range expects[k] {
			for _, act := range actuals[k] {
				if exp.ID == act.ID {
					continue
				}
				batch = append(batch,
					kmpEdge(act, exp.ID, graph.EdgeImplements),
					kmpEdge(exp, act.ID, graph.EdgeReferences),
				)
				paired++
			}
		}
	}

	if len(batch) > 0 {
		g.AddBatch(nil, batch)
	}
	return paired
}

// kmpEdge builds one direction of an expect↔actual pairing.
func kmpEdge(from *graph.Node, toID string, kind graph.EdgeKind) *graph.Edge {
	return &graph.Edge{
		From:            from.ID,
		To:              toID,
		Kind:            kind,
		FilePath:        from.FilePath,
		Line:            from.StartLine,
		Confidence:      0.8,
		ConfidenceLabel: graph.ConfidenceLabelFor(kind, 0.8),
		Origin:          graph.OriginASTInferred,
		Meta: map[string]any{
			"via":             kmpExpectActualVia,
			MetaSynthesizedBy: SynthKMPExpectActual,
			MetaProvenance:    ProvenanceHeuristic,
		},
	}
}

package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// fabricBridgeVia marks a synthesized Fabric spec↔native-view-manager edge.
const fabricBridgeVia = "fabric.component"

// ResolveFabricComponents is the framework-dispatch synthesizer for React
// Native Fabric / Codegen view components. The TS extractor emits a node
// per `codegenNativeComponent<Props>('Name')` spec (Meta
// fabric_component, plus fabric_events from DirectEventHandler props); the
// Objective-C and Java extractors emit a node per native view manager
// (Meta fabric_component derived from the manager class name, plus
// fabric_props). This pass binds the spec to its native implementation(s)
// by component name with bidirectional EdgeReferences bridge edges, so a
// Fabric component spec resolves to the native code that renders it.
//
// Full recompute and idempotent (graph.AddEdge dedupes; graph.EvictFile
// drops the bridge on reindex). Edges ride at ast_inferred with
// synthesizer provenance.
//
// Returns the number of Fabric specs bound to at least one native view
// manager.
func ResolveFabricComponents(g graph.Store) int {
	if g == nil {
		return 0
	}

	var specs []*graph.Node
	nativeByComponent := map[string][]*graph.Node{}
	for _, n := range nodesByKindsOrAll(g, graph.KindType) {
		component, normalized, native := fabricComponentCandidate(n)
		if component == "" {
			continue
		}
		if native {
			nativeByComponent[normalized] = append(nativeByComponent[normalized], n)
		} else {
			specs = append(specs, n)
		}
	}
	if len(specs) == 0 || len(nativeByComponent) == 0 {
		return 0
	}

	var batch []*graph.Edge
	bound := 0
	for _, spec := range specs {
		comp, normalized, _ := fabricComponentCandidate(spec)
		matches := nativeByComponent[normalized]
		if len(matches) == 0 {
			continue
		}
		linked := false
		for _, native := range matches {
			if native.ID == spec.ID {
				continue
			}
			batch = append(batch,
				fabricBridgeEdge(spec, native, comp),
				fabricBridgeEdge(native, spec, comp),
			)
			linked = true
		}
		if linked {
			bound++
		}
	}

	for _, e := range batch {
		g.AddEdge(e)
	}
	return bound
}

// fabricComponentCandidate reads only the component and role markers. Both
// reducers scan Type rows; role is key presence, not the marker's value.
func fabricComponentCandidate(n *graph.Node) (component, normalized string, native bool) {
	if n == nil || n.Kind != graph.KindType {
		return "", "", false
	}
	component, _ = n.Meta["fabric_component"].(string)
	_, native = n.Meta["fabric_native"]
	return component, fabricNormalize(component), native
}

// fabricNormalize folds a component name for cross-language matching: the
// native side often carries platform prefixes/suffixes the JS spec drops
// (RCTWebView ↔ WebView). Lower-cases and strips a leading "rct"/"rn".
func fabricNormalize(name string) string {
	n := strings.ToLower(name)
	n = strings.TrimPrefix(n, "rct")
	n = strings.TrimPrefix(n, "rn")
	return n
}

func fabricBridgeEdge(from, to *graph.Node, component string) *graph.Edge {
	return &graph.Edge{
		From:            from.ID,
		To:              to.ID,
		Kind:            graph.EdgeReferences,
		FilePath:        from.FilePath,
		Line:            from.StartLine,
		Confidence:      0.6,
		ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeReferences, 0.6),
		Origin:          graph.OriginASTInferred,
		Meta: map[string]any{
			"via":              fabricBridgeVia,
			"fabric_component": component,
			"to_language":      to.Language,
			MetaSynthesizedBy:  SynthFabric,
			MetaProvenance:     ProvenanceHeuristic,
		},
	}
}

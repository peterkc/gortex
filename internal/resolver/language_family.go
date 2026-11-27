package resolver

import "github.com/zzet/gortex/internal/graph"

// languageFamily maps a language to the family within which cross-language
// symbol resolution is legitimate (a `@model Foo` / `<Counter/>` reference may
// bind across languages of the same family). "" means the language belongs to
// no multi-language family, so any cross-language bind for it is coincidental.
func languageFamily(lang string) string {
	switch lang {
	case "java", graph.KMPKotlinLanguage, "scala":
		return "jvm"
	case "swift", "objc", "objective-c", "objectivec":
		return "apple"
	case "typescript", "ts", "tsx", "javascript", "js", "jsx":
		return "web"
	case "c", "cpp", "c++", "cxx":
		return "c"
	case "csharp", "c#", "fsharp", "f#", "razor":
		return "dotnet"
	}
	return ""
}

// sameLanguageFamily reports whether a and b are the same language or belong to
// the same multi-language family (so a within-family cross-language bind is
// permitted): csharp↔razor, ts↔tsx, java↔kotlin, swift↔objc.
func sameLanguageFamily(a, b string) bool {
	if a == b {
		return a != ""
	}
	fa := languageFamily(a)
	return fa != "" && fa == languageFamily(b)
}

// frameworkBridgeSynths are the synthesizers whose entire purpose is to bridge
// two language families (JS→native, Swift→ObjC, KMP expect/actual). Their
// edges are exempt from the cross-family gate.
var frameworkBridgeSynths = map[string]bool{
	SynthSwiftObjC:       true,
	SynthReactNative:     true,
	SynthReactNativePair: true,
	SynthExpoModules:     true,
	SynthFabric:          true,
	SynthKMPExpectActual: true,
}

// gateFrameworkResult reports whether a framework-synthesized reference/import
// result should be dropped: it crosses two known, different language families
// (a coincidental PascalCase collision) and was not produced by a bridge
// synthesizer. An unknown family on either side, the same family, or a bridge
// synthesizer all permit the result.
func gateFrameworkResult(synth, fromLang, toLang string) bool {
	if frameworkBridgeSynths[synth] {
		return false
	}
	fa, fb := languageFamily(fromLang), languageFamily(toLang)
	if fa == "" || fb == "" {
		return false
	}
	return fa != fb
}

// applyFrameworkFamilyGate drops framework-synthesized reference / import edges
// that cross two known, different language families (e.g. a Razor reference
// that coincidentally bound a TypeScript component), keeping bridge-synthesizer
// edges and call/config edges. Returns the number of edges dropped.
func applyFrameworkFamilyGate(g graph.Store) int {
	return applyFrameworkFamilyGateScoped(g, nil)
}

// applyFrameworkFamilyGateScoped is the repository-frontier form. A nil scope
// preserves the full/cold whole-graph reconciliation.
func applyFrameworkFamilyGateScoped(g graph.Store, scope map[string]bool) int {
	return applyFrameworkFamilyGateScopedForFiles(g, scope, nil)
}

// applyFrameworkFamilyGateScopedForFiles narrows an incremental settle to
// reference/import edges sourced by the exact changed-file frontier. A full or
// repository-only run retains the conservative repository scan.
func applyFrameworkFamilyGateScopedForFiles(
	g graph.Store,
	scope map[string]bool,
	filePaths []string,
) int {
	if g == nil {
		return 0
	}
	if len(filePaths) > 0 {
		return applyFrameworkFamilyGateCandidates(g, frameworkFamilyGateEdgesForFiles(g, scope, filePaths))
	}
	if scope == nil {
		return applyFrameworkFamilyGateIdentities(g, frameworkFamilyGateProjectedIdentities(g))
	}
	return applyFrameworkFamilyGateCandidates(
		g,
		frameworkRepoEdges(g, scope, graph.EdgeReferences, graph.EdgeImports),
	)
}

func frameworkFamilyGateEdgesForFiles(
	g graph.Store,
	scope map[string]bool,
	filePaths []string,
) []*graph.Edge {
	identities := make([]graph.EdgeIdentity, 0)
	seen := make(map[graph.EdgeIdentity]struct{})
	for row := range graph.EdgesInScopeSeq(
		g,
		frameworkScopePrefixes(scope),
		filePaths,
		graph.EdgeReferences,
		graph.EdgeImports,
	) {
		if row.Edge == nil {
			continue
		}
		identity := graph.EdgeIdentityFor(row.Edge)
		if _, duplicate := seen[identity]; duplicate {
			continue
		}
		seen[identity] = struct{}{}
		identities = append(identities, identity)
	}
	fetched := findFrameworkEdgesByIdentities(g, identities)
	edges := make([]*graph.Edge, 0, len(fetched))
	for _, identity := range identities {
		if edge := fetched[identity]; edge != nil {
			edges = append(edges, edge)
		}
	}
	return edges
}

func frameworkFamilyGateProjectedIdentities(g graph.Store) []graph.EdgeIdentity {
	identities := make([]graph.EdgeIdentity, 0)
	seen := make(map[graph.EdgeIdentity]struct{})
	for row := range graph.FrameworkCensusEdgesSeq(g, graph.EdgeReferences, graph.EdgeImports) {
		if row.SynthesizedBy == "" || frameworkBridgeSynths[row.SynthesizedBy] {
			continue
		}
		if _, duplicate := seen[row.EdgeIdentity]; duplicate {
			continue
		}
		seen[row.EdgeIdentity] = struct{}{}
		identities = append(identities, row.EdgeIdentity)
	}
	return identities
}

func applyFrameworkFamilyGateCandidates(g graph.Store, edges []*graph.Edge) int {
	identities := make([]graph.EdgeIdentity, 0, len(edges))
	seen := make(map[graph.EdgeIdentity]struct{}, len(edges))
	for _, edge := range edges {
		if edge == nil || edge.Meta == nil {
			continue
		}
		synth, _ := edge.Meta[MetaSynthesizedBy].(string)
		if synth == "" || frameworkBridgeSynths[synth] {
			continue
		}
		identity := graph.EdgeIdentityFor(edge)
		if _, duplicate := seen[identity]; duplicate {
			continue
		}
		seen[identity] = struct{}{}
		identities = append(identities, identity)
	}
	return applyFrameworkFamilyGateIdentities(g, identities)
}

func applyFrameworkFamilyGateIdentities(g graph.Store, identities []graph.EdgeIdentity) int {
	type cand struct {
		edge  *graph.Edge
		synth string
	}
	current := findFrameworkEdgesByIdentities(g, identities)
	cands := make([]cand, 0, len(identities))
	endpointIDs := map[string]struct{}{}
	for _, identity := range identities {
		edge := current[identity]
		if edge == nil || edge.Meta == nil {
			continue
		}
		synth, _ := edge.Meta[MetaSynthesizedBy].(string)
		if synth == "" || frameworkBridgeSynths[synth] {
			continue
		}
		cands = append(cands, cand{edge: edge, synth: synth})
		endpointIDs[edge.From] = struct{}{}
		endpointIDs[edge.To] = struct{}{}
	}
	if len(cands) == 0 {
		return 0
	}
	ids := make([]string, 0, len(endpointIDs))
	for id := range endpointIDs {
		ids = append(ids, id)
	}
	nodes := g.GetNodesByIDs(ids)
	langOf := func(id string) string {
		if n := nodes[id]; n != nil {
			return n.Language
		}
		return ""
	}
	toDrop := make([]*graph.Edge, 0, len(cands))
	for _, c := range cands {
		if gateFrameworkResult(c.synth, langOf(c.edge.From), langOf(c.edge.To)) {
			toDrop = append(toDrop, c.edge)
		}
	}
	return graph.RemoveEdgesExact(g, toDrop)
}

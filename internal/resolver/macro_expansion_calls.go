package resolver

import (
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// macroExpansionVia marks a call edge synthesized at a function-like
// macro's expansion (use) site.
//
// The C/C++ extractor recovers the calls hidden inside a function-like
// macro's replacement list and attributes them to the macro's `#define`
// line — that is where the call *text* lives. But it is not where the
// call *happens*: a `CALL_M(o);` in real code expands to `(o)->run()` at
// the invocation line, inside the invoking function. This pass adds, at
// the use site, the caller -> callee edge the expansion implies, so a
// forward call walk (get_call_chain) shows `caller -> run` where the
// macro is invoked, not only at the definition.
const macroExpansionVia = "macro_expansion"

// macroFunctionKindMeta is the Meta["macro_kind"] value the extractor
// stamps on a function-like (parameterised) macro node — the only macro
// shape whose use site is a call_expression and whose body can hide
// calls. Object-like macros are skipped.
const macroFunctionKindMeta = "function"

// ResolveMacroExpansionCalls mints, for every site that invokes a
// function-like macro, a direct caller -> callee call edge for each call
// the macro's body hides — attributed to the USE-SITE line and file
// rather than the macro's `#define` line.
//
// Recoverable seam: a function-like macro invocation `CALL_M(o)` parses
// (the preprocessor is not run) as an ordinary call_expression naming
// CALL_M, so the extractor emits `caller --(use line)--> unresolved::CALL_M`.
// The macro node itself is KindMacro, which the call resolver never binds
// (it accepts only function / method targets), so the use-site edge stays
// an `unresolved::<macro>` placeholder carrying the use-site line — the
// durable signal this pass keys on. The macro node already carries the
// recovered body callees as its out-edges (`macro --(define line)-->
// callee`, emitted by the extractor).
//
// For each use-site edge whose name uniquely binds — within the caller's
// repo — to one function-like macro that recovered callees, the pass
// emits `caller --(use line)--> callee` for each recovered callee. The
// edge rides at OriginASTInferred (a heuristic materialisation, never an
// upgrade over a compiler-verified fact) and carries the macro-expansion
// provenance. It is idempotent: the edge key includes the line,
// graph.AddEdge dedupes by key, and an existing edge at that key authored
// by anything other than this pass is left untouched, so a real (e.g.
// LSP-resolved) edge is never overwritten or downgraded.
//
// Conservative by construction: a macro name bound by more than one
// function-like macro in the same repo is ambiguous and skipped; the
// macro's def-site recovered-callee edges and the use-site placeholder
// both remain, so this pass only ever adds the use-site attribution and
// never removes the existing edges.
//
// Returns the number of use-site call edges the pass owns after this run.
func ResolveMacroExpansionCalls(g graph.Store) int {
	return resolveMacroExpansionCalls(g, nil)
}

// resolveMacroExpansionCalls is the census-multiplexed form: cands, when
// non-nil, supplies the shared macro-node snapshot and the pre-matched
// use-site candidates (edges naming a function-like macro), replacing the
// pass's own whole-kind node scan and EdgeCalls decode. The candidate
// vocabulary is a name-only superset of the index built here (it skips the
// recovered-callee requirement), so the loop's own byKey / macroByID
// filters remain the deciding gate.
func resolveMacroExpansionCalls(g graph.Store, cands *frameworkPassCandidates) int {
	if g == nil {
		return 0
	}

	// Index function-like macros that recovered body callees, keyed by
	// (repo, name). A name bound by more than one such macro in a repo is
	// ambiguous: mark it so every use site of that name is skipped.
	type macroEntry struct {
		node      *graph.Node
		callees   []string
		ambiguous bool
	}
	byKey := map[string]*macroEntry{}
	macroByID := map[string]*macroEntry{}
	macroNames := map[string]struct{}{}
	macroKey := func(repo, name string) string { return repo + "\x00" + name }

	var macroNodes []*graph.Node
	if cands != nil {
		macroNodes = cands.nodes.kind(g, graph.KindMacro)
	} else {
		macroNodes = nodesByKindsOrAll(g, graph.KindMacro)
	}
	var macros []*graph.Node
	var macroIDs []string
	for _, n := range macroNodes {
		if !functionLikeMacro(n) {
			continue
		}
		macros = append(macros, n)
		macroIDs = append(macroIDs, n.ID)
	}
	macroOut := g.GetOutEdgesByNodeIDs(macroIDs)
	for _, n := range macros {
		callees := macroBodyCallees(macroOut[n.ID])
		if len(callees) == 0 {
			continue
		}
		key := macroKey(n.RepoPrefix, n.Name)
		if entry, ok := byKey[key]; ok {
			entry.ambiguous = true
			macroByID[n.ID] = entry
			macroNames[n.Name] = struct{}{}
			continue
		}
		entry := &macroEntry{node: n, callees: callees}
		byKey[key] = entry
		macroByID[n.ID] = entry
		macroNames[n.Name] = struct{}{}
	}
	if len(byKey) == 0 {
		return 0
	}

	// Collect use-site edges first so the AddEdge writes below cannot
	// mutate a live EdgesByKind iteration. A use site is either an
	// unresolved `unresolved::<macro>` placeholder (the common case — the
	// call resolver leaves macro-named calls unresolved) or a call edge
	// landed directly on the macro node (defensive: no resolver does this
	// today, but a future one might).
	type useSite struct {
		caller string
		file   string
		line   int
		entry  *macroEntry
	}
	useSiteStream := g.EdgesByKind(graph.EdgeCalls)
	if cands != nil {
		useSiteStream = frameworkEdgeSeq(cands.calls)
	}
	var callEdges []*graph.Edge
	var candidateCallerIDs []string
	callerSeen := make(map[string]struct{})
	for e := range useSiteStream {
		if e == nil || e.From == "" || e.To == "" {
			continue
		}
		if graph.IsUnresolvedTarget(e.To) {
			name := graph.UnresolvedName(e.To)
			if _, candidate := macroNames[name]; !candidate {
				continue
			}
			if _, seen := callerSeen[e.From]; !seen {
				callerSeen[e.From] = struct{}{}
				candidateCallerIDs = append(candidateCallerIDs, e.From)
			}
		} else if macroByID[e.To] == nil {
			continue
		}
		callEdges = append(callEdges, e)
	}
	callers := g.GetNodesByIDs(candidateCallerIDs)
	var sites []useSite
	for _, e := range callEdges {
		var entry *macroEntry
		if graph.IsUnresolvedTarget(e.To) {
			caller := callers[e.From]
			if caller == nil {
				continue
			}
			entry = byKey[macroKey(caller.RepoPrefix, graph.UnresolvedName(e.To))]
		} else {
			entry = macroByID[e.To]
		}
		if entry == nil || entry.ambiguous {
			continue
		}
		sites = append(sites, useSite{caller: e.From, file: e.FilePath, line: e.Line, entry: entry})
	}
	if len(sites) == 0 {
		return 0
	}

	// Deterministic order so the owned-edge count and the batched writes
	// are stable across runs.
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].caller != sites[j].caller {
			return sites[i].caller < sites[j].caller
		}
		if sites[i].file != sites[j].file {
			return sites[i].file < sites[j].file
		}
		if sites[i].line != sites[j].line {
			return sites[i].line < sites[j].line
		}
		return sites[i].entry.node.ID < sites[j].entry.node.ID
	})

	callerIDs := make([]string, 0, len(sites))
	for _, site := range sites {
		callerIDs = append(callerIDs, site.caller)
	}
	existingCalls := make(map[graph.EdgeIdentity]*graph.Edge)
	for _, edges := range g.GetOutEdgesByNodeIDs(dedupeFrameworkIDs(callerIDs)) {
		for _, edge := range edges {
			if edge != nil && edge.Kind == graph.EdgeCalls {
				existingCalls[graph.EdgeIdentityFor(edge)] = edge
			}
		}
	}

	owned := 0
	for _, s := range sites {
		for _, callee := range s.entry.callees {
			if callee == s.caller {
				continue
			}
			edge := macroExpansionEdge(s.caller, callee, s.file, s.line, s.entry.node.Name)
			key := graph.EdgeIdentityFor(edge)
			if !macroExpansionMayAdd(existingCalls[key]) {
				// A real edge already occupies this identity. Leave it alone.
				continue
			}
			g.AddEdge(edge)
			existingCalls[key] = edge
			owned++
		}
	}
	return owned
}

func functionLikeMacro(n *graph.Node) bool {
	if n == nil || n.Name == "" {
		return false
	}
	kind, _ := n.Meta["macro_kind"].(string)
	return kind == macroFunctionKindMeta
}

func macroBodyCallee(e *graph.Edge) string {
	if e == nil || e.Kind != graph.EdgeCalls {
		return ""
	}
	return e.To
}

func macroExpansionMayAdd(existing *graph.Edge) bool {
	if existing == nil {
		return true
	}
	via, _ := existing.Meta["via"].(string)
	return via == macroExpansionVia
}

func macroExpansionEdge(caller, callee, file string, line int, name string) *graph.Edge {
	return &graph.Edge{
		From:            caller,
		To:              callee,
		Kind:            graph.EdgeCalls,
		FilePath:        file,
		Line:            line,
		Confidence:      ConfidenceHeuristic,
		ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeCalls, ConfidenceHeuristic),
		Origin:          graph.OriginASTInferred,
		Meta: map[string]any{
			"via":             macroExpansionVia,
			"macro":           name,
			MetaSynthesizedBy: SynthMacroExpansion,
			MetaProvenance:    ProvenanceHeuristic,
		},
	}
}

// macroBodyCallees returns the distinct call targets a macro node's body
// recovered — the `To` of each EdgeCalls out-edge the extractor emitted
// from the macro node — in stable order.
func macroBodyCallees(edges []*graph.Edge) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range edges {
		callee := macroBodyCallee(e)
		if callee == "" || seen[callee] {
			continue
		}
		seen[callee] = true
		out = append(out, callee)
	}
	sort.Strings(out)
	return out
}

package resolver

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zzet/gortex/internal/graph"
)

// temporalStubPrefix is the placeholder namespace the Go extractor
// emits for a Temporal workflow → activity (or workflow → child
// workflow) dispatch it can't land locally
// (`unresolved::temporal::<kind>::<name>`).
const temporalStubPrefix = unresolvedPrefix + "temporal::"

// temporalEnvDefaultConfidence is stamped on a stub edge whose name was
// resolved through an env-var-with-literal-default variable (the parser
// tags it `temporal_name_origin=env_default`). It sits in the
// speculative band (< 0.5) so the edge lands at the AMBIGUOUS label and,
// together with MetaSpeculative, is hidden from default queries: the
// runtime env override may name a different handler than the default.
const temporalEnvDefaultConfidence = 0.4

// temporalCrossLangConfidence is stamped on a cross-language Temporal link
// (e.g. a Java service that starts a Go workflow, matched by canonical
// name across a type-system boundary with no compiler guarantee the names
// line up). It sits in the speculative band so the edge is hidden from
// default queries, consistent with the env-default tier.
const temporalCrossLangConfidence = 0.4

// temporalExactSigConfidence is stamped on an exact-name signature-gated
// resolution (Cat 4): a PascalCase dispatch landed on a same-named,
// unregistered, non-suffixed function whose signature matches the dispatch
// kind. The name match is exact and the signature is a strong positive
// gate, but the target is not register-confirmed — so it sits in the
// speculative band (hidden, AMBIGUOUS) like the other best-guess tiers.
const temporalExactSigConfidence = 0.45

// temporalEnvDefaultInferredConfidence is stamped instead when the env-default
// was recognised with high confidence — a provable os.Getenv read or a helper
// name in the configured allow-list (`temporal_env_source` = "os_getenv" /
// "allowlist"). It sits in the inferred band (≥ 0.5, visible by default): we
// trust that the dispatch DOES default to this name, leaving the residual
// "runtime may override" risk to the optional LLM cleaning pass.
const temporalEnvDefaultInferredConfidence = 0.6

// Temporal annotation node IDs the Java extractor emits via
// EmitAnnotationEdge. The resolver consumes these to discover
// temporal-tagged interfaces and methods.
const (
	javaActivityIfaceAnnoID = "annotation::java::ActivityInterface"
	javaWorkflowIfaceAnnoID = "annotation::java::WorkflowInterface"
	javaActivityMethodID    = "annotation::java::ActivityMethod"
	javaWorkflowMethodID    = "annotation::java::WorkflowMethod"
	javaSignalMethodID      = "annotation::java::SignalMethod"
	javaQueryMethodID       = "annotation::java::QueryMethod"
	javaUpdateMethodID      = "annotation::java::UpdateMethod"
)

// ResolveTemporalCalls is the graph-wide materialisation pass for the
// Temporal workflow → activity dispatch layer (N35). It performs two
// complementary jobs:
//
//  1. Role tagging. Stamps `temporal_role` (one of "workflow" /
//     "activity" / "activity_interface" / "workflow_interface" /
//     "signal" / "query" / "update") on every node the SDK treats as
//     a workflow / activity. Discovery uses two signals: (a) Go
//     `worker.RegisterActivity(F)` / `RegisterWorkflow(F)` calls,
//     emitted by the Go extractor as EdgeCalls edges carrying
//     `Meta["via"]="temporal.register"` and `Meta["temporal_name"]=<F>`;
//     (b) Java `@ActivityInterface` / `@WorkflowInterface` /
//     `@SignalMethod` / `@QueryMethod` / `@UpdateMethod` annotations,
//     emitted by the Java extractor as EdgeAnnotated edges to a
//     well-known synthetic annotation node. For Java interface
//     annotations the role is propagated to every implementor's
//     matching method via EdgeImplements + name match — that gives
//     queries a flat view of "every activity method in this codebase"
//     without re-walking the interface chain.
//
//  2. Stub-call resolution. Every Go `workflow.ExecuteActivity(ctx, F,
//     ...)` call is emitted as an EdgeCalls edge to a
//     `unresolved::temporal::<kind>::<name>` placeholder carrying
//     `Meta["via"]="temporal.stub"`. This pass rewrites each such edge
//     to point at the function the worker registered under that name.
//     The Java side is already resolved by normal interface dispatch
//     (`stub.someMethod()` is a call on a `@ActivityInterface` type;
//     the existing AST resolver lands it on the interface method, and
//     EdgeImplements connects to the impl); the role tag in step 1 is
//     the only extra surface Java needs.
//
// The pass is a full recompute and idempotent: every temporal.stub
// edge's target is recomputed from its own `temporal_name` meta on
// every call, so it is incremental-safe — a reindex of either the
// workflow or the activity file leaves the meta intact and the next
// pass re-lands (or un-lands) the edge. graph.ReindexEdge keeps the
// out/in buckets consistent. An edge whose target is no longer in the
// graph is reset back to the placeholder and loses its
// resolution-tier metadata.
//
// Runs at every resolver settle point that already runs InferImplements
// (so the Java interface → impl chain has its EdgeImplements edges)
// and after ResolveGRPCStubCalls (so the two SDK passes share the
// same post-condition).
//
// Returns the number of temporal.stub edges pointing at a resolved
// handler after the pass.
// argNameAt reads the positional arg name recorded on a call edge.
//
// PURPOSE — read the positional arg name recorded on a call edge by the extractor
// RATIONALE — arg_names can be []string (most paths) or []any (json-round-tripped)
// KEYWORDS — arg_names, wrapper-following, position
func argNameAt(e *graph.Edge, pos int) string {
	if e == nil || e.Meta == nil || pos < 0 {
		return ""
	}
	switch a := e.Meta["arg_names"].(type) {
	case []string:
		if pos < len(a) {
			return a[pos]
		}
	case []any:
		if pos < len(a) {
			if s, ok := a[pos].(string); ok {
				return s
			}
		}
	}
	return ""
}

// metaIntValue coerces an int-ish meta value to an int.
//
// PURPOSE — coerce various numeric representations of a position to int
// RATIONALE — meta values can be stored as int, int64, float64, or string depending on serialization
// KEYWORDS — position, coercion, param, wrapper-following
func metaIntValue(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	case string:
		if n, err := strconv.Atoi(x); err == nil {
			return n, true
		}
	}
	return 0, false
}

// temporalWrapperStubExists is the idempotence guard for the wrapper pass.
//
// PURPOSE — prevent duplicate wrapper-synthesized stub edges on repeated resolver runs
// RATIONALE — resolveTemporalWrapperCalls runs on every settle; the guard is O(out-edges of caller)
// KEYWORDS — idempotence, temporal.stub, wrapper
func temporalWrapperStubExists(g graph.Store, from, kind, name string) bool {
	for _, e := range g.GetOutEdges(from) {
		if e == nil || e.Meta == nil {
			continue
		}
		if v, _ := e.Meta["via"].(string); v != "temporal.stub" {
			continue
		}
		if k, _ := e.Meta["temporal_kind"].(string); k != kind {
			continue
		}
		if n, _ := e.Meta["temporal_name"].(string); n == name {
			return true
		}
	}
	return false
}

// resolveTemporalWrapperCalls synthesises temporal.stub edges at callers of
// dispatch wrappers, propagating the caller's literal arg value through the
// wrapper's forwarded parameter.
//
// PURPOSE — synthesize temporal.stub edges at callers of wrapper functions that forward
//
//	a parameter as the dispatch name, propagating the caller's literal arg value
//
// RATIONALE — single-level pass: find edges WITH temporal_name_param, find their callers,
//
//	extract the arg at the wrapper's param position, emit a new stub
//
// KEYWORDS — wrapper-following, temporal.stub, arg_names, single-level
// resolveTemporalWrapperCalls returns the fresh wrapper-stub edges it
// synthesised, so the caller can iterate the pass to a fixpoint and follow
// multi-hop (depth>1) wrapper chains. When shared is set, seed carries the
// stub edges to discover wrappers among (the census candidates plus the
// stubs earlier iterations emitted) in place of this function's own
// whole-stream decode; the caller scan that finds each wrapper's invokers
// keeps its stream — it matches arbitrary call edges, not a via-tagged
// subset — and runs only when a wrapper actually exists.
func resolveTemporalWrapperCalls(g graph.Store, seed []*graph.Edge, shared bool) []*graph.Edge {
	type wrapper struct {
		id, kind, name string
		pos            int
	}
	byID := map[string]wrapper{}
	byName := map[string][]wrapper{}

	stubStream := g.EdgesByKind(graph.EdgeCalls)
	if shared {
		stubStream = frameworkEdgeSeq(seed)
	}
	for e := range stubStream {
		if e == nil || e.Meta == nil || e.From == "" {
			continue
		}
		if v, _ := e.Meta["via"].(string); v != "temporal.stub" {
			continue
		}
		param, _ := e.Meta["temporal_name_param"].(string)
		kind, _ := e.Meta["temporal_kind"].(string)
		if param == "" || kind == "" {
			continue
		}
		if _, seen := byID[e.From]; seen {
			continue
		}
		pn := g.GetNode(e.From + "#param:" + param)
		if pn == nil {
			continue
		}
		pos, ok := metaIntValue(pn.Meta["position"])
		if !ok {
			continue
		}
		wname := ""
		if wnode := g.GetNode(e.From); wnode != nil {
			wname = wnode.Name
		}
		w := wrapper{id: e.From, kind: kind, name: wname, pos: pos}
		byID[e.From] = w
		if wname != "" {
			byName[wname] = append(byName[wname], w)
		}
	}
	if len(byID) == 0 {
		return nil
	}

	type pending struct {
		from, file, kind, name, wrapperName string
		line                                int
		// fwdParam, when non-empty, marks this emitted stub as itself a
		// name-forwarding wrapper: the caller passed its OWN parameter
		// (named fwdParam) into the inner wrapper's name position, so the
		// caller is a transitive wrapper the NEXT iteration must discover.
		// The stub then carries temporal_name_param=fwdParam (the depth>1
		// hook), enabling iterative resolution.
		fwdParam string
	}
	var out []pending
	emit := func(w wrapper, ce *graph.Edge) {
		if ce.From == w.id {
			return
		}
		name := argNameAt(ce, w.pos)
		if name == "" {
			return
		}
		// Depth>1 propagation: if the forwarded argument is itself a
		// parameter of the caller (a `<caller>#param:<name>` node with a
		// position exists), the caller merely passes a name THROUGH — it is
		// a transitive wrapper. Emit a temporal_name_param stub so the next
		// iteration discovers the caller as a wrapper and reaches its own
		// callers. Otherwise the argument is a literal / const NAME the main
		// resolver lands directly, and no further hop is needed.
		fwd := ""
		if pn := g.GetNode(ce.From + "#param:" + name); pn != nil && pn.Kind == graph.KindParam {
			if _, ok := metaIntValue(pn.Meta["position"]); ok {
				fwd = name
			}
		}
		out = append(out, pending{from: ce.From, file: ce.FilePath, line: ce.Line,
			kind: w.kind, name: name, wrapperName: w.name, fwdParam: fwd})
	}

	for ce := range g.EdgesByKind(graph.EdgeCalls) {
		if ce == nil || ce.From == "" || ce.Meta == nil {
			continue
		}
		if _, ok := ce.Meta["arg_names"]; !ok {
			continue
		}
		if w, ok := byID[ce.To]; ok {
			emit(w, ce)
			continue
		}
		callee, _ := ce.Meta["callee"].(string)
		if callee == "" {
			continue
		}
		for _, w := range byName[callee] {
			emit(w, ce)
		}
	}

	var added []*graph.Edge
	for _, p := range out {
		if temporalWrapperStubExists(g, p.from, p.kind, p.name) {
			continue
		}
		meta := map[string]any{
			"via":                  "temporal.stub",
			"temporal_kind":        p.kind,
			"temporal_name":        p.name,
			"temporal_via_wrapper": p.wrapperName,
		}
		// Transitive wrapper: stamp temporal_name_param so the next
		// iteration discovers p.from as a wrapper and propagates through
		// to its own callers (depth > 1).
		if p.fwdParam != "" {
			meta["temporal_name_param"] = p.fwdParam
		}
		stub := &graph.Edge{
			From: p.from, To: temporalStubPlaceholder(p.kind, p.name),
			Kind: graph.EdgeCalls, FilePath: p.file, Line: p.line,
			Meta: meta,
		}
		g.AddEdge(stub)
		added = append(added, stub)
	}
	return added
}

func ResolveTemporalCalls(g graph.Store) int {
	return resolveTemporalCalls(g, nil)
}

// resolveTemporalCalls is the census-multiplexed form of the pass: cands,
// when non-nil, carries every temporal.* EdgeCalls edge and every temporal
// annotation edge the shared full-census walk collected, replacing this
// pass's own presence probe and per-phase whole-stream decodes. The wrapper
// fixpoint's caller scan and the cross-language node sweeps keep their own
// streams: the former discovers arbitrary callers of wrapper functions, the
// latter must observe the temporal_role stamps this very run writes.
func resolveTemporalCalls(g graph.Store, cands *frameworkPassCandidates) int {
	if g == nil {
		return 0
	}
	// Presence probe, hoisted above the lock and the wrapper/executor
	// pre-passes: every input any phase of this pass consumes is either a
	// calls-edge whose RAW via carries the "temporal." prefix (stub / start /
	// register / executor-field — tested before any kind/name filtering, so
	// malformed stubs still count as presence) or a Java temporal annotation
	// edge. A workspace with neither provably yields zero from every phase,
	// and previously still paid the fixpoint's own full calls-scans
	// (measured 43.9s for nothing on a temporal-free 28-repo workspace).
	// Break-on-first-hit keeps temporal-using workspaces at ~zero extra cost.
	// On the multiplexed path the census already answered the same two
	// predicates: the collected buffers ARE the probe.
	if cands != nil {
		if len(cands.calls) == 0 && len(cands.annotated) == 0 {
			return 0
		}
	} else {
		temporalPresent := false
		for e := range g.EdgesByKind(graph.EdgeCalls) {
			if e == nil || e.Meta == nil {
				continue
			}
			if v, _ := e.Meta["via"].(string); strings.HasPrefix(v, "temporal.") {
				temporalPresent = true
				break
			}
		}
		if !temporalPresent {
			for e := range g.EdgesByKind(graph.EdgeAnnotated) {
				if e == nil {
					continue
				}
				if r, m := temporalRoleForJavaAnnotation(e.To); r != "" || m != "" {
					temporalPresent = true
					break
				}
			}
		}
		if !temporalPresent {
			return 0
		}
	}
	// Serialise against other graph-wide passes that mutate Node.Meta
	// (markTestSymbolsAndEmitEdges, detectClonesAndEmitEdges,
	// reach.BuildIndex). stampTemporalRole below writes n.Meta on
	// existing graph nodes; without this lock a concurrent reader
	// (e.g. clone detection invoked from indexFile) trips the runtime's
	// "concurrent map read and map write" check.
	mu := g.ResolveMutex()
	mu.Lock()
	defer mu.Unlock()

	// Wrapper-following pre-pass: synthesise temporal.stub edges at callers of
	// wrapper functions that forward a parameter as the Temporal dispatch name.
	// Must run before the stub-collection sweep so the freshly synthesised stubs
	// are picked up and resolved by the existing loop below. Iterate to a
	// fixpoint: each pass may turn a caller into a newly-discovered transitive
	// wrapper (a stub stamped temporal_name_param), which the next pass follows
	// up another hop — depth>1 wrapper chains resolve this way. The pass is
	// idempotent (temporalWrapperStubExists guards re-emission), so it
	// terminates once no fresh stub is added; the bound caps pathological
	// chains.
	// On the multiplexed path each phase re-reads the collected candidates
	// in their CURRENT store form — one batched read per phase in place of
	// one whole-stream decode per phase, with the same per-phase object
	// isolation a fresh scan gives each backend.
	shared := cands != nil
	var wrapperSeed []*graph.Edge
	if shared {
		wrapperSeed = cands.calls
	}
	for i := 0; i < 16; i++ {
		added := resolveTemporalWrapperCalls(g, wrapperSeed, shared)
		if len(added) == 0 {
			break
		}
		if shared {
			// The next iteration discovers transitive wrappers among the
			// stubs this one just emitted; grow the seed instead of
			// re-scanning the stream for them.
			wrapperSeed = append(wrapperSeed, added...)
		}
	}

	// Executor-field pre-pass: rewrite struct-field dispatch stubs to the
	// literal name supplied at the executor's construction site. Also runs
	// before the sweep so the rewritten stubs resolve below.
	if shared {
		resolveTemporalExecutorFields(g, cands.calls, true)
	} else {
		resolveTemporalExecutorFields(g, nil, false)
	}

	// Single sweep over EdgeCalls — the largest edge class — collecting
	// both the temporal.register edges (index inputs) and the
	// temporal.stub edges (edges to resolve), instead of scanning it once
	// per concern. The From IDs of stub edges are gathered so the
	// per-edge caller lookup below collapses to one batch fetch.
	type stubEdge struct {
		edge       *graph.Edge
		kind, name string
	}
	var stubs []stubEdge
	var registerEdges []*graph.Edge
	fromIDSet := map[string]struct{}{}
	sweepStream := g.EdgesByKind(graph.EdgeCalls)
	if shared {
		// The census candidates plus this pass's own staged wrapper stubs
		// are exactly the overlay a fresh stream walk would surface here.
		sweep := cands.calls
		if bs, ok := g.(*frameworkEdgeBatchStore); ok {
			sweep = append(sweep, bs.stagedEdgesMatching(func(e *graph.Edge) bool {
				if e.Kind != graph.EdgeCalls || e.Meta == nil {
					return false
				}
				v, _ := e.Meta["via"].(string)
				return strings.HasPrefix(v, "temporal.")
			})...)
		}
		sweepStream = frameworkEdgeSeq(sweep)
	}
	for e := range sweepStream {
		if e == nil || e.Meta == nil {
			continue
		}
		switch v, _ := e.Meta["via"].(string); v {
		case "temporal.register":
			registerEdges = append(registerEdges, e)
		case "temporal.stub", "temporal.start":
			// temporal.stub is a workflow→activity / workflow→child-workflow
			// dispatch; temporal.start is a service→workflow start
			// (client.ExecuteWorkflow / SignalWithStartWorkflow). Both
			// resolve the same way — rewrite to the registered handler /
			// workflow found by <kind>::<name>.
			kind, _ := e.Meta["temporal_kind"].(string)
			name, _ := e.Meta["temporal_name"].(string)
			if kind == "" || name == "" {
				continue
			}
			stubs = append(stubs, stubEdge{edge: e, kind: kind, name: name})
			if e.From != "" {
				fromIDSet[e.From] = struct{}{}
			}
		}
	}

	// Probe the (smaller) annotation class for Java temporal tags. The
	// census walk already collected the matching edges on the multiplexed
	// path; no pass mutates annotation edges, so they are used directly.
	var annotatedEdges []*graph.Edge
	if shared {
		annotatedEdges = cands.annotated
	} else {
		for e := range g.EdgesByKind(graph.EdgeAnnotated) {
			if e == nil {
				continue
			}
			if r, m := temporalRoleForJavaAnnotation(e.To); r == "" && m == "" {
				continue
			}
			annotatedEdges = append(annotatedEdges, e)
		}
	}

	// Early-out: a graph with no Temporal register / stub / annotation
	// edges (the common case for most repos) skips all node fetches,
	// index building, role stamping, and Java propagation entirely — the
	// pass costs only the two EdgesByKind scans above.
	if len(registerEdges) == 0 && len(stubs) == 0 && len(annotatedEdges) == 0 {
		return 0
	}

	var snap *frameworkNodeSnapshot
	if shared {
		snap = cands.nodes
	}
	idx := buildTemporalIndex(g, registerEdges, annotatedEdges, snap)
	resolved := 0
	var reindexBatch []graph.EdgeReindex
	fromList := make([]string, 0, len(fromIDSet))
	for id := range fromIDSet {
		fromList = append(fromList, id)
	}
	callerNodes := g.GetNodesByIDs(fromList)

	// Const-dereference map: a dispatch named through a string const
	// (`const ChargeCardActivity = "ChargeCard"`) reaches the resolver as
	// the identifier "ChargeCardActivity"; map it to the literal value so
	// the lookup keys on the registered name. Built once from the
	// queryable constant_values sidecar.
	stubNames := make([]string, 0, len(stubs))
	for _, s := range stubs {
		stubNames = append(stubNames, s.name)
		// Env-default const reference: the helper's default argument was a
		// constant NAME (`GetEnvOrDefault(KEY, config.ACTIVITY_NAME_DEFAULT)`),
		// recorded as `temporal_default_const`. Include it so the deref map
		// resolves it to its literal value alongside the dispatch names.
		if cn, _ := s.edge.Meta["temporal_default_const"].(string); cn != "" {
			stubNames = append(stubNames, cn)
		}
	}
	derefByName := buildConstDerefMap(g, stubNames)
	deref := func(name string) string { return derefByName[name] }

	for _, s := range stubs {
		e := s.edge
		callerRepo := ""
		callerLang := ""
		if from := callerNodes[e.From]; from != nil {
			callerRepo = from.RepoPrefix
			callerLang = from.Language
		}
		oldTo := e.To
		bound, moved := rebindTemporalStub(e, s.kind, s.name, callerRepo, callerLang, idx, deref)
		if bound {
			resolved++
		}
		if !moved {
			continue
		}
		reindexBatch = append(reindexBatch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
	}
	if len(reindexBatch) > 0 {
		g.ReindexEdges(reindexBatch)
	}
	// Link Java consumers (workflow starts / signals / queries) to the Go
	// workflows and handlers they target, by shared canonical name. Runs
	// last: it reads temporal_role / temporal_name meta stamped by the
	// sweep above and the via=temporal.handler edges emitted by the Go
	// extractor. Additive — graph.AddEdge dedupes.
	if shared {
		resolveTemporalCrossLanguage(g, cands.calls, true)
	} else {
		resolveTemporalCrossLanguage(g, nil, false)
	}
	return resolved
}

type temporalRebindLookup interface {
	lookup(kind, name, callerRepo, callerLang string) (id, origin string, confidence float64)
	lookupConvention(kind, name, callerRepo, callerLang string) string
	lookupCrossLang(kind, name, callerLang string) (id string, ok bool)
	lookupExactSig(kind, name, callerRepo string) string
}

// rebindTemporalStub returns whether a handler matched and whether the
// target moved. Only a move needs reindexing; SQLite does not persist the
// decoded payload of a stationary row.
func rebindTemporalStub(e *graph.Edge, kind, name, callerRepo, callerLang string,
	idx temporalRebindLookup, deref func(string) string,
) (resolved, moved bool) {
	handlerID, origin, conf := idx.lookup(kind, name, callerRepo, callerLang)
	// When the direct name didn't resolve, try dereferencing it as a
	// string constant and re-looking-up under the literal value. The
	// deref value is computed up front so it also feeds the
	// cross-language join below (a Java const-ref dispatch through
	// `Constants.X` derefs X to the literal, then matches the Go
	// workflow of that name across the type-system boundary).
	constDeref := ""
	derefVal := deref(name)
	if handlerID == "" && derefVal != "" {
		if hID, o, c := idx.lookup(kind, derefVal, callerRepo, callerLang); hID != "" {
			handlerID, origin, conf = hID, o, c
			constDeref = derefVal
		}
	}
	// Env-default const reference: the env-helper's default argument was a
	// constant reference (`GetEnvOrDefault(KEY, config.ACTIVITY_NAME_DEFAULT)`),
	// recorded as `temporal_default_const`. Substitute the constant's literal
	// VALUE through the deref map (register-confirmed candidates), then look
	// it up. The env-default tier override below keeps the edge at the
	// const_ref tier (inferred, visible) regardless of how it resolved.
	if handlerID == "" {
		if cn, _ := e.Meta["temporal_default_const"].(string); cn != "" {
			if v := deref(cn); v != "" {
				if id, o, c := idx.lookup(kind, v, callerRepo, callerLang); id != "" {
					handlerID, origin, conf = id, o, c
					e.Meta["temporal_const_value"] = v
				}
			}
		}
	}

	// Convention fallback: dispatch to an activity/workflow FUNCTION
	// by name when the worker registers it elsewhere (unregistered
	// here) — Pattern 2 / Stage 1.2. Try the dispatch name, then its
	// const-deref value. Landed at the inferred tier (name convention,
	// not a register-confirmed binding). Tried before the cross-language
	// join: a same-language convention match is a stronger signal than a
	// speculative by-string match across a type-system boundary.
	convention := false
	if handlerID == "" {
		candNames := []string{name}
		if v := derefVal; v != "" && v != name {
			candNames = append(candNames, v)
		}
		for _, nm := range candNames {
			if id := idx.lookupConvention(kind, nm, callerRepo, callerLang); id != "" {
				handlerID, origin, conf = id, graph.OriginASTInferred, 0.6
				convention = true
				if nm != name {
					constDeref = nm
				}
				break
			}
		}
	}

	// Cross-language join: a consumer (typically a temporal.start, e.g.
	// a Java service starting a Go workflow) with no same-language
	// handler is matched to a unique other-language candidate by
	// canonical name, at the speculative tier.
	crossLang := false
	if handlerID == "" {
		matchName := name
		if derefVal != "" {
			matchName = derefVal
		}
		if hID, ok := idx.lookupCrossLang(kind, matchName, callerLang); ok {
			handlerID = hID
			origin = graph.OriginSpeculative
			conf = temporalCrossLangConfidence
			crossLang = true
			if derefVal != "" {
				constDeref = derefVal
			}
		}
	}

	// Exact-name signature-gated fallback (Cat 4): a PascalCase dispatch
	// whose target is a same-named, unregistered, non-suffixed function.
	// Gated on exact name + a kind-matching Temporal signature + a unique
	// candidate; landed speculative + hidden. Last resort, so it runs
	// only after register / const-deref / cross-language all abstained.
	exactSig := false
	if handlerID == "" {
		matchName := name
		if constDeref != "" {
			matchName = constDeref
		}
		if id := idx.lookupExactSig(kind, matchName, callerRepo); id != "" {
			handlerID = id
			origin = graph.OriginSpeculative
			conf = temporalExactSigConfidence
			exactSig = true
		}
	}

	// When the dispatch name came from an env-var-with-literal-default
	// variable (env_default) or from tracing a bare local dispatch
	// variable to its last assignment (var_trace), the value is a
	// best-guess. HOW it was recognised decides the tier:
	//   - "allowlist" / "os_getenv" / "const_ref": confident it IS an
	//     env-with-default — inferred tier (0.6, visible).
	//   - "heuristic" / var_trace / unknown: a guess — hidden speculative
	//     tier (0.4), where the optional LLM cleaning pass can prune it.
	envDefault := false
	envSource := ""
	switch v, _ := e.Meta["temporal_name_origin"].(string); v {
	case "env_default", "var_trace":
		envDefault = true
		envSource, _ = e.Meta["temporal_env_source"].(string)
	}
	envSpeculative := envDefault && envSource != "allowlist" && envSource != "os_getenv" && envSource != "const_ref"
	if handlerID != "" && envDefault {
		if envSpeculative {
			origin = graph.OriginSpeculative
			conf = temporalEnvDefaultConfidence
		} else {
			origin = graph.OriginASTInferred
			conf = temporalEnvDefaultInferredConfidence
		}
	}

	want := handlerID
	if want == "" {
		want = temporalStubPlaceholder(kind, name)
	}
	if e.To == want {
		return handlerID != "", false
	}

	e.To = want
	if handlerID != "" {
		e.Origin = origin
		e.Confidence = conf
		e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, conf)
		e.Meta["temporal_resolution"] = origin
		if envSpeculative || crossLang || exactSig {
			e.Meta[graph.MetaSpeculative] = true
		}
		if crossLang {
			e.Meta["temporal_cross_lang"] = true
		} else {
			delete(e.Meta, "temporal_cross_lang")
		}
		if exactSig {
			e.Meta["temporal_resolution_via"] = "exact_sig"
		} else if convention {
			e.Meta["temporal_resolution_via"] = "convention"
		} else {
			delete(e.Meta, "temporal_resolution_via")
		}
		if constDeref != "" {
			e.Meta["temporal_const_deref"] = constDeref
		} else {
			delete(e.Meta, "temporal_const_deref")
		}
		StampSynthesized(e, SynthTemporalStub)
	} else {
		e.Origin = ""
		e.Confidence = 0
		e.ConfidenceLabel = ""
		delete(e.Meta, "temporal_resolution")
		delete(e.Meta, graph.MetaSpeculative)
		delete(e.Meta, "temporal_const_deref")
		delete(e.Meta, "temporal_cross_lang")
		delete(e.Meta, "temporal_resolution_via")
		UnstampSynthesized(e)
	}
	return handlerID != "", true
}

// temporalStubPlaceholder is the canonical placeholder target for an
// unresolved Temporal stub call.
func temporalStubPlaceholder(kind, name string) string {
	return temporalStubPrefix + kind + "::" + name
}

// resolveTemporalCrossLanguage links Java consumers to the Go workflows /
// handlers they target, by shared canonical name:
//
//   - a Java `@WorkflowInterface` method (role "workflow", canonical name
//     from `@WorkflowMethod(name=…)`) → the Go workflow registered /
//     named the same  → via=temporal.start-workflow.
//   - a Java `@SignalMethod(name="cancel")` → a Go workflow that serves
//     signal "cancel" (via=temporal.handler kind=signal) →
//     via=temporal.signal-link.
//   - a Java `@QueryMethod(name="status")` → a Go query handler →
//     via=temporal.query-link.
//
// All edges carry cross_language=true and are emitted at the inferred
// tier (the link is by string name across the type-system boundary).
// graph.AddEdge dedupes → idempotent.
//
// When shared is set, sharedCalls carries the pass's temporal.* candidates
// in their current form and replaces the temporal.handler stream decode.
// The four node sweeps keep their own streams deliberately: they read the
// temporal_role / temporal_name stamps THIS run's earlier phases wrote, and
// a census-time snapshot predates those stamps on disk-backed stores.
func resolveTemporalCrossLanguage(g graph.Store, sharedCalls []*graph.Edge, shared bool) {
	// Go provider indexes, by name.
	goWorkflow := map[string][]string{}
	goSignalWf := map[string][]string{}
	goQueryWf := map[string][]string{}
	addGoWorkflow := func(n *graph.Node) {
		if n == nil || n.Language != "go" {
			return
		}
		if r, _ := n.Meta["temporal_role"].(string); r != "workflow" {
			return
		}
		name := n.Name
		if tn, _ := n.Meta["temporal_name"].(string); tn != "" {
			name = tn
		}
		goWorkflow[name] = append(goWorkflow[name], n.ID)
		if n.Name != name {
			goWorkflow[n.Name] = append(goWorkflow[n.Name], n.ID)
		}
	}
	for n := range g.NodesByKind(graph.KindFunction) {
		addGoWorkflow(n)
	}
	for n := range g.NodesByKind(graph.KindMethod) {
		addGoWorkflow(n)
	}
	handlerStream := g.EdgesByKind(graph.EdgeCalls)
	if shared {
		handlerStream = frameworkEdgeSeq(sharedCalls)
	}
	for e := range handlerStream {
		if e == nil || e.Meta == nil || e.From == "" {
			continue
		}
		if v, _ := e.Meta["via"].(string); v != "temporal.handler" {
			continue
		}
		from := g.GetNode(e.From)
		if from == nil || from.Language != "go" {
			continue
		}
		kind, _ := e.Meta["temporal_kind"].(string)
		name, _ := e.Meta["temporal_name"].(string)
		if name == "" {
			continue
		}
		switch kind {
		case "signal":
			goSignalWf[name] = append(goSignalWf[name], e.From)
		case "query":
			goQueryWf[name] = append(goQueryWf[name], e.From)
		}
	}
	if len(goWorkflow) == 0 && len(goSignalWf) == 0 && len(goQueryWf) == 0 {
		return
	}

	type link struct {
		from, to, via string
	}
	var out []link
	consume := func(n *graph.Node) {
		if n == nil || n.Language != "java" {
			return
		}
		role, _ := n.Meta["temporal_role"].(string)
		name, _ := n.Meta["temporal_name"].(string)
		if name == "" {
			name = n.Name
		}
		var targets []string
		var via string
		switch role {
		case "workflow":
			targets, via = goWorkflow[name], "temporal.start-workflow"
		case "signal":
			targets, via = goSignalWf[name], "temporal.signal-link"
		case "query":
			targets, via = goQueryWf[name], "temporal.query-link"
		default:
			return
		}
		for _, to := range targets {
			if to != n.ID {
				out = append(out, link{from: n.ID, to: to, via: via})
			}
		}
	}
	for n := range g.NodesByKind(graph.KindMethod) {
		consume(n)
	}
	for n := range g.NodesByKind(graph.KindFunction) {
		consume(n)
	}
	for _, l := range out {
		g.AddEdge(&graph.Edge{
			From:   l.from,
			To:     l.to,
			Kind:   graph.EdgeCalls,
			Origin: graph.OriginASTInferred,
			Meta: map[string]any{
				"via":            l.via,
				"cross_language": true,
			},
		})
	}
}

// temporalIndex maps (kind, name) to candidate handler nodes plus the
// origin / confidence tier the resolver should stamp on the rewritten
// edge.
type temporalIndex struct {
	// byKindName maps "<kind>::<name>" → handler candidate nodes.
	byKindName map[string][]*graph.Node
	// funcExact indexes EXPORTED Go functions / methods by their exact
	// (bare) name — including those WITHOUT the Activity/Workflow suffix and
	// those never passed to worker.Register*. Used only by lookupExactSig,
	// the precision-gated last resort for a PascalCase dispatch whose target
	// is a same-named, unregistered function (the "PascalCase, target in
	// another package/repo" category). Resolution there is gated on an exact
	// name match, a Temporal-shaped signature for the kind, and uniqueness.
	funcExact map[string][]*graph.Node
	// funcByName indexes Go functions / methods whose name follows the
	// activity / workflow naming convention (suffix "Activity" /
	// "Workflow"), keyed by bare name. Used as a last-resort, lower-
	// confidence resolution for dispatch to an UNREGISTERED activity —
	// the common case where activity repos hold the functions but the
	// `worker.Register*` calls live in a separate worker-runner (so the
	// register-based byKindName index never sees them). Pattern 2's
	// two-part name resolves here once F1/F2 reduce it to the func name.
	funcByName map[string][]*graph.Node
}

// isCrossRepoTestStub reports whether candidate n is a `*_test.go` node in a
// DIFFERENT repo than the dispatching caller. Such a node is, in practice, a
// test mock / fixture of the activity / workflow (a `workflow_test.go` stub),
// not the real cross-repo implementation; matching a dispatch to it mints a
// spurious edge — the one confirmed false positive in the L1 corpus audit (a
// service repo's dispatch resolving to a `*_test.go` stub in an unrelated
// workflow repo). Same-repo test files stay eligible: the overwhelmingly
// common test-workflow → test-activity edge within one package is correct.
// An empty callerRepo or candidate RepoPrefix can't establish the cross-repo
// relation, so the node is left eligible (precision over recall in reverse —
// we only suppress when we are sure both repos are known and differ).
func isCrossRepoTestStub(n *graph.Node, callerRepo string) bool {
	if n == nil || callerRepo == "" || n.RepoPrefix == "" || n.RepoPrefix == callerRepo {
		return false
	}
	return strings.HasSuffix(n.FilePath, "_test.go")
}

// eligibleTemporalCandidates drops cross-repo `*_test.go` stub candidates (see
// isCrossRepoTestStub) from a candidate list, returning the input unchanged
// when nothing is suppressed.
func eligibleTemporalCandidates(cands []*graph.Node, callerRepo string) []*graph.Node {
	if callerRepo == "" {
		return cands
	}
	var out []*graph.Node
	suppressed := false
	for _, n := range cands {
		if isCrossRepoTestStub(n, callerRepo) {
			suppressed = true
			continue
		}
		out = append(out, n)
	}
	if !suppressed {
		return cands
	}
	return out
}

func (idx *temporalIndex) lookup(kind, name, callerRepo, callerLang string) (id, origin string, confidence float64) {
	all := eligibleTemporalCandidates(idx.byKindName[kind+"::"+name], callerRepo)
	if len(all) == 0 {
		return "", "", 0
	}
	// Language gate: a Temporal stub call resolves only within its own
	// language. The candidate set co-mingles Go register targets and Java
	// annotation-tagged methods under the same "<kind>::<name>" key with
	// no language tag, so without this gate a Go workflow.ExecuteActivity
	// stub could land on a Java method node when names collide and that
	// Java entry is the unique overall candidate (pickGoTemporalTarget
	// gates language only on the Go register-indexing path, not here). The
	// intentional Java→Go cross-language join is a separate, explicitly
	// cross-language pass, not this same-language stub resolver.
	cands := all
	if callerLang != "" {
		cands = cands[:0:0]
		for _, n := range all {
			if n.Language == callerLang {
				cands = append(cands, n)
			}
		}
		if len(cands) == 0 {
			return "", "", 0
		}
	}
	// Prefer same-repo, then unique overall.
	var sameRepo []*graph.Node
	for _, n := range cands {
		if callerRepo != "" && n.RepoPrefix == callerRepo {
			sameRepo = append(sameRepo, n)
		}
	}
	if len(sameRepo) == 1 {
		return sameRepo[0].ID, graph.OriginASTResolved, 0.9
	}
	if len(sameRepo) == 0 && len(cands) == 1 {
		return cands[0].ID, graph.OriginASTResolved, 0.9
	}
	return "", "", 0
}

// lookupConvention resolves a dispatch name to a convention-named Go
// function (suffix "Activity" / "Workflow" matching the kind) when no
// registered handler matched — the unregistered-activity case (Pattern 2
// / Stage 1.2). Returns "" when there's no unambiguous candidate. The
// caller stamps this at a lower (inferred) confidence than a
// register-confirmed match.
//
// callerLang mirrors the same-language gate idx.lookup applies: a Go
// workflow.ExecuteActivity dispatch resolves only to a Go function, never
// to a like-named symbol in another language.
func (idx *temporalIndex) lookupConvention(kind, name, callerRepo, callerLang string) string {
	// Drop cross-repo *_test.go stubs (test mocks of the activity/workflow)
	// the same way the register path does, so a convention match never lands
	// on a cross-repo test fixture.
	cands := eligibleTemporalCandidates(idx.funcByName[name], callerRepo)
	if len(cands) == 0 {
		return ""
	}
	suffix := "Activity"
	if kind == "workflow" {
		suffix = "Workflow"
	}
	var filtered, sameRepo []*graph.Node
	for _, n := range cands {
		if !strings.HasSuffix(n.Name, suffix) {
			continue
		}
		if callerLang != "" && n.Language != callerLang {
			continue
		}
		filtered = append(filtered, n)
		if callerRepo != "" && n.RepoPrefix == callerRepo {
			sameRepo = append(sameRepo, n)
		}
	}
	if len(sameRepo) == 1 {
		return sameRepo[0].ID
	}
	if len(filtered) == 1 {
		return filtered[0].ID
	}
	return ""
}

// lookupCrossLang is the cross-language fallback for a Temporal consumer
// whose same-language lookup found no handler: it matches a candidate in a
// DIFFERENT language by canonical name (e.g. a Java service that starts a
// Go workflow, or vice-versa). The match is a by-string name across a
// type-system boundary with no compiler guarantee, so it resolves only
// when there is exactly ONE other-language candidate for the name — and
// the caller lands it at the speculative tier. Returns ("", false) when
// the join is absent or ambiguous.
func (idx *temporalIndex) lookupCrossLang(kind, name, callerLang string) (id string, ok bool) {
	all := idx.byKindName[kind+"::"+name]
	if len(all) == 0 || callerLang == "" {
		return "", false
	}
	var other []*graph.Node
	for _, n := range all {
		if n != nil && n.Language != callerLang {
			other = append(other, n)
		}
	}
	if len(other) == 1 {
		return other[0].ID, true
	}
	return "", false
}

// signatureMatchesKind reports whether a function's first-parameter type
// POSITIVELY matches the dispatch kind in the Temporal Go SDK convention:
// activities take context.Context, workflows take workflow.Context. It
// requires the expected type to be PRESENT in the signature AND the other
// absent, so it is a strong, precision-positive gate for the exact-name
// fallback (a kind mismatch — an activity dispatch onto a workflow.Context
// function — abstains).
func signatureMatchesKind(n *graph.Node, kind string) bool {
	if n == nil {
		return false
	}
	sig, _ := n.Meta["signature"].(string)
	if sig == "" {
		return false
	}
	want, other := "context.Context", "workflow.Context"
	if kind == "workflow" {
		want, other = "workflow.Context", "context.Context"
	}
	return strings.Contains(sig, want) && !strings.Contains(sig, other)
}

// lookupExactSig is the precision-gated last resort for a PascalCase dispatch
// whose target is a same-named function that is neither register-confirmed
// nor convention-suffixed (the "PascalCase, target in another package/repo"
// category). It resolves ONLY when the dispatch name exactly equals an
// exported Go func/method name, that candidate's signature matches the
// dispatch kind (activity → context.Context / workflow → workflow.Context),
// and the match is unique (same-repo preferred, else unique workspace-wide).
// Cross-repo `*_test.go` candidates are dropped. The caller lands the result
// at the speculative, hidden tier. Returns "" otherwise.
func (idx *temporalIndex) lookupExactSig(kind, name, callerRepo string) string {
	cands := idx.funcExact[name]
	if len(cands) == 0 {
		return ""
	}
	var filtered, sameRepo []*graph.Node
	for _, n := range cands {
		if n == nil {
			continue
		}
		// Drop a cross-repo test stub: a different-repo candidate living in a
		// _test.go file is almost never the production handler.
		if callerRepo != "" && n.RepoPrefix != "" && n.RepoPrefix != callerRepo &&
			strings.HasSuffix(n.FilePath, "_test.go") {
			continue
		}
		if !signatureMatchesKind(n, kind) {
			continue
		}
		filtered = append(filtered, n)
		if callerRepo != "" && n.RepoPrefix == callerRepo {
			sameRepo = append(sameRepo, n)
		}
	}
	if len(sameRepo) == 1 {
		return sameRepo[0].ID
	}
	if len(filtered) == 1 {
		return filtered[0].ID
	}
	return ""
}

// buildTemporalIndex (a) stamps temporal_role on every node identifiable
// as a Temporal workflow / activity via either Go `worker.Register*`
// calls or Java `@ActivityInterface` / `@WorkflowInterface` annotations
// (propagated to interface implementors), and (b) returns a name index
// the stub-call resolver consults.
//
// registerEdges and annotatedEdges are the temporal.register EdgeCalls
// edges and the temporal-annotation EdgeAnnotated edges, already
// collected by the single ResolveTemporalCalls sweep — passing them in
// avoids re-scanning the (largest) EdgeCalls class and the EdgeAnnotated
// class a second time.
// A non-nil snap feeds the function / method sweep from the run-wide shared
// node snapshot instead of this pass's own kind scans; the sweep reads only
// names and extractor-stamped signature meta, which no synthesizer mutates.
func buildTemporalIndex(g graph.Store, registerEdges, annotatedEdges []*graph.Edge, snap *frameworkNodeSnapshot) *temporalIndex {
	idx := &temporalIndex{
		byKindName: map[string][]*graph.Node{},
		funcExact:  map[string][]*graph.Node{},
		funcByName: map[string][]*graph.Node{},
	}

	// funcExact index (Cat 4): every EXPORTED Go function / method by exact
	// bare name, for the signature-gated exact-name last resort. Bounded to
	// exported names carrying a signature so the map stays small; consulted
	// only when register / const-deref / cross-language all abstain.
	indexExactFunc := func(n *graph.Node) {
		if n == nil || n.Language != "go" || n.Name == "" {
			return
		}
		if c := n.Name[0]; c < 'A' || c > 'Z' {
			return
		}
		if sig, _ := n.Meta["signature"].(string); sig == "" {
			return
		}
		idx.funcExact[n.Name] = append(idx.funcExact[n.Name], n)
	}

	// Convention index: Go functions / methods named like activities or
	// workflows (suffix "Activity" / "Workflow"), for resolving dispatch
	// to functions the worker-runner registers elsewhere (unregistered
	// here). Bounded to the convention-named set to keep it small. Consumed
	// by lookupConvention as a last-resort fallback after the register- and
	// const-deref-based byKindName lookups miss.
	indexConventionFunc := func(n *graph.Node) {
		if n == nil || n.Language != "go" {
			return
		}
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			return
		}
		if strings.HasSuffix(n.Name, "Activity") || strings.HasSuffix(n.Name, "Workflow") {
			idx.funcByName[n.Name] = append(idx.funcByName[n.Name], n)
		}
	}

	// Single sweep over functions + methods populates both indexes.
	indexFuncNode := func(n *graph.Node) {
		indexExactFunc(n)
		indexConventionFunc(n)
	}
	for _, n := range frameworkKindNodes(g, snap, graph.KindFunction) {
		indexFuncNode(n)
	}
	for _, n := range frameworkKindNodes(g, snap, graph.KindMethod) {
		indexFuncNode(n)
	}

	// Phase 1 — Go side. Walk the pre-collected `temporal.register` edges
	// and stamp the registered function's node.
	//
	// Collect every register edge's targets first so we can batch-fetch
	// every caller node and resolve every Go target name in one pair of
	// round-trips, instead of N AllNodes scans + N GetNode calls.
	type goRegister struct {
		edge *graph.Edge
		kind string
		// name is the function-reference identifier (used to locate the
		// registered node); regName is the canonical registered name (the
		// index key) — they differ only when RegisterActivityWithOptions
		// overrides the name via RegisterOptions{Name: "..."}. For a plural
		// registration name is the struct TYPE name and regName is unused.
		name, regName string
		// plural marks a RegisterActivities(&Struct{}) struct registration:
		// every exported method of the struct is promoted to an activity.
		plural bool
	}
	var goRegisters []goRegister
	registerCallerIDs := map[string]struct{}{}
	registerNames := map[string]struct{}{}
	for _, e := range registerEdges {
		if e == nil || e.Meta == nil {
			continue
		}
		kind, _ := e.Meta["temporal_kind"].(string)
		name, _ := e.Meta["temporal_name"].(string)
		if kind == "" || name == "" {
			continue
		}
		regName, _ := e.Meta["temporal_registered_name"].(string)
		if regName == "" {
			regName = name
		}
		plural, _ := e.Meta["temporal_register_plural"].(bool)
		goRegisters = append(goRegisters, goRegister{edge: e, kind: kind, name: name, regName: regName, plural: plural})
		if e.From != "" {
			registerCallerIDs[e.From] = struct{}{}
		}
		registerNames[name] = struct{}{}
	}
	callerList := make([]string, 0, len(registerCallerIDs))
	for id := range registerCallerIDs {
		callerList = append(callerList, id)
	}
	registerCallers := g.GetNodesByIDs(callerList)
	nameList := make([]string, 0, len(registerNames))
	for n := range registerNames {
		nameList = append(nameList, n)
	}
	candidatesByName := g.FindNodesByNames(nameList)

	for _, r := range goRegisters {
		caller := registerCallers[r.edge.From]
		if caller == nil {
			continue
		}
		if r.plural {
			// RegisterActivities(&MyActivities{}): promote every exported
			// method of the struct to an activity keyed by its method name.
			typeNode := pickGoTypeNode(candidatesByName[r.name], caller)
			if typeNode == nil {
				continue
			}
			for _, m := range exportedGoMethodsOfType(g, typeNode) {
				stampTemporalRole(g, m, r.kind, m.Name)
				idx.byKindName[r.kind+"::"+m.Name] = append(idx.byKindName[r.kind+"::"+m.Name], m)
			}
			continue
		}
		target := pickGoTemporalTarget(candidatesByName[r.name], caller)
		if target == nil {
			continue
		}
		// Stamp + index under the canonical registered name (regName),
		// which is the func-ref name unless a RegisterOptions{Name}
		// override renamed it — that is the name a dispatch matches.
		stampTemporalRole(g, target, r.kind, r.regName)
		idx.byKindName[r.kind+"::"+r.regName] = append(idx.byKindName[r.kind+"::"+r.regName], target)
	}

	// Phase 2 — Java side. Walk the pre-collected temporal-annotation
	// `EdgeAnnotated` edges to find temporal-tagged interfaces and
	// methods. As with Phase 1, batch the From-side GetNode calls.
	type javaAnno struct {
		fromID                string
		ifaceRole, methodRole string
		args                  string // raw annotation inner-parens text
	}
	var javaAnnos []javaAnno
	annoFromIDs := map[string]struct{}{}
	for _, e := range annotatedEdges {
		if e == nil {
			continue
		}
		role, methodRole := temporalRoleForJavaAnnotation(e.To)
		if role == "" && methodRole == "" {
			continue
		}
		args, _ := e.Meta["args"].(string)
		javaAnnos = append(javaAnnos, javaAnno{fromID: e.From, ifaceRole: role, methodRole: methodRole, args: args})
		if e.From != "" {
			annoFromIDs[e.From] = struct{}{}
		}
	}
	annoFromList := make([]string, 0, len(annoFromIDs))
	for id := range annoFromIDs {
		annoFromList = append(annoFromList, id)
	}
	annoFromNodes := g.GetNodesByIDs(annoFromList)

	type javaIfaceTag struct {
		ifaceID    string
		role       string // "activity_interface" / "workflow_interface"
		namePrefix string // @ActivityInterface(namePrefix = "...")
	}
	var javaIfaces []javaIfaceTag
	for _, a := range javaAnnos {
		from := annoFromNodes[a.fromID]
		if from == nil {
			continue
		}
		// Method-level annotation: stamp + index under the canonical
		// Temporal name (explicit @XxxMethod(name=) > activity Capitalize >
		// bare method name) so it keys off the same string a matching Go
		// registration uses.
		if a.methodRole != "" && (from.Kind == graph.KindMethod || from.Kind == graph.KindFunction) {
			canonical := javaMethodCanonicalName(a.methodRole, from.Name, a.args)
			stampTemporalRole(g, from, a.methodRole, canonical)
			key := normaliseTemporalKind(a.methodRole) + "::" + canonical
			idx.byKindName[key] = append(idx.byKindName[key], from)
			continue
		}
		// Interface-level annotation: queue for the propagation pass.
		if a.ifaceRole != "" && from.Kind == graph.KindInterface {
			stampTemporalRole(g, from, a.ifaceRole, from.Name)
			javaIfaces = append(javaIfaces, javaIfaceTag{
				ifaceID:    from.ID,
				role:       a.ifaceRole,
				namePrefix: javaAnnotationStringArg(a.args, "namePrefix"),
			})
		}
	}

	// Phase 3 — Java propagation. For each tagged interface, find its
	// methods (flat nodes living in the same file, within the
	// interface's line range) and stamp them. Then walk EdgeImplements
	// from each implementor and tag its same-named methods.
	//
	// Build a single Java method index up front via NodesByKind, then
	// project it into the two views the propagation needs:
	//   - methodsByFile: file path → []*method (used for interface
	//     methods, which the Java extractor emits as flat
	//     <file>::<name> nodes whose StartLine sits inside the
	//     interface's line range).
	//   - methodsByReceiver: receiver class name → []*method (used for
	//     impl-class methods, which carry Meta["receiver"]).
	// One pass beats AllNodes() per interface.
	javaMethodsByFile, javaMethodsByReceiver := buildJavaMethodViews(g, len(javaIfaces))

	// Prefetch the interface nodes + the implementing-type nodes for
	// the entire iface set so the propagation loop never issues an
	// inline GetNode.
	ifaceIDs := make([]string, 0, len(javaIfaces))
	for _, t := range javaIfaces {
		ifaceIDs = append(ifaceIDs, t.ifaceID)
	}
	ifaceNodes := g.GetNodesByIDs(ifaceIDs)
	implTypeIDSet := map[string]struct{}{}
	implIDsByIface := map[string][]string{}
	for _, t := range javaIfaces {
		for _, ie := range g.GetInEdges(t.ifaceID) {
			if ie == nil || ie.Kind != graph.EdgeImplements {
				continue
			}
			implIDsByIface[t.ifaceID] = append(implIDsByIface[t.ifaceID], ie.From)
			if ie.From != "" {
				implTypeIDSet[ie.From] = struct{}{}
			}
		}
	}
	implTypeIDList := make([]string, 0, len(implTypeIDSet))
	for id := range implTypeIDSet {
		implTypeIDList = append(implTypeIDList, id)
	}
	implTypeNodes := g.GetNodesByIDs(implTypeIDList)

	for _, t := range javaIfaces {
		methodRole := "activity"
		if t.role == "workflow_interface" {
			methodRole = "workflow"
		}
		iface := ifaceNodes[t.ifaceID]
		if iface == nil {
			continue
		}
		// Canonical Temporal name for a method of this interface: a
		// workflow's type is the interface simple name; an activity's type
		// is its method name capitalized, with the @ActivityInterface
		// namePrefix prepended. Keyed the same for interface and impl
		// methods (same method name) so a dispatch lands on either.
		canonicalFor := func(m *graph.Node) string {
			if t.role == "workflow_interface" {
				return iface.Name
			}
			return t.namePrefix + capitalizeASCII(m.Name)
		}
		ifaceMethods := collectJavaInterfaceMethodsFromIndex(iface, javaMethodsByFile)
		for _, m := range ifaceMethods {
			// A method carrying its own @WorkflowMethod / @SignalMethod /
			// @QueryMethod / @UpdateMethod annotation was already stamped
			// (with its name= override) in Phase 2 — don't let the
			// interface-level role clobber a more specific method role.
			if r, _ := m.Meta["temporal_role"].(string); r != "" {
				continue
			}
			canonical := canonicalFor(m)
			stampTemporalRole(g, m, methodRole, canonical)
			idx.byKindName[methodRole+"::"+canonical] = append(idx.byKindName[methodRole+"::"+canonical], m)
		}
		// Propagate to implementing classes' methods.
		implMethodNames := map[string]struct{}{}
		for _, m := range ifaceMethods {
			implMethodNames[m.Name] = struct{}{}
		}
		for _, implTypeID := range implIDsByIface[t.ifaceID] {
			implType := implTypeNodes[implTypeID]
			if implType == nil {
				continue
			}
			for _, m := range methodsOfJavaTypeFromIndex(implType, javaMethodsByReceiver) {
				if _, ok := implMethodNames[m.Name]; !ok {
					continue
				}
				canonical := canonicalFor(m)
				stampTemporalRole(g, m, methodRole, canonical)
				idx.byKindName[methodRole+"::"+canonical] = append(idx.byKindName[methodRole+"::"+canonical], m)
			}
		}
	}

	return idx
}

// temporalRoleForJavaAnnotation maps a Java annotation node ID to a
// (interface-role, method-role) pair. Only one is non-empty per
// annotation; the caller uses whichever fits the annotated node kind.
func temporalRoleForJavaAnnotation(annoID string) (ifaceRole, methodRole string) {
	switch annoID {
	case javaActivityIfaceAnnoID:
		return "activity_interface", ""
	case javaWorkflowIfaceAnnoID:
		return "workflow_interface", ""
	case javaActivityMethodID:
		return "", "activity"
	case javaWorkflowMethodID:
		return "", "workflow"
	case javaSignalMethodID:
		return "", "signal"
	case javaQueryMethodID:
		return "", "query"
	case javaUpdateMethodID:
		return "", "update"
	}
	return "", ""
}

// javaAnnotationStringArg extracts the value of a `key = "value"` argument
// from an annotation's raw inner-parens text (the EdgeAnnotated Meta
// "args"), e.g. javaAnnotationStringArg(`name = "ChargeCard"`, "name") ==
// "ChargeCard". Matched on a word boundary so a "name" lookup does not
// match "namePrefix". Returns "" when the key is absent or unquoted.
func javaAnnotationStringArg(args, key string) string {
	for i := 0; i+len(key) <= len(args); i++ {
		if args[i:i+len(key)] != key {
			continue
		}
		if i > 0 {
			if b := args[i-1]; b != ' ' && b != ',' && b != '(' {
				continue
			}
		}
		j := i + len(key)
		for j < len(args) && args[j] == ' ' {
			j++
		}
		if j >= len(args) || args[j] != '=' {
			continue
		}
		rest := args[j+1:]
		q := strings.IndexByte(rest, '"')
		if q < 0 {
			return ""
		}
		rest = rest[q+1:]
		end := strings.IndexByte(rest, '"')
		if end < 0 {
			return ""
		}
		return rest[:end]
	}
	return ""
}

// capitalizeASCII upper-cases the first rune of s (Temporal's Java SDK
// derives an activity's default type from the method name with the first
// letter capitalized).
func capitalizeASCII(s string) string {
	if s == "" {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[size:]
}

// javaMethodCanonicalName computes the canonical Temporal name a Java
// method-level annotation registers under, so the resolver keys it off the
// same string a matching Go registration would use:
//   - an explicit @XxxMethod(name = "...") always wins;
//   - an activity method defaults to its name with the first letter
//     capitalized (the Java SDK default activity type);
//   - signal / query / update / workflow methods default to the bare
//     method name (signal/query/update names match by string at runtime;
//     a workflow's type is usually the interface name, handled in Phase 3).
func javaMethodCanonicalName(role, methodName, args string) string {
	if explicit := javaAnnotationStringArg(args, "name"); explicit != "" {
		return explicit
	}
	if role == "activity" {
		return capitalizeASCII(methodName)
	}
	return methodName
}

// normaliseTemporalKind collapses the seven role tags down to the two
// kinds that drive stub-call lookup ("activity" / "workflow"). Signal
// / query / update handlers are workflow methods, not separate kinds.
func normaliseTemporalKind(role string) string {
	switch role {
	case "workflow", "signal", "query", "update":
		return "workflow"
	default:
		return "activity"
	}
}

// stampTemporalRole writes `temporal_role` and `temporal_name` into a
// node's Meta. Idempotent: re-stamping the same role is a no-op. When
// a previously-stamped node is re-stamped with a different role the
// new role wins (the resolver runs as a full recompute, so this lets
// the latest registration take precedence).
func stampTemporalRole(g graph.Store, n *graph.Node, role, name string) {
	if n == nil || role == "" {
		return
	}
	// Skip the write-back entirely when the role + name are already what
	// we would stamp. ResolveTemporalCalls is a full recompute that runs
	// on every incremental edit, so without this guard every Temporal-role
	// node is re-AddNode'd (a serialised single-row write on the sqlite
	// backend) on every pass even when nothing changed. The common steady
	// state — re-running the pass after an unrelated edit — then costs no
	// node writes at all.
	if cur, _ := n.Meta["temporal_role"].(string); cur == role {
		if name == "" {
			return
		}
		if curName, _ := n.Meta["temporal_name"].(string); curName == name {
			return
		}
	}
	if n.Meta == nil {
		n.Meta = map[string]any{}
	}
	n.Meta["temporal_role"] = role
	if name != "" {
		n.Meta["temporal_name"] = name
	}
	// Round-trip the stamp back through the store. On the in-memory
	// backend n is canonical so this is an idempotent re-insert; on disk
	// backends n is a per-call GetNode/AllNodes reconstruction,
	// so without the write-back temporal_role/temporal_name would be
	// discarded the moment this pass returns. ResolveTemporalCalls runs
	// from RunGlobalGraphPasses, which can execute after the bulk-load
	// buffer is flushed, so the in-place mutation is not otherwise
	// captured. Matches reach / coverage / blame / releases / churn.
	g.AddNode(n)
}

// pickGoTemporalTarget selects the Go function or method that a
// `worker.Register*(F)` call refers to from a name-matched candidate
// set. The register call lives at `caller`; the function `F` is
// either declared in the same file or imported. The search order is:
//
//  1. Same-file function whose name matches.
//  2. Same-repo function whose name matches.
//  3. Unique workspace-wide function whose name matches.
//
// Returns nil when no unambiguous match exists. The candidate list
// MUST be pre-filtered to Name == registered name (FindNodesByNames
// already does that); this helper applies the Go-kind and language
// gates plus the locality tie-break.
func pickGoTemporalTarget(candidates []*graph.Node, caller *graph.Node) *graph.Node {
	if caller == nil {
		return nil
	}
	var sameFile, sameRepo, all []*graph.Node
	for _, n := range candidates {
		if n == nil {
			continue
		}
		if n.Language != "go" {
			continue
		}
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		all = append(all, n)
		if caller.RepoPrefix != "" && n.RepoPrefix == caller.RepoPrefix {
			sameRepo = append(sameRepo, n)
		}
		if n.FilePath == caller.FilePath {
			sameFile = append(sameFile, n)
		}
	}
	if len(sameFile) == 1 {
		return sameFile[0]
	}
	if len(sameRepo) == 1 {
		return sameRepo[0]
	}
	if len(all) == 1 {
		return all[0]
	}
	return nil
}

// pickGoTypeNode selects the Go type node a `RegisterActivities(&T{})`
// struct registration refers to, from a name-matched candidate set, using
// the same same-file → same-repo → unique-overall locality tie-break as
// pickGoTemporalTarget. Returns nil when no unambiguous Go type matches.
func pickGoTypeNode(candidates []*graph.Node, caller *graph.Node) *graph.Node {
	if caller == nil {
		return nil
	}
	var sameFile, sameRepo, all []*graph.Node
	for _, n := range candidates {
		if n == nil || n.Language != "go" {
			continue
		}
		if n.Kind != graph.KindType && n.Kind != graph.KindInterface {
			continue
		}
		all = append(all, n)
		if caller.RepoPrefix != "" && n.RepoPrefix == caller.RepoPrefix {
			sameRepo = append(sameRepo, n)
		}
		if n.FilePath == caller.FilePath {
			sameFile = append(sameFile, n)
		}
	}
	if len(sameFile) == 1 {
		return sameFile[0]
	}
	if len(sameRepo) == 1 {
		return sameRepo[0]
	}
	if len(all) == 1 {
		return all[0]
	}
	return nil
}

// exportedGoMethodsOfType returns the exported Go method nodes of a type,
// found via the EdgeMemberOf in-edges the Go extractor emits from each
// method to its receiver type. Used to promote every method of a
// RegisterActivities(&Struct{}) registration to a temporal activity.
func exportedGoMethodsOfType(g graph.Store, typeNode *graph.Node) []*graph.Node {
	if typeNode == nil {
		return nil
	}
	var memberIDs []string
	for _, ie := range g.GetInEdges(typeNode.ID) {
		if ie == nil || ie.Kind != graph.EdgeMemberOf || ie.From == "" {
			continue
		}
		memberIDs = append(memberIDs, ie.From)
	}
	if len(memberIDs) == 0 {
		return nil
	}
	members := g.GetNodesByIDs(memberIDs)
	var out []*graph.Node
	for _, id := range memberIDs {
		m := members[id]
		if m == nil || m.Language != "go" || m.Kind != graph.KindMethod {
			continue
		}
		if !isExportedGoName(m.Name) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// isExportedGoName reports whether a Go identifier is exported (its first
// rune is an uppercase letter) — Temporal registers only exported methods
// of a struct passed to RegisterActivities.
func isExportedGoName(name string) bool {
	if name == "" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(name)
	return unicode.IsUpper(r)
}

// buildConstDerefMap resolves the names of string constants used as
// Temporal dispatch identifiers to their literal values, read from the
// queryable constant_values sidecar. Returns name → value for every name
// that is a string const with a single unambiguous value across the
// workspace; a name with conflicting values in different files (e.g. the
// same const name defined twice with different literals) is dropped so a
// dereference is never a wrong guess. Returns nil when the backend does
// not implement ConstantValueReader.
//
// Const-to-const aliases (Cat 3, `const ALIAS = RealName`) are followed: a
// requested name whose const node carries no literal but a Meta["const_ref"]
// is chased to the referenced const's literal value by a bounded fixpoint,
// so an ALL_CAPS dispatch name that is itself an alias (ALIAS = REAL =
// "lit") still dereferences. Cycles never progress and are dropped at the cap.
func buildConstDerefMap(g graph.Store, names []string) map[string]string {
	if len(names) == 0 {
		return nil
	}
	reader, _ := g.(graph.ConstantValueReader)
	nameSet := make(map[string]struct{}, len(names))
	for _, n := range names {
		nameSet[n] = struct{}{}
	}
	// Expand the lookup set with every const_ref hop reachable from the
	// requested names so an alias chain's terminal literal is fetched too.
	// Bounded passes keep a malicious / cyclic graph from looping. The same
	// sweep collects Java string-constant fields (see javaFieldVals).
	idToName := map[string]string{}
	aliasRef := map[string]string{} // name → referenced const name
	// javaFieldVals collects Java string constants (`static final String`),
	// which the Java extractor emits as KindField nodes carrying the literal
	// on Meta["value"] rather than through the queryable constant sidecar.
	// Ingesting them lets a Java invoker const-ref dispatch
	// (`invoker.invokeAsync(Constants.X, …)`) deref X cross-language to the
	// registered Go workflow / activity.
	javaFieldVals := map[string]string{}
	resolveNames := func(want map[string]struct{}) {
		uniq := make([]string, 0, len(want))
		for n := range want {
			uniq = append(uniq, n)
		}
		for name, cands := range g.FindNodesByNames(uniq) {
			for _, n := range cands {
				if n == nil {
					continue
				}
				switch {
				case n.Kind == graph.KindConstant || n.Kind == graph.KindFunction || n.Kind == graph.KindMethod:
					idToName[n.ID] = name
					if ref, ok := n.Meta["const_ref"].(string); ok && ref != "" {
						aliasRef[name] = ref
					}
					// A Java `static final String` is classified KindConstant
					// but carries its literal on Meta["value"] (the Java
					// extractor stamps it there, not in the queryable constant
					// sidecar). Ingest it so a Java const-ref Temporal dispatch
					// derefs cross-language, just like the KindField case below.
					if n.Language == "java" {
						if v, ok := n.Meta["value"].(string); ok && v != "" {
							javaFieldVals[name] = v
						}
					}
				case n.Kind == graph.KindField && n.Language == "java":
					if v, ok := n.Meta["value"].(string); ok && v != "" {
						javaFieldVals[name] = v
					}
				}
			}
		}
	}
	resolveNames(nameSet)
	for pass := 0; pass < 8; pass++ {
		pending := map[string]struct{}{}
		for _, ref := range aliasRef {
			if _, known := nameSet[ref]; !known {
				pending[ref] = struct{}{}
			}
		}
		if len(pending) == 0 {
			break
		}
		for n := range pending {
			nameSet[n] = struct{}{}
		}
		resolveNames(pending)
	}
	var constIDs []string
	for id := range idToName {
		constIDs = append(constIDs, id)
	}

	out := make(map[string]string)
	ambiguous := map[string]struct{}{}
	ingest := func(name, v string) {
		if name == "" || v == "" {
			return
		}
		if _, dropped := ambiguous[name]; dropped {
			return
		}
		if existing, seen := out[name]; seen && existing != v {
			delete(out, name)
			ambiguous[name] = struct{}{}
			return
		}
		out[name] = v
	}

	if reader != nil && len(constIDs) > 0 {
		if vals, err := reader.ConstantValuesByNodeIDs(constIDs); err == nil {
			for id, v := range vals {
				ingest(idToName[id], v)
			}
		}
	}
	// Java string-constant fields share the SAME dereference index, with the
	// same workspace-wide ambiguity rule as Go string constants.
	for name, v := range javaFieldVals {
		ingest(name, v)
	}

	// Collapse alias chains against the literal map by a bounded fixpoint: an
	// alias name with no literal of its own inherits its referent's resolved
	// literal. Cycles never progress and are dropped at the cap.
	for pass := 0; pass < 8; pass++ {
		progressed := false
		for name, ref := range aliasRef {
			if _, has := out[name]; has {
				continue
			}
			if v, ok := out[ref]; ok && v != "" {
				out[name] = v
				progressed = true
			}
		}
		if !progressed {
			break
		}
	}

	if len(out) == 0 {
		return nil
	}
	return out
}

// buildJavaMethodViews materialises two indexes over every Java
// method node in the graph: methodsByFile groups nodes whose Meta has
// NO "receiver" (interface methods, per the Java extractor's
// convention); methodsByReceiver groups nodes whose Meta carries a
// non-empty receiver. One NodesByKind scan replaces the N AllNodes()
// passes the old collectJavaInterfaceMethods + methodsOfJavaType
// helpers ran inside the per-interface propagation loop.
//
// ifaceCount == 0 is a fast no-op; with no tagged interfaces the
// indexes are unused so we skip the scan.
func buildJavaMethodViews(g graph.Store, ifaceCount int) (map[string][]*graph.Node, map[string][]*graph.Node) {
	if ifaceCount == 0 {
		return nil, nil
	}
	methodsByFile := map[string][]*graph.Node{}
	methodsByReceiver := map[string][]*graph.Node{}
	for n := range g.NodesByKind(graph.KindMethod) {
		if n == nil || n.Language != "java" {
			continue
		}
		recv, _ := n.Meta["receiver"].(string)
		if recv == "" {
			methodsByFile[n.FilePath] = append(methodsByFile[n.FilePath], n)
		} else {
			methodsByReceiver[recv] = append(methodsByReceiver[recv], n)
		}
	}
	return methodsByFile, methodsByReceiver
}

// collectJavaInterfaceMethodsFromIndex returns the interface's method
// nodes — flat KindMethod nodes in the interface's file whose
// StartLine sits inside the interface's line range. Consumes the
// methodsByFile view built by buildJavaMethodViews so the scan is
// O(methods in this file) rather than O(every node).
func collectJavaInterfaceMethodsFromIndex(iface *graph.Node, methodsByFile map[string][]*graph.Node) []*graph.Node {
	if iface == nil {
		return nil
	}
	var out []*graph.Node
	for _, n := range methodsByFile[iface.FilePath] {
		if n.StartLine < iface.StartLine || (iface.EndLine > 0 && n.StartLine > iface.EndLine) {
			continue
		}
		out = append(out, n)
	}
	return out
}

// methodsOfJavaTypeFromIndex returns the method nodes whose
// Meta["receiver"] matches the type's name (or the receiver-suffix
// shape on the class node's ID). Consumes the methodsByReceiver view
// built by buildJavaMethodViews so the scan is O(methods of this
// receiver) rather than O(every node).
func methodsOfJavaTypeFromIndex(t *graph.Node, methodsByReceiver map[string][]*graph.Node) []*graph.Node {
	if t == nil {
		return nil
	}
	out := methodsByReceiver[t.Name]
	// Honour the legacy id-suffix tie-break: a class node's id is
	// `<filePath>::<ClassName>`; a method whose receiver matches that
	// trailing component is still a member even when the receiver
	// Meta carries a fully-qualified name.
	for recv, candidates := range methodsByReceiver {
		if recv == t.Name {
			continue
		}
		if !strings.HasSuffix(t.ID, "::"+recv) {
			continue
		}
		out = append(out, candidates...)
	}
	return out
}

// resolveTemporalExecutorFields rewrites the dispatch name of a method
// stub that reads a receiver field to the string literal the struct was
// constructed with at its (possibly remote) construction site.
//
// PURPOSE — when a struct method reads a field to dispatch an activity/workflow
// (e.g. `workflow.ExecuteActivity(ctx, e.ActivityName)`) and the struct was
// constructed with a string literal for that field
// (`ActivityExecutor{ActivityName: "ChargeCard"}`), this pass rewrites the
// method stub's `temporal_name` from the field name to that literal, so the
// main resolver sweep lands it on the registered handler. The dispatch happens
// IN the method, so the call edge stays anchored to the method (get_callers on
// the activity surfaces the dispatching method, not the construction site).
// RATIONALE — two-edge join: the method-stub edge carries (recvType, field)
// from the dispatch site; the executor-field marker edge carries
// (type, field, value) from the construction site. The join key is
// `recvType::fieldName`. The rewrite is re-derived from the marker edges on
// every pass (never relying on the prior pass's mutation surviving), so it is
// recompute-safe under the full-recompute contract of ResolveTemporalCalls:
// the parser re-emits the stub with `temporal_name=<field>` on reindex, and
// this pass re-applies the literal before the main sweep runs. A
// recvType::field with conflicting construction-site literals is left
// unresolved — same unique-or-nothing policy as the const-deref join.
// KEYWORDS — temporal, executor-field, resolver
// When shared is set, sharedCalls carries the pass's temporal.* candidates
// in their current form and replaces both phases' whole-stream decodes (the
// two phases filter disjoint via tags, so one slice serves both).
func resolveTemporalExecutorFields(g graph.Store, sharedCalls []*graph.Edge, shared bool) {
	// Phase 1: collect the method-stub edges that read a receiver field,
	// grouped by `recvType::field`.
	type dispatch struct {
		stubs []*graph.Edge
	}
	byField := map[string]*dispatch{}
	stubStream := g.EdgesByKind(graph.EdgeCalls)
	if shared {
		stubStream = frameworkEdgeSeq(sharedCalls)
	}
	for e := range stubStream {
		if e == nil || e.Meta == nil {
			continue
		}
		if v, _ := e.Meta["via"].(string); v != "temporal.stub" {
			continue
		}
		field, _ := e.Meta["temporal_name_field"].(string)
		rtype, _ := e.Meta["temporal_recv_type"].(string)
		kind, _ := e.Meta["temporal_kind"].(string)
		if field == "" || rtype == "" || kind == "" {
			continue
		}
		key := rtype + "::" + field
		d := byField[key]
		if d == nil {
			d = &dispatch{}
			byField[key] = d
		}
		d.stubs = append(d.stubs, e)
	}
	if len(byField) == 0 {
		return
	}

	// Phase 2: for each executor-field marker edge, collect the literal
	// construction value per `recvType::field`. A key with conflicting
	// values across construction sites is ambiguous and dropped.
	valByField := map[string]string{}
	ambiguous := map[string]struct{}{}
	markerStream := g.EdgesByKind(graph.EdgeCalls)
	if shared {
		markerStream = frameworkEdgeSeq(sharedCalls)
	}
	for e := range markerStream {
		if e == nil || e.Meta == nil || e.From == "" {
			continue
		}
		if v, _ := e.Meta["via"].(string); v != "temporal.executor-field" {
			continue
		}
		rtype, _ := e.Meta["executor_type"].(string)
		field, _ := e.Meta["executor_field"].(string)
		value, _ := e.Meta["executor_value"].(string)
		if rtype == "" || field == "" || value == "" {
			continue
		}
		key := rtype + "::" + field
		if _, ok := byField[key]; !ok {
			continue
		}
		if existing, seen := valByField[key]; seen && existing != value {
			ambiguous[key] = struct{}{}
			continue
		}
		valByField[key] = value
	}
	for key := range ambiguous {
		delete(valByField, key)
	}

	// Phase 3: rewrite each matched method stub's dispatch name to the
	// construction literal. e.To is left for the main sweep to recompute
	// from the new temporal_name; temporal_name_field / temporal_recv_type
	// are preserved as the join key for the next full-recompute pass.
	for key, value := range valByField {
		d := byField[key]
		if d == nil {
			continue
		}
		for _, e := range d.stubs {
			e.Meta["temporal_name"] = value
			e.Meta["temporal_via_executor"] = true
		}
	}
}

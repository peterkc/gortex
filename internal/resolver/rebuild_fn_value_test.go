package resolver

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func rebuildFnValueTestNode(id, name, file string) *graph.Node {
	return &graph.Node{ID: id, Name: name, FilePath: file, Kind: graph.KindFunction, Language: "typescript"}
}

func rebuildFnValueTestSite(from *graph.Node, name string, line int) *graph.Edge {
	return &graph.Edge{From: from.ID, To: graph.FnValuePlaceholderMarker + name, Kind: graph.EdgeReferences, FilePath: from.FilePath, Line: line,
		Meta: map[string]any{"via": fnValueCandidateVia, metaFnValueName: name}}
}

func rebuildFnValueTestSelection(t *testing.T) FrameworkSynthesizerSelection {
	t.Helper()
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthFnValue})
	require.NoError(t, err)
	return selection
}

func TestRebuildFnValueSQLiteOracle(t *testing.T) {
	const file, other = "site.ts", "other.ts"
	caller := rebuildFnValueTestNode("caller", "register", file)
	local := rebuildFnValueTestSite(caller, "local", 1)
	member := rebuildFnValueTestSite(caller, "member", 2)
	member.Meta["fn_ref_recv_hint"], member.Meta["fn_ref_form"] = "Owner", "method_reference"
	global := rebuildFnValueTestSite(caller, "global", 3)
	global.Meta["fn_value_ungated"] = true
	first := rebuildFnValueTestNode("a-local", "local", file)
	second := rebuildFnValueTestNode("z-local", "local", file)
	m1 := rebuildFnValueTestNode("m1", "member", other)
	m2 := rebuildFnValueTestNode("m2", "member", other)
	for _, m := range []*graph.Node{m1, m2} {
		m.Kind = graph.KindMethod
		m.Meta = map[string]any{"receiver": "Owner"}
	}
	remote := rebuildFnValueTestNode("remote", "global", other)
	prototype := rebuildFnValueTestNode("proto", "global", other)
	prototype.Meta = map[string]any{"prototype": true}
	static := rebuildFnValueTestNode("static", "global", other)
	static.Meta = map[string]any{"scope_static": true}
	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(file, caller.Language, []*graph.Node{caller, second, first}, local, member, global),
		rebuildEventFacts(other, caller.Language, []*graph.Node{m2, m1, remote, prototype, static}),
	}
	b, native := openRebuildEventStores(t, facts...)
	require.Equal(t, 4, ResolveFnValueCallbacks(native))
	want := rebuildEventSQLiteRows(native)
	require.Contains(t, want.edges, local)
	for _, e := range want.edges {
		if e.Meta["via"] != fnValueRegistrationVia {
			continue
		}
		switch e.Line {
		case 1:
			require.Equal(t, first.ID, e.To, "SQLite node order is not receipt order")
		case 2:
			require.Contains(t, []string{m1.ID, m2.ID}, e.To)
			require.EqualValues(t, 2, e.Meta["overload_set"])
			require.Equal(t, "method_reference", e.Meta["fn_ref_form"])
			require.Equal(t, 0.6, e.Confidence)
		case 3:
			require.Equal(t, remote.ID, e.To)
			require.Equal(t, 0.45, e.Confidence)
		}
	}
	selection := rebuildFnValueTestSelection(t)
	for _, expected := range []uint64{4, 0} {
		count, err := RunRebuildFnValueCallbacks(t.Context(), b, rebuildGRPCReceipts(facts...), selection)
		require.NoError(t, err)
		require.Equal(t, expected, count)
		require.Equal(t, want, rebuildEventSavedRows(t, b))
	}
}

func TestRebuildFnValueReceiptEntrant(t *testing.T) {
	const file = "site.ts"
	caller := rebuildFnValueTestNode("caller", "register", file)
	winner := rebuildFnValueTestNode("z", "target", file)
	entrant := rebuildFnValueTestNode("a", "other", file)
	call := rebuildFnValueTestSite(caller, "target", 1)
	facts := rebuildEventFacts(file, caller.Language, []*graph.Node{caller, winner, entrant}, call)
	b := openRebuildGRPCBuild(t, facts)
	before := rebuildEventSavedRows(t, b)
	raw := *entrant
	raw.Name = winner.Name
	facts.Nodes = []*graph.Node{caller, winner, &raw}
	_, oracle := openRebuildEventStores(t, facts)
	require.Equal(t, 1, ResolveFnValueCallbacks(oracle))
	want := fnValueRegistration(call, "target", entrant.ID, 0.6, graph.OriginASTInferred, 1)
	require.Contains(t, rebuildEventSQLiteRows(oracle).edges, want)
	count, err := RunRebuildFnValueCallbacks(t.Context(), b, rebuildGRPCReceipts(facts), rebuildFnValueTestSelection(t))
	requireRebuildFnValueRefusal(t, b, before, call, count, err)
}

func TestRebuildFnValueSharedRawClaim(t *testing.T) {
	const file = "site.ts"
	caller := rebuildFnValueTestNode("caller", "register", file)
	target := rebuildFnValueTestNode("target", "target", file)
	call := rebuildFnValueTestSite(caller, target.Name, 1)
	facts := rebuildEventFacts(file, caller.Language, []*graph.Node{caller, target}, call)
	b, oracle := openRebuildEventStores(t, facts)
	settled := *call
	settled.To = "settled"
	require.NoError(t, b.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{{Edge: &settled, OldTo: call.To}}))
	b.AddEdge(call)
	oracle.AddEdge(&settled)
	require.Equal(t, 1, ResolveFnValueCallbacks(oracle))
	before := rebuildEventSavedRows(t, b)
	count, err := RunRebuildFnValueCallbacks(t.Context(), b, rebuildGRPCReceipts(facts), rebuildFnValueTestSelection(t))
	requireRebuildFnValueRefusal(t, b, before, call, count, err)
}

func TestRebuildFnValueUnprovedGlobalTie(t *testing.T) {
	const file = "site.ts"
	caller := rebuildFnValueTestNode("caller", "register", file)
	call := rebuildFnValueTestSite(caller, "target", 1)
	call.Meta["skip_gate"] = true
	a := rebuildFnValueTestNode("a", "target", "one.ts")
	z := rebuildFnValueTestNode("z", "target", "two.ts")
	site := rebuildEventFacts(file, caller.Language, []*graph.Node{caller}, call)
	first := rebuildEventFacts(a.FilePath, a.Language, []*graph.Node{a})
	second := rebuildEventFacts(z.FilePath, z.Language, []*graph.Node{z})
	b, oracle := openRebuildEventStores(t, site, first, second)
	require.Zero(t, ResolveFnValueCallbacks(oracle))
	_, one := openRebuildEventStores(t, site, first)
	require.Equal(t, 1, ResolveFnValueCallbacks(one), "one of the optional definitions can change the result")
	before := rebuildEventSavedRows(t, b)
	count, err := RunRebuildFnValueCallbacks(t.Context(), b, rebuildGRPCReceipts(site), rebuildFnValueTestSelection(t))
	requireRebuildFnValueRefusal(t, b, before, call, count, err)
}

func TestRebuildFnValueCapsRoleRows(t *testing.T) {
	const file = "site.ts"
	padding := strings.Repeat("x", 16<<20)
	for _, name := range []string{"target", "unrelated"} {
		t.Run(name, func(t *testing.T) {
			caller := rebuildFnValueTestNode("caller", "register", file)
			call := rebuildFnValueTestSite(caller, "target", 1)
			candidate := rebuildFnValueTestNode("large", name, file)
			candidate.Meta = map[string]any{"padding": padding}
			known := rebuildFnValueTestNode("z", "target", file)
			facts := rebuildEventFacts(file, caller.Language, []*graph.Node{caller, candidate, known}, call)
			b, oracle := openRebuildEventStores(t, facts)
			before := rebuildEventSavedRows(t, b)
			require.Equal(t, 1, ResolveFnValueCallbacks(oracle))
			count, err := RunRebuildFnValueCallbacks(t.Context(), b, rebuildGRPCReceipts(facts), rebuildFnValueTestSelection(t))
			if name == "target" {
				require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
				require.Zero(t, count)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 1, count)
			require.Equal(t, rebuildEventSQLiteRows(oracle), rebuildEventSavedRows(t, b))
		})
	}
}

func requireRebuildFnValueRefusal(t *testing.T, b *store_pebble.BuildStore, before rebuildEventRows, site *graph.Edge, count uint64, err error) {
	t.Helper()
	var refused *store_pebble.RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(site)}, refused)
	require.Zero(t, count)
	require.Equal(t, before, rebuildEventSavedRows(t, b))
}

func TestRebuildFnValueSelfReceiverProof(t *testing.T) {
	const file, remote = "site.ts", "other.ts"
	caller := rebuildFnValueTestNode("caller", "register", file)
	caller.Meta = map[string]any{"receiver": "A"}
	rawCaller := *caller
	rawCaller.Meta = map[string]any{"receiver": "B"}
	call := rebuildFnValueTestSite(caller, "member", 1)
	call.Meta["fn_ref_recv_hint"] = "<self>"
	for _, name := range []string{"member", "unrelated"} {
		t.Run(name, func(t *testing.T) {
			method := rebuildFnValueTestNode("method", name, remote)
			method.Kind = graph.KindMethod
			method.Meta = map[string]any{"receiver": "B"}
			liveSite := rebuildEventFacts(file, caller.Language, []*graph.Node{caller}, call)
			declarations := rebuildEventFacts(remote, caller.Language, []*graph.Node{method})
			b, native := openRebuildEventStores(t, liveSite, declarations)
			require.Zero(t, ResolveFnValueCallbacks(native))
			before := rebuildEventSavedRows(t, b)
			rawSite := rebuildEventFacts(file, caller.Language, []*graph.Node{&rawCaller}, call)
			_, rawNative := openRebuildEventStores(t, rawSite)
			require.Zero(t, ResolveFnValueCallbacks(rawNative))
			count, err := RunRebuildFnValueCallbacks(t.Context(), b, rebuildGRPCReceipts(rawSite), rebuildFnValueTestSelection(t))
			if name == "member" {
				_, mixed := openRebuildEventStores(t, rawSite, declarations)
				require.Equal(t, 1, ResolveFnValueCallbacks(mixed))
				requireRebuildFnValueRefusal(t, b, before, call, count, err)
				return
			}
			require.NoError(t, err)
			require.Zero(t, count)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildFnValueUniqueMemberTie(t *testing.T) {
	const file, remote = "site.ts", "other.ts"
	caller := rebuildFnValueTestNode("caller", "register", file)
	for _, skip := range []bool{false, true} {
		call := rebuildFnValueTestSite(caller, "member", 1)
		call.Meta["fn_ref_recv_hint"], call.Meta["skip_gate"] = "Owner", skip
		var methods []*graph.Node
		for _, id := range []string{"a", "b", "c"} {
			method := rebuildFnValueTestNode(id, "member", remote)
			method.Kind = graph.KindMethod
			method.Meta = map[string]any{"receiver": "Owner"}
			methods = append(methods, method)
		}
		site := rebuildEventFacts(file, caller.Language, []*graph.Node{caller}, call)
		declarations := rebuildEventFacts(remote, caller.Language, methods)
		b, native := openRebuildEventStores(t, site, declarations)
		before := rebuildEventSavedRows(t, b)
		receipt := rebuildEventFacts(remote, caller.Language, methods[:2])
		count, err := RunRebuildFnValueCallbacks(t.Context(), b, rebuildGRPCReceipts(site, receipt), rebuildFnValueTestSelection(t))
		if skip {
			require.Zero(t, ResolveFnValueCallbacks(native))
			require.NoError(t, err)
			require.Zero(t, count)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
			continue
		}
		// Ordinary member references retain the full overload set, unlike
		// skip-gate's unique-member lookup.
		require.Equal(t, 3, ResolveFnValueCallbacks(native))
		requireRebuildFnValueRefusal(t, b, before, call, count, err)
	}
}

func TestRebuildFnValueSettledPredecessor(t *testing.T) {
	const file = "site.ts"
	caller := rebuildFnValueTestNode("caller", "register", file)
	target := rebuildFnValueTestNode("target", "target", file)
	call := rebuildFnValueTestSite(caller, target.Name, 1)
	facts := rebuildEventFacts(file, caller.Language, []*graph.Node{caller, target}, call)
	b, native := openRebuildEventStores(t, facts)
	settled := *call
	settled.To = "predecessor-target"
	move := []graph.EdgeReindex{{Edge: &settled, OldTo: call.To}}
	require.NoError(t, b.ApplyLinkReindexes(t.Context(), move))
	native.ReindexEdges(move)
	require.Zero(t, ResolveFnValueCallbacks(native))
	before := rebuildEventSavedRows(t, b)
	count, err := RunRebuildFnValueCallbacks(t.Context(), b, rebuildGRPCReceipts(facts), rebuildFnValueTestSelection(t))
	require.NoError(t, err)
	require.Zero(t, count)
	require.Equal(t, before, rebuildEventSavedRows(t, b))
	require.Equal(t, rebuildEventSQLiteRows(native), before)
}

func TestRebuildFnValueSQLiteOracleGateTiers(t *testing.T) {
	const file, remote = "site.ts", "other.ts"
	for _, tc := range []struct {
		name, receiver string
		member, proto  bool
		skip           bool
		confidence     float64
		origin         string
	}{
		{name: "self member", receiver: "<self>", member: true, confidence: 0.85, origin: graph.OriginASTResolved},
		{name: "skip unique member", receiver: "Owner", member: true, skip: true, confidence: 0.5, origin: graph.OriginASTInferred},
		{name: "skip global", skip: true, confidence: 0.5, origin: graph.OriginASTInferred},
		{name: "skip prototype", proto: true, skip: true, confidence: 0.5, origin: graph.OriginASTInferred},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caller := rebuildFnValueTestNode("caller", "register", file)
			caller.Meta = map[string]any{"receiver": "Owner"}
			call := rebuildFnValueTestSite(caller, "target", 1)
			call.Meta["fn_ref_recv_hint"], call.Meta["skip_gate"] = tc.receiver, tc.skip
			target := rebuildFnValueTestNode("target", "target", remote)
			if tc.member {
				target.Kind = graph.KindMethod
				target.Meta = map[string]any{"receiver": "Owner"}
			}
			if tc.proto {
				target.Meta = map[string]any{"prototype": true}
			}
			nodes := []*graph.Node{target}
			if tc.member && tc.skip {
				// The global fallback must be ambiguous; only Owner's member can win.
				other := rebuildFnValueTestNode("other-target", target.Name, remote)
				other.Kind = graph.KindMethod
				other.Meta = map[string]any{"receiver": "Other"}
				nodes = append(nodes, other)
			}
			site := rebuildEventFacts(file, caller.Language, []*graph.Node{caller}, call)
			decls := rebuildEventFacts(remote, target.Language, nodes)
			build, native := openRebuildEventStores(t, site, decls)
			require.Equal(t, 1, ResolveFnValueCallbacks(native))
			want := rebuildEventSQLiteRows(native)
			require.Contains(t, want.edges, fnValueRegistration(call, "target", target.ID, tc.confidence, tc.origin, 1))
			for _, expected := range []uint64{1, 0} {
				count, err := RunRebuildFnValueCallbacks(t.Context(), build, rebuildGRPCReceipts(site, decls), rebuildFnValueTestSelection(t))
				require.NoError(t, err)
				require.Equal(t, expected, count)
				require.Equal(t, want, rebuildEventSavedRows(t, build))
			}
		})
	}
}

package resolver

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildCSharpPartialSQLiteConsumers(t *testing.T) {
	const a, b, goFile, java = "A.cs", "B.cs", "rpc.go", "Publisher.java"
	const canonical, fragment, server, contract = a + "::Greeter", b + "::Greeter", a + "::GreeterServer", a + "::Contract"
	node := func(id, name, file, language string, kind graph.NodeKind, line int) *graph.Node {
		return &graph.Node{ID: id, Name: name, FilePath: file, Language: language, Kind: kind, StartLine: line}
	}
	first := node(canonical, "Greeter", a, "csharp", graph.KindType, 2)
	second := node(fragment, "Greeter", b, "csharp", graph.KindType, 2)
	iface := node(server, "GreeterServer", a, "csharp", graph.KindInterface, 3)
	structural := node(contract, "Contract", a, "csharp", graph.KindInterface, 4)
	structural.Meta = map[string]any{"methods": []string{"Hello", "Run"}}
	parent := node(a+"::hello", "Hello", a, "csharp", graph.KindMethod, 5)
	run := node(a+"::run", "Run", a, "csharp", graph.KindMethod, 6)
	file := node(a, a, a, "csharp", graph.KindFile, 0)
	handler := node(goFile+"::hello", "Hello", goFile, "go", graph.KindMethod, 3)
	caller := node(goFile+"::call", "call", goFile, "go", graph.KindFunction, 4)
	grpc := &graph.Edge{From: caller.ID, To: grpcStubPlaceholder("Greeter", "Hello"), Kind: graph.EdgeCalls, FilePath: goFile, Line: 7,
		Meta: map[string]any{"via": "grpc.stub", "grpc_service": "Greeter", "grpc_method": "Hello"}}
	publish := node(java+"::publish", "publish", java, "java", graph.KindMethod, 10)
	listener := node(java+"::on", "on", java, "java", graph.KindMethod, 12)
	listener.Meta = map[string]any{"spring_listener_type": "GreeterServer"}
	spring := &graph.Edge{From: publish.ID, To: publish.ID, Kind: graph.EdgeCalls, FilePath: java, Line: 10,
		Meta: map[string]any{"via": springEventVia, "spring_event_type": "Greeter"}}
	arc := &graph.Edge{From: fragment, To: server, Kind: graph.EdgeImplements, FilePath: b, Line: 2, Origin: graph.OriginASTResolved}
	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(a, "csharp", []*graph.Node{file, first, iface, structural, parent, run}, diIfacesMarker(a, "Greeter"),
			&graph.Edge{From: parent.ID, To: server, Kind: graph.EdgeMemberOf, FilePath: a, Line: 5},
			&graph.Edge{From: run.ID, To: canonical, Kind: graph.EdgeMemberOf, FilePath: a, Line: 6}),
		rebuildEventFacts(b, "csharp", []*graph.Node{second}, arc),
		rebuildEventFacts(goFile, "go", []*graph.Node{handler, caller}, grpc,
			&graph.Edge{From: handler.ID, To: fragment, Kind: graph.EdgeMemberOf, FilePath: goFile, Line: 3}),
		rebuildEventFacts(java, "java", []*graph.Node{publish, listener}, spring),
	}
	build, native := openRebuildEventStores(t, facts...)
	ctx := t.Context()
	receipts := rebuildGRPCReceipts(facts...)
	r := New(native)
	r.mergeCSharpPartialTypes()
	require.NoError(t, runRebuildCSharpPartial(ctx, build, receipts))
	folded := rebuildEventSQLiteRows(native)
	require.Equal(t, folded, rebuildEventSavedRows(t, build))
	require.NoError(t, runRebuildCSharpPartial(ctx, build, receipts))
	require.Equal(t, folded, rebuildEventSavedRows(t, build), "a settled fold can replay")

	// Implements and Overrides execute on the real folded inputs. Their
	// C# candidates are native positives and deferred in the private pass.
	require.Equal(t, 1, r.InferImplements())
	require.NoError(t, RunRebuildImplements(ctx, build, receipts))
	require.Equal(t, folded, rebuildEventSavedRows(t, build))
	require.Equal(t, 1, r.InferOverrides())
	require.NoError(t, RunRebuildOverrides(ctx, build, receipts))
	require.Equal(t, folded, rebuildEventSavedRows(t, build))

	selection, err := NewFrameworkSynthesizerSelection([]string{SynthGRPCStub, SynthSpringEvent})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(ctx, build.RebuildView(), selection)
	require.NoError(t, err)
	require.True(t, frozen.allows(SynthGRPCStub))
	require.True(t, frozen.allows(SynthSpringEvent))
	require.Equal(t, 1, ResolveGRPCStubCalls(native))
	grpcStats, err := RunRebuildGRPC(ctx, build, receipts, frozen)
	require.NoError(t, err)
	require.Equal(t, store_pebble.RebuildGRPCStats{Stubs: 1, Resolved: 1, Moves: 1}, grpcStats)
	require.Equal(t, 1, ResolveSpringEventCalls(native))
	springStats, err := RunRebuildSpringEvents(ctx, build, receipts, frozen)
	require.NoError(t, err)
	require.Equal(t, store_pebble.RebuildSpringEventStats{Stubs: 1, Resolved: 1, Moves: 1}, springStats)
	want := rebuildEventSQLiteRows(native)
	want.edges = slices.DeleteFunc(want.edges, func(e *graph.Edge) bool {
		return (e.Kind == graph.EdgeImplements && e.From == canonical && e.To == contract) ||
			(e.Kind == graph.EdgeOverrides && e.From == handler.ID && e.To == parent.ID)
	})
	require.Equal(t, want, rebuildEventSavedRows(t, build))
	require.Equal(t, 1, ResolveCSharpDIImplementedInterfaces(native), "canonical beats the husk and inferred interfaces do not register")
	for _, e := range native.GetOutEdges(a) {
		if e.Meta[graph.MetaDIBinding] == graph.DIBindingUseClass {
			require.Equal(t, "GreeterServer", e.Meta[graph.MetaDIProvidesFor])
		}
	}
}

func TestRebuildCSharpPartialMainFrontier(t *testing.T) {
	const a, b = "A.cs", "B.cs"
	first := &graph.Node{ID: a + "::T", Name: "T", FilePath: a, Language: "csharp", Kind: graph.KindType}
	second := &graph.Node{ID: b + "::T", Name: "T", FilePath: b, Language: "csharp", Kind: graph.KindType}
	for _, pending := range []bool{false, true} {
		ctx, build, opts := frontierFixture(t, 0)
		l := rebuildEventFacts(a, "csharp", []*graph.Node{first})
		r := rebuildEventFacts(b, "csharp", []*graph.Node{second})
		if pending {
			r.Edges = []*graph.Edge{{From: second.ID, To: "unresolved::missing", Kind: graph.EdgeCalls, FilePath: b, Line: 2}}
		}
		require.NoError(t, build.AddParsedFile("", l))
		require.NoError(t, build.AddParsedFile("", r))
		native := openRebuildTemporalOracle(t, l, r)
		_, err := New(native).ResolveAllContext(ctx)
		require.NoError(t, err)
		// Deferred main rows still count toward the original frontier.
		require.NoError(t, RunRebuildMain(ctx, build, opts, RebuildMainInputs{
			Receipts:      rebuildGRPCReceipts(l, r),
			SelectCallers: func([]*graph.Edge) ([]*graph.Edge, error) { return nil, nil },
		}))
		n, err := build.RebuildView().Node(ctx, second.ID)
		require.NoError(t, err)
		require.Equal(t, native.GetNode(second.ID).Meta[metaPartialMergedInto], n.Meta[metaPartialMergedInto])
		if pending {
			require.Equal(t, first.ID, n.Meta[metaPartialMergedInto])
		} else {
			require.NotContains(t, n.Meta, metaPartialMergedInto)
		}
	}
}

func TestRebuildCSharpPartialSQLiteFragmentOrder(t *testing.T) {
	const a, b, c = "A.cs", "B.cs", "B.cs!.cs"
	const canonical, target = a + "::T", a + "::IFoo"
	fixture := func(path string, line int) store_pebble.ParseFacts {
		n := &graph.Node{ID: path + "::T", Name: "T", FilePath: path, Language: "csharp", Kind: graph.KindType}
		e := &graph.Edge{From: n.ID, To: target, Kind: graph.EdgeImplements, FilePath: path, Line: line, Origin: graph.OriginASTResolved}
		return rebuildEventFacts(path, "csharp", []*graph.Node{n}, e)
	}
	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(a, "csharp", []*graph.Node{
			{ID: canonical, Name: "T", FilePath: a, Language: "csharp", Kind: graph.KindType},
			{ID: target, Name: "IFoo", FilePath: a, Language: "csharp", Kind: graph.KindInterface},
		}),
		fixture(b, 3),
		fixture(c, 5),
	}
	build, native := openRebuildEventStores(t, facts...)
	New(native).mergeCSharpPartialTypes()
	want := rebuildEventSQLiteRows(native)
	require.Len(t, want.edges, 1)
	require.Equal(t, canonical, want.edges[0].From)
	require.Equal(t, c, want.edges[0].FilePath, "B.cs!.cs::T comes before B.cs::T in the SQLite scan")
	require.Equal(t, 5, want.edges[0].Line)
	require.NoError(t, runRebuildCSharpPartial(t.Context(), build, rebuildGRPCReceipts(facts...)))
	require.Equal(t, want, rebuildEventSavedRows(t, build))
}

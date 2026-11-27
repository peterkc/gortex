package resolver

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func ifaceRebuildNode(file, name, language string, kind graph.NodeKind) *graph.Node {
	return &graph.Node{ID: file + "::" + name, Name: name, FilePath: file, Language: language, Kind: kind}
}

func ifaceRebuildEdge(from *graph.Node, to string, kind graph.EdgeKind, line int) *graph.Edge {
	return &graph.Edge{From: from.ID, To: to, Kind: kind, FilePath: from.FilePath, Line: line}
}

func ifaceRebuildSelection(t *testing.T, build *store_pebble.BuildStore) FrameworkSynthesizerSelection {
	t.Helper()
	sel, err := NewFrameworkSynthesizerSelection([]string{SynthCSharpIfaceDispatch})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), build.RebuildView(), sel)
	require.NoError(t, err)
	return frozen
}

func TestRebuildCSharpIfaceDispatchSQLiteFolded(t *testing.T) {
	const a, b, goFile = "A.cs", "B.cs", "client.go"
	iface := ifaceRebuildNode(a, "IThing", "csharp", graph.KindInterface)
	anchor := ifaceRebuildNode(a, "Run", "csharp", graph.KindMethod)
	anchor.Meta = map[string]any{"iface_member": true}
	canonical := ifaceRebuildNode(a, "Thing", "csharp", graph.KindType)
	husk := ifaceRebuildNode(b, "Thing", "csharp", graph.KindType)
	method := ifaceRebuildNode(b, "Run", "csharp", graph.KindMethod)
	caller := ifaceRebuildNode(goFile, "invoke", "go", graph.KindFunction)
	arc := ifaceRebuildEdge(husk, "unresolved::IThing", graph.EdgeImplements, 1)
	member := ifaceRebuildEdge(method, canonical.ID, graph.EdgeMemberOf, 2)
	call := ifaceRebuildEdge(caller, anchor.ID, graph.EdgeCalls, 1)
	weak := ifaceRebuildEdge(caller, anchor.ID, graph.EdgeCalls, 2)
	weak.Origin = graph.OriginTextMatched
	weakSibling := ifaceRebuildEdge(method, anchor.ID, graph.EdgeCalls, 3)
	weakSibling.Origin = graph.OriginTextMatched
	incumbent := ifaceRebuildEdge(caller, method.ID, graph.EdgeCalls, 4)
	incumbent.Origin = graph.OriginASTResolved
	blocked := ifaceRebuildEdge(caller, anchor.ID, graph.EdgeCalls, 4)
	inferred := ifaceRebuildNode(b, "FalseMatch", "csharp", graph.KindType)
	inferredMember := ifaceRebuildNode(b, "Run2", "csharp", graph.KindMethod)
	inferredMember.Name = "Run"
	inferredArc := ifaceRebuildEdge(inferred, iface.ID, graph.EdgeImplements, 5)
	inferredArc.Meta = map[string]any{"via": MetaViaMethodSetInference}
	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(a, "csharp", []*graph.Node{iface, anchor, canonical}, ifaceRebuildEdge(anchor, iface.ID, graph.EdgeMemberOf, 1)),
		rebuildEventFacts(b, "csharp", []*graph.Node{husk, method, inferred, inferredMember}, arc, member, weakSibling, inferredArc,
			ifaceRebuildEdge(inferredMember, inferred.ID, graph.EdgeMemberOf, 6)),
		rebuildEventFacts(goFile, "go", []*graph.Node{caller}, call, weak, incumbent, blocked),
	}
	build, native := openRebuildEventStores(t, facts...)
	receipts := rebuildGRPCReceipts(facts...)
	New(native).mergeCSharpPartialTypes()
	require.NoError(t, runRebuildCSharpPartial(t.Context(), build, receipts))
	frozen := ifaceRebuildSelection(t, build)
	require.True(t, frozen.allows(SynthCSharpIfaceDispatch))
	require.Equal(t, 1, ResolveCSharpInterfaceDispatch(native))
	want := rebuildEventSQLiteRows(native)
	for _, count := range []uint64{1, 0} {
		adds, err := RunRebuildCSharpIfaceDispatch(t.Context(), build, receipts, frozen)
		require.NoError(t, err)
		require.Equal(t, count, adds)
		require.Equal(t, want, rebuildEventSavedRows(t, build))
	}
	require.Contains(t, want.edges, csharpIfaceDispatchEdge(call, method.ID, iface.ID, 1))
	require.Contains(t, want.edges, incumbent)
}

func TestRebuildCSharpIfaceDispatchSQLiteCap(t *testing.T) {
	const file = "Types.cs"
	for _, count := range []int{csharpIfaceDispatchCap - 1, csharpIfaceDispatchCap} {
		iface := ifaceRebuildNode(file, "IFoo", "csharp", graph.KindInterface)
		anchor := ifaceRebuildNode(file, "anchor", "csharp", graph.KindMethod)
		anchor.Name, anchor.Meta = "Run", map[string]any{"iface_member": true}
		owner := ifaceRebuildNode(file, "Foo", "csharp", graph.KindType)
		caller := ifaceRebuildNode(file, "caller", "csharp", graph.KindMethod)
		nodes := []*graph.Node{iface, anchor, owner, caller}
		edges := []*graph.Edge{
			ifaceRebuildEdge(anchor, iface.ID, graph.EdgeMemberOf, 1),
			ifaceRebuildEdge(owner, iface.ID, graph.EdgeImplements, 1),
			ifaceRebuildEdge(caller, anchor.ID, graph.EdgeCalls, 1),
		}
		for i := range count {
			method := ifaceRebuildNode(file, fmt.Sprintf("Run%d", i), "csharp", graph.KindMethod)
			method.Name = "Run"
			nodes = append(nodes, method)
			edges = append(edges, ifaceRebuildEdge(method, owner.ID, graph.EdgeMemberOf, i+2))
		}
		facts := rebuildEventFacts(file, "csharp", nodes, edges...)
		build, native := openRebuildEventStores(t, facts)
		want := ResolveCSharpInterfaceDispatch(native)
		if count+1 > csharpIfaceDispatchCap {
			require.Zero(t, want)
		} else {
			require.Equal(t, count, want)
		}
		adds, err := RunRebuildCSharpIfaceDispatch(t.Context(), build, rebuildGRPCReceipts(facts), ifaceRebuildSelection(t, build))
		require.NoError(t, err)
		require.Equal(t, uint64(want), adds)
		require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, build))
		if count == csharpIfaceDispatchCap {
			before := rebuildEventSavedRows(t, build)
			missing := facts
			missing.Nodes = nodes[:len(nodes)-1]
			adds, err := RunRebuildCSharpIfaceDispatch(t.Context(), build, rebuildGRPCReceipts(missing), ifaceRebuildSelection(t, build))
			require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
			require.Zero(t, adds)
			require.Equal(t, before, rebuildEventSavedRows(t, build))
		}
	}
}

func TestRebuildCSharpIfaceDispatchRefusesOnlyFreeUnprovedOutput(t *testing.T) {
	const file = "Types.cs"
	iface := ifaceRebuildNode(file, "IFoo", "csharp", graph.KindInterface)
	anchor := ifaceRebuildNode(file, "Run", "csharp", graph.KindMethod)
	anchor.Meta = map[string]any{"iface_member": true}
	impl := ifaceRebuildNode(file, "Foo", "csharp", graph.KindType)
	method := ifaceRebuildNode(file, "Foo.Run", "csharp", graph.KindMethod)
	method.Name = "Run"
	caller := ifaceRebuildNode(file, "caller", "csharp", graph.KindMethod)
	arc := ifaceRebuildEdge(impl, iface.ID, graph.EdgeExtends, 1)
	call := ifaceRebuildEdge(caller, anchor.ID, graph.EdgeCalls, 1)
	incumbent := ifaceRebuildEdge(caller, method.ID, graph.EdgeCalls, 1)
	for _, isOccupied := range []bool{false, true} {
		edges := []*graph.Edge{arc, call, ifaceRebuildEdge(anchor, iface.ID, graph.EdgeMemberOf, 1),
			ifaceRebuildEdge(method, impl.ID, graph.EdgeMemberOf, 1)}
		if isOccupied {
			edges = append(edges, incumbent)
		}
		facts := rebuildEventFacts(file, "csharp", []*graph.Node{iface, impl, anchor, method, caller}, edges...)
		build, native := openRebuildEventStores(t, facts)
		before := rebuildEventSavedRows(t, build)
		receipt := facts
		receipt.Edges = slices.Clone(edges[1:])
		adds, err := RunRebuildCSharpIfaceDispatch(t.Context(), build, rebuildGRPCReceipts(receipt), ifaceRebuildSelection(t, build))
		if isOccupied {
			require.NoError(t, err)
			require.Zero(t, ResolveCSharpInterfaceDispatch(native))
		} else {
			require.ErrorIs(t, err, store_pebble.ErrRebuildCSharpIfaceDispatch)
			var refusal *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refusal)
			require.Equal(t, graph.EdgeIdentityFor(incumbent), refusal.First)
		}
		require.Zero(t, adds)
		require.Equal(t, before, rebuildEventSavedRows(t, build))
	}
}

func TestRebuildCSharpIfaceDispatchFrozenSelection(t *testing.T) {
	build := openRebuildGRPCBuild(t)
	frozen := ifaceRebuildSelection(t, build)
	require.False(t, frozen.allows(SynthCSharpIfaceDispatch))
	build.AddNode(ifaceRebuildNode("Type.cs", "IFoo", "csharp", graph.KindInterface))
	require.True(t, ifaceRebuildSelection(t, build).allows(SynthCSharpIfaceDispatch))
	adds, err := RunRebuildCSharpIfaceDispatch(t.Context(), build, nil, frozen)
	require.NoError(t, err)
	require.Zero(t, adds)
}

func TestRebuildCSharpIfaceDispatchReceiptFamily(t *testing.T) {
	const file = "Types.cs"
	iface := ifaceRebuildNode(file, "IFoo", "csharp", graph.KindInterface)
	anchor := ifaceRebuildNode(file, "Run", "csharp", graph.KindMethod)
	anchor.Meta = map[string]any{"iface_member": true}
	owner := ifaceRebuildNode(file, "Foo", "csharp", graph.KindType)
	method := ifaceRebuildNode(file, "Foo.Run", "csharp", graph.KindMethod)
	method.Name = anchor.Name
	competitor := ifaceRebuildNode(file, "Other", "csharp", graph.KindInterface)
	caller := ifaceRebuildNode(file, "call", "csharp", graph.KindFunction)
	call := ifaceRebuildEdge(caller, anchor.ID, graph.EdgeCalls, 3)
	arcs := []*graph.Edge{
		ifaceRebuildEdge(owner, "unresolved::IFoo", graph.EdgeImplements, 1),
		ifaceRebuildEdge(anchor, iface.ID, graph.EdgeMemberOf, 1),
		ifaceRebuildEdge(method, owner.ID, graph.EdgeMemberOf, 1),
		call,
	}
	facts := rebuildEventFacts(file, "csharp", []*graph.Node{iface, anchor, owner, method, competitor, caller}, arcs...)
	for _, role := range []string{"method name", "unresolved parent"} {
		t.Run(role, func(t *testing.T) {
			other := facts
			other.Nodes = slices.Clone(facts.Nodes)
			if role == "method name" {
				changed := *method
				changed.Name = "Other"
				other.Nodes[3] = &changed
			} else {
				ambiguous := *competitor
				ambiguous.Name = iface.Name
				other.Nodes[4] = &ambiguous
			}
			// Check both removal and entrance to the family.
			for _, inputs := range [][2]store_pebble.ParseFacts{{facts, other}, {other, facts}} {
				build := openRebuildGRPCBuild(t, inputs[0])
				before := rebuildEventSavedRows(t, build)
				adds, err := RunRebuildCSharpIfaceDispatch(t.Context(), build, rebuildGRPCReceipts(inputs[1]), ifaceRebuildSelection(t, build))
				require.Zero(t, adds)
				var refused *store_pebble.RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refused)
				require.EqualValues(t, 1, refused.Count)
				require.Equal(t, graph.EdgeIdentityFor(csharpIfaceDispatchEdge(call, method.ID, iface.ID, 1)), refused.First)
				require.Equal(t, before, rebuildEventSavedRows(t, build))
			}
		})
	}
}

// U5 records ties between native families at one physical Call key.
func TestRebuildCSharpIfaceDispatchTwoFamiliesOneKey(t *testing.T) {
	const file = "Types.cs"
	a := ifaceRebuildNode(file, "IA", "csharp", graph.KindInterface)
	z := ifaceRebuildNode(file, "IZ", "csharp", graph.KindInterface)
	anchor := ifaceRebuildNode(file, "Run", "csharp", graph.KindMethod)
	anchor.Meta = map[string]any{"iface_member": true}
	owner := ifaceRebuildNode(file, "Foo", "csharp", graph.KindType)
	method := ifaceRebuildNode(file, "Foo.Run", "csharp", graph.KindMethod)
	method.Name = anchor.Name
	caller := ifaceRebuildNode(file, "call", "csharp", graph.KindFunction)
	call := ifaceRebuildEdge(caller, anchor.ID, graph.EdgeCalls, 3)
	facts := rebuildEventFacts(file, "csharp", []*graph.Node{z, a, anchor, owner, method, caller},
		ifaceRebuildEdge(anchor, z.ID, graph.EdgeMemberOf, 1),
		ifaceRebuildEdge(anchor, a.ID, graph.EdgeMemberOf, 1),
		ifaceRebuildEdge(owner, z.ID, graph.EdgeImplements, 2),
		ifaceRebuildEdge(owner, a.ID, graph.EdgeImplements, 2),
		ifaceRebuildEdge(method, owner.ID, graph.EdgeMemberOf, 2), call)
	build := openRebuildGRPCBuild(t, facts)
	adds, err := RunRebuildCSharpIfaceDispatch(t.Context(), build, rebuildGRPCReceipts(facts), ifaceRebuildSelection(t, build))
	require.NoError(t, err)
	require.EqualValues(t, 1, adds)
	require.Contains(t, rebuildEventSavedRows(t, build).edges, csharpIfaceDispatchEdge(call, method.ID, a.ID, 1))
}

func TestRebuildCSharpIfaceDispatchParentNodeCap(t *testing.T) {
	const file = "Types.cs"
	iface := ifaceRebuildNode(file, "IFoo", "csharp", graph.KindInterface)
	foreign := ifaceRebuildNode("foreign/Types.cs", iface.Name, iface.Language, iface.Kind)
	foreign.RepoPrefix = "foreign"
	anchor := ifaceRebuildNode(file, "Run", "csharp", graph.KindMethod)
	anchor.Meta = map[string]any{"iface_member": true}
	owner := ifaceRebuildNode(file, "Foo", "csharp", graph.KindType)
	method := ifaceRebuildNode(file, "Foo.Run", "csharp", graph.KindMethod)
	method.Name = anchor.Name
	caller := ifaceRebuildNode(file, "call", "csharp", graph.KindFunction)
	call := ifaceRebuildEdge(caller, anchor.ID, graph.EdgeCalls, 3)
	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(file, "csharp", []*graph.Node{iface, anchor, owner, method, caller},
			ifaceRebuildEdge(owner, "unresolved::IFoo", graph.EdgeImplements, 1),
			ifaceRebuildEdge(anchor, iface.ID, graph.EdgeMemberOf, 1),
			ifaceRebuildEdge(method, owner.ID, graph.EdgeMemberOf, 1), call),
		rebuildEventFacts(foreign.FilePath, foreign.Language, []*graph.Node{foreign}),
	}
	padding := strings.Repeat("x", 16<<20)
	for _, tc := range []struct {
		name   string
		large  *graph.Node
		refuse bool
	}{
		{"same repository", iface, true},
		{"other repository", foreign, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			build := openRebuildGRPCBuild(t, facts...)
			frozen := ifaceRebuildSelection(t, build)
			require.True(t, frozen.allows(SynthCSharpIfaceDispatch))
			large := *tc.large
			large.Meta = map[string]any{"padding": padding}
			build.AddNode(&large)
			before := rebuildEventSavedRows(t, build).edges
			adds, err := RunRebuildCSharpIfaceDispatch(t.Context(), build, rebuildGRPCReceipts(facts...), frozen)
			after := rebuildEventSavedRows(t, build).edges
			if tc.refuse {
				require.ErrorIs(t, err, store_pebble.ErrRebuildCSharpIfaceDispatch)
				require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
				require.ErrorContains(t, err, "csharp interface dispatch parent node")
				require.Zero(t, adds)
				require.Equal(t, before, after)
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 1, adds)
			require.Contains(t, after, csharpIfaceDispatchEdge(call, method.ID, iface.ID, 1))
		})
	}
}

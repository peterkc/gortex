package indexer

import (
	"iter"
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildDirectCapabilitiesNativeParity(t *testing.T) {
	const (
		path     = "work.ts"
		declPath = "declarations.ts"
		from     = path + "::work"
		field    = declPath + "::State.value"
		env      = "cfg::env::MODE"
		embedded = "cfg::viper::nested.cfg::env::MODE"
		dotted   = "exec.Command"
		scheme   = "stdlib::os/exec::Command"
		process  = "string::process::exec.Command"
	)
	work := &graph.Node{ID: from, Kind: graph.KindFunction, Name: "work", FilePath: path, Language: "typescript"}
	configs := []*graph.Node{
		{ID: env, Kind: graph.KindConfigKey, Name: "MODE", FilePath: path, Language: "typescript"},
		{ID: embedded, Kind: graph.KindConfigKey, Name: "nested.cfg::env::MODE", FilePath: path, Language: "typescript"},
	}
	// The matcher spellings name live declarations, not unresolved or external stubs.
	decls := []*graph.Node{
		{ID: field, Kind: graph.KindField, Name: "value", FilePath: declPath, Language: "typescript"},
		{ID: dotted, Kind: graph.KindFunction, Name: "Command", FilePath: declPath, Language: "typescript"},
		{ID: scheme, Kind: graph.KindFunction, Name: "Command", FilePath: declPath, Language: "typescript"},
	}
	workNodes := append([]*graph.Node{work}, configs...)
	nodes := slices.Concat(workNodes, decls)
	raw := []*graph.Edge{
		{From: from, To: env, Kind: graph.EdgeReadsConfig, FilePath: path, Line: 9, Origin: graph.OriginASTInferred,
			Confidence: 0.75, Meta: map[string]any{"method": "env"}},
		{From: from, To: env, Kind: graph.EdgeReadsConfig, FilePath: path, Line: 3, Origin: graph.OriginASTInferred},
		{From: from, To: embedded, Kind: graph.EdgeReadsConfig, FilePath: path, Line: 4, Origin: graph.OriginASTInferred},
		// Read wins despite a later line, and the original To wins before process folding.
		{From: from, To: field, Kind: graph.EdgeWrites, FilePath: path, Line: 5, Origin: graph.OriginASTResolved},
		{From: from, To: field, Kind: graph.EdgeReads, FilePath: path, Line: 10, Origin: graph.OriginASTResolved},
		{From: from, To: scheme, Kind: graph.EdgeCalls, FilePath: path, Line: 6, Origin: graph.OriginASTResolved},
		{From: from, To: dotted, Kind: graph.EdgeCalls, FilePath: path, Line: 11, Origin: graph.OriginASTResolved,
			ConfidenceLabel: "EXTRACTED", Meta: map[string]any{"argument": "demo"}},
	}
	old := []*graph.Edge{
		{From: from, To: env, Kind: graph.EdgeReadsEnv, FilePath: path, Line: 3, Origin: graph.OriginASTInferred,
			Meta: map[string]any{"prior": "overwrite"}},
		{From: from, To: env, Kind: graph.EdgeReadsEnv, FilePath: path, Line: 20, Origin: graph.OriginASTInferred,
			Meta: map[string]any{"prior": "preserve"}},
	}
	for _, order := range []string{"forward", "reverse"} {
		t.Run(order, func(t *testing.T) {
			edges := slices.Clone(raw)
			if order == "reverse" {
				slices.Reverse(edges)
			}
			before := slices.Concat(edges, old)
			native := rebuildDirectCapabilitiesNative(t, nodes, before, [3]int{2, 1, 1})
			require.ElementsMatch(t, []*graph.Edge{
				{From: from, To: env, Kind: graph.EdgeReadsEnv, FilePath: path, Line: 3, Origin: graph.OriginASTResolved},
				{From: from, To: embedded, Kind: graph.EdgeReadsEnv, FilePath: path, Line: 4, Origin: graph.OriginASTResolved},
				old[1],
			}, slices.Collect(native.EdgesByKind(graph.EdgeReadsEnv)))
			require.Equal(t, []*graph.Edge{{From: from, To: field, Kind: graph.EdgeAccessesField, FilePath: path, Line: 10,
				Origin: graph.OriginASTResolved, Meta: map[string]any{"access": "read"}}},
				slices.Collect(native.EdgesByKind(graph.EdgeAccessesField)))
			require.Equal(t, []*graph.Edge{{From: from, To: process, Kind: graph.EdgeExecutesProcess, FilePath: path, Line: 11,
				Origin: graph.OriginASTInferred}}, slices.Collect(native.EdgesByKind(graph.EdgeExecutesProcess)))
			require.Equal(t, &graph.Node{ID: process, Kind: graph.KindString, Name: dotted,
				Meta: map[string]any{"context": "process", "mechanism": dotted}}, native.GetNode(process))

			build, _ := openRebuildTestFileStore(t)
			// The deferred receipt's call loses even though it has an earlier
			// line. It cannot change the elected row and must not refuse it.
			var owned, deferred []*graph.Edge
			for _, e := range edges {
				if e.Kind == graph.EdgeCalls && e.To == scheme {
					deferred = append(deferred, e)
				} else {
					owned = append(owned, e)
				}
			}
			receipts := seedRebuildTestSymbolReceipts(t, build, []store_pebble.ParseFacts{
				{EffectiveLanguage: "typescript", File: graph.FileMetaRow{FilePath: path, NodeCount: len(workNodes)},
					Nodes: workNodes, Edges: owned},
				{EffectiveLanguage: "typescript", File: graph.FileMetaRow{FilePath: declPath, NodeCount: len(decls)}, Nodes: decls},
				{EffectiveLanguage: "python", File: graph.FileMetaRow{FilePath: "loser.py"}, Edges: deferred},
			})
			build.AddBatch(nil, old)
			slices.SortFunc(before, compareRebuildTestFileEdgeKeys)
			requireRebuildTestFileRows(t, build, nodes, before)
			require.NoError(t, runRebuildDirectCapabilities(t.Context(), build, receipts))
			want := slices.Clone(native.AllEdges())
			slices.SortFunc(want, compareRebuildTestFileEdgeKeys)
			requireRebuildTestFileRows(t, build, native.AllNodes(), want)
		})
	}
}

func TestRebuildDirectCapabilitiesRefusesUnprovedWinner(t *testing.T) {
	const path, from = "work.go", "work.go::work"
	const early, late = "cfg::env::A", "cfg::env::Z"
	nodes := []*graph.Node{
		{ID: from, Kind: graph.KindFunction, Name: "work", FilePath: path, Language: "go"},
		{ID: early, Kind: graph.KindConfigKey, Name: "A", FilePath: path, Language: "go"},
		{ID: late, Kind: graph.KindConfigKey, Name: "Z", FilePath: path, Language: "go"},
	}
	proved := []*graph.Edge{
		{From: from, To: early, Kind: graph.EdgeReadsConfig, FilePath: path, Line: 5, Origin: graph.OriginASTInferred},
		{From: from, To: late, Kind: graph.EdgeReadsConfig, FilePath: path, Line: 9, Origin: graph.OriginASTInferred},
	}
	// This row has a lower site in the supported file, but the deferred receipt produced it.
	unproved := &graph.Edge{From: from, To: late, Kind: graph.EdgeReadsConfig, FilePath: path, Line: 2, Origin: graph.OriginASTInferred}
	// A proved group sorts first. Refusal must not overwrite even this earlier row.
	old := &graph.Edge{From: from, To: early, Kind: graph.EdgeReadsEnv, FilePath: path, Line: 5,
		Origin: graph.OriginASTInferred, Meta: map[string]any{"prior": "keep on refusal"}}
	withoutDeferred := append(slices.Clone(proved), old)
	before := append(slices.Clone(withoutDeferred), unproved)
	for _, tc := range []struct {
		edges []*graph.Edge
		line  int
	}{{withoutDeferred, 9}, {before, 2}} {
		native := rebuildDirectCapabilitiesNative(t, nodes, tc.edges, [3]int{2, 0, 0})
		require.ElementsMatch(t, []*graph.Edge{
			{From: from, To: early, Kind: graph.EdgeReadsEnv, FilePath: path, Line: 5, Origin: graph.OriginASTResolved},
			{From: from, To: late, Kind: graph.EdgeReadsEnv, FilePath: path, Line: tc.line, Origin: graph.OriginASTResolved},
		}, slices.Collect(native.EdgesByKind(graph.EdgeReadsEnv)))
	}

	build, _ := openRebuildTestFileStore(t)
	receipts := seedRebuildTestSymbolReceipts(t, build, []store_pebble.ParseFacts{
		{EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: path, NodeCount: len(nodes)}, Nodes: nodes, Edges: proved},
		{EffectiveLanguage: "python", File: graph.FileMetaRow{FilePath: "probe.py"}, Edges: []*graph.Edge{unproved}},
	})
	build.AddBatch(nil, []*graph.Edge{old})
	slices.SortFunc(before, compareRebuildTestFileEdgeKeys)
	requireRebuildTestFileRows(t, build, nodes, before)
	err := runRebuildDirectCapabilities(t.Context(), build, receipts)
	var refusal *store_pebble.RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, uint64(1), refusal.Count)
	reply := buildRefusal(err)
	require.Equal(t, uint8(36), reply.Refusal)
	require.Equal(t, uint64(1), reply.UnknownProvenance)
	requireRebuildTestFileRows(t, build, nodes, before)
}

func TestRebuildCapabilitiesResolvedCallReceipts(t *testing.T) {
	const path, caller, field = "p/m.go", "p/m.go::T.Tick", "p/m.go::T.n"
	for _, route := range []struct {
		name, callee string
		counts       [3]int
	}{
		{"direct", "execSync", [3]int{0, 1, 0}},
		{"receiver", "Bump", [3]int{0, 0, 2}},
	} {
		t.Run(route.name, func(t *testing.T) {
			callee := path + "::T." + route.callee
			nodes := []*graph.Node{
				{ID: caller, Name: "Tick", Kind: graph.KindMethod, FilePath: path, Language: "go", Meta: map[string]any{"receiver": "T"}},
				{ID: callee, Name: route.callee, Kind: graph.KindMethod, FilePath: path, Language: "go", Meta: map[string]any{"receiver": "T"}},
				{ID: field, Name: "n", Kind: graph.KindField, FilePath: path, Language: "go", Meta: map[string]any{"receiver": "T"}},
			}
			call := &graph.Edge{From: caller, To: callee, Kind: graph.EdgeCalls, FilePath: path, Line: 7,
				Origin: graph.OriginASTResolved, Meta: map[string]any{"recv_self": true}}
			before := []*graph.Edge{call}
			var base []*graph.Edge
			if route.name == "receiver" {
				base = []*graph.Edge{{From: callee, To: field, Kind: graph.EdgeWrites, FilePath: path, Line: 3,
					Origin: graph.OriginASTResolved}}
				before = append(before, base...)
			}
			slices.SortFunc(before, compareRebuildTestFileEdgeKeys)
			native := rebuildDirectCapabilitiesNative(t, nodes, before, route.counts)
			want := native.AllEdges()
			slices.SortFunc(want, compareRebuildTestFileEdgeKeys)
			for _, tc := range []struct {
				name, admitted, deferred string
				accept                   bool
			}{
				{"decoy", "unresolved::*.Noop", "", false},
				{"converse", "unresolved::*." + route.callee, "unresolved::*.Noop", true},
				{"namespaced", "repo::unresolved::*." + route.callee, "", true},
				{"structured-decoy", "unresolved::extern::other::" + route.callee, "", false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					build, _ := openRebuildTestFileStore(t)
					build.AddBatch(nodes, before)
					requireRebuildTestFileRows(t, build, nodes, before)
					raw := *call
					raw.To = tc.admitted
					// Deliberately supply receipts independently of the live graph.
					// Only the named receipt evidence changes between cases.
					var receipts iter.Seq2[store_pebble.ParseFacts, error] = func(yield func(store_pebble.ParseFacts, error) bool) {
						if !yield(store_pebble.ParseFacts{EffectiveLanguage: "go",
							File: graph.FileMetaRow{FilePath: path}, Nodes: nodes, Edges: append(slices.Clone(base), &raw)}, nil) {
							return
						}
						if tc.deferred != "" {
							decoy := raw
							decoy.To = tc.deferred
							yield(store_pebble.ParseFacts{EffectiveLanguage: "python", File: graph.FileMetaRow{FilePath: "decoy.py"},
								Edges: []*graph.Edge{&decoy}}, nil)
						}
					}
					ctx := t.Context()
					var err error
					if route.name == "receiver" {
						_, err = build.RebuildCapabilities(ctx, receipts, processExecMechanism, rebuildProcessImport, rebuildReceiverRules(), 1<<20)
					} else {
						err = runRebuildDirectCapabilities(ctx, build, receipts)
					}
					if tc.accept {
						require.NoError(t, err)
						requireRebuildTestFileRows(t, build, native.AllNodes(), want)
						return
					}
					var refusal *store_pebble.RebuildUnknownProvenanceError
					require.ErrorAs(t, err, &refusal)
					require.Equal(t, uint64(1), refusal.Count)
					if route.name == "receiver" {
						require.Equal(t, graph.EdgeIdentity{From: caller, To: field, Kind: graph.EdgeAccessesField, FilePath: path, Line: 7}, refusal.First)
					}
					requireRebuildTestFileRows(t, build, nodes, before)
				})
			}
		})
	}
}

func rebuildDirectCapabilitiesNative(t *testing.T, nodes []*graph.Node, edges []*graph.Edge, counts [3]int) *graph.Graph {
	t.Helper()
	g := graph.New()
	ownedNodes := make([]*graph.Node, 0, len(nodes))
	for _, n := range nodes {
		copy := *n
		copy.Meta = maps.Clone(n.Meta)
		ownedNodes = append(ownedNodes, &copy)
	}
	ownedEdges := make([]*graph.Edge, 0, len(edges))
	for _, e := range edges {
		copy := *e
		copy.Meta = maps.Clone(e.Meta)
		ownedEdges = append(ownedEdges, &copy)
	}
	g.AddBatch(ownedNodes, ownedEdges)
	require.ElementsMatch(t, nodes, g.AllNodes())
	require.ElementsMatch(t, edges, g.AllEdges())
	reads, processes, fields, err := synthesizeCapabilityEdges(g)
	require.NoError(t, err)
	require.Equal(t, counts, [3]int{reads, processes, fields})
	return g
}

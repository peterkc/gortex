package indexer

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestRebuildArgOfNativeOrder(t *testing.T) {
	// Lock the oracle to complete param_of key order (rule b). The reverse
	// native ingest must actually pick a different winner before it can prove
	// that the working store does not follow insertion order.
	locked := rebuildArgOfNativeRows(t, false)
	rebuildArgOfNativeRows(t, true)

	for _, order := range []struct {
		name    string
		reverse bool
	}{
		{"forward", false},
		{"reverse", true},
	} {
		t.Run(order.name, func(t *testing.T) {
			ctx := t.Context()
			caller, callee, args := rebuildArgOfOrderFixture(order.reverse)
			build, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "working"), store_pebble.FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, build.Close()) })
			build.AddBatch(caller.Nodes, args)
			build.AddBatch(callee.Nodes, callee.Edges)

			read := func() []*graph.Edge {
				t.Helper()
				var rows []*graph.Edge
				require.NoError(t, build.RebuildView().VisitEdges(ctx, func(e *graph.Edge) error {
					rows = append(rows, e)
					return nil
				}))
				return rows
			}
			require.ElementsMatch(t, slices.Concat(args, callee.Edges), read(), "settled input rows")
			require.NoError(t, build.RebuildView().VisitLinkPendingPages(ctx, 1, 1<<20, func(edges []*graph.Edge) error {
				t.Fatalf("settled input has a pending resolver page: %v", edges)
				return nil
			}))
			idx := newTestIndexer(graph.New())
			t.Cleanup(idx.Close)
			store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
			job := rebuildFileStages{recipes: map[string]rebuildDigest{"go": {1}, "typescript": {2}}}
			census := &rebuildExtractorCensus{byPath: make(map[string]string)}
			for _, base := range []store_pebble.ParseFacts{caller, callee} {
				input := rebuildInput{Path: base.File.FilePath, Language: base.EffectiveLanguage}
				job.inputs = append(job.inputs, input)
				census.byPath[input.Path] = base.EffectiveLanguage
				key := input.parseKey(idx.rebuildExtractorRecipe(job.recipes[input.Language], input))
				require.NoError(t, store.write(key, encodeRebuildTestParseFacts(t, base)))
			}
			receipts := idx.rebuildDataflowReceipts(ctx, job, store, census)
			require.NoError(t, runRebuildArgOf(ctx, build, receipts))
			got := read()
			require.ElementsMatch(t, locked, got, "all saved rows, including payloads and unchanged guards")
			require.NoError(t, runRebuildArgOf(ctx, build, receipts))
			require.Equal(t, got, read(), "replay must not change any saved row")
		})
	}
}

func rebuildArgOfNativeRows(t *testing.T, reverse bool) []*graph.Edge {
	t.Helper()
	caller, callee, args := rebuildArgOfOrderFixture(reverse)
	native, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, native.Close()) })
	require.NoError(t, native.AddBatchChecked(caller.Nodes, args))
	require.NoError(t, native.AddBatchChecked(callee.Nodes, nil))
	// One committed insert per candidate forces the SQLite row-id order.
	for _, param := range callee.Edges {
		require.NoError(t, native.AddBatchChecked(nil, []*graph.Edge{param}))
	}
	before := slices.Concat(args, callee.Edges)
	require.ElementsMatch(t, before, native.AllEdges(), "native input (reverse=%t)", reverse)
	idx := &Indexer{graph: native}
	idx.materializeDataflowParams()

	want := slices.Clone(before)
	lifted := *args[0]
	lifted.To = "callee.ts::Sink#param:a@0"
	if reverse {
		lifted.To = "callee.ts::Sink#param:z@0"
	}
	want[0] = &lifted
	rows := native.AllEdges()
	require.ElementsMatch(t, want, rows, "native first winner %q (reverse=%t)", lifted.To, reverse)
	return rows
}

func rebuildArgOfOrderFixture(reverse bool) (caller, callee store_pebble.ParseFacts, settled []*graph.Edge) {
	const goFile, tsFile = "caller.go", "callee.ts"
	const callerID, owner = goFile + "::Caller", tsFile + "::Sink"
	const source = callerID + "#local:input@+2"
	caller = store_pebble.ParseFacts{
		File:              graph.FileMetaRow{FilePath: goFile, NodeCount: 2},
		EffectiveLanguage: "go",
		Nodes: []*graph.Node{
			{ID: callerID, Name: "Caller", Kind: graph.KindFunction, FilePath: goFile, Language: "go", StartLine: 10, EndLine: 25},
			{ID: source, Name: "input", Kind: graph.KindLocal, FilePath: goFile, Language: "go", StartLine: 11, EndLine: 11},
		},
	}
	callee = store_pebble.ParseFacts{
		File:              graph.FileMetaRow{FilePath: tsFile, NodeCount: 3},
		EffectiveLanguage: "typescript",
		Nodes: []*graph.Node{
			{ID: owner, Name: "Sink", Kind: graph.KindFunction, FilePath: tsFile, Language: "typescript", StartLine: 2, EndLine: 6},
		},
	}
	for i, name := range []string{"a", "z"} {
		id, line := owner+"#param:"+name+"@0", 2+i
		callee.Nodes = append(callee.Nodes, &graph.Node{ID: id, Name: name, Kind: graph.KindParam,
			FilePath: tsFile, Language: "typescript", StartLine: line, EndLine: line, Meta: map[string]any{"position": 0}})
		callee.Edges = append(callee.Edges, &graph.Edge{From: id, To: owner, Kind: graph.EdgeParamOf,
			FilePath: tsFile, Line: line, Origin: graph.OriginASTResolved})
	}
	if reverse {
		slices.Reverse(callee.Edges)
	}
	// The fixture starts after resolution. It does not claim that the native
	// resolver would produce this Go-to-TypeScript join from source text.
	for i, meta := range []map[string]any{
		{"arg_position": 0, "fixture_tag": "settled payload"},
		{"fixture_tag": "no position"},
		{"arg_position": 1, "fixture_tag": "no match"},
	} {
		arg := &graph.Edge{From: source, To: owner, Kind: graph.EdgeArgOf, FilePath: goFile, Line: 20 + i,
			Origin: graph.OriginASTResolved, Tier: "ast", Confidence: 0.75, ConfidenceLabel: "medium", Meta: meta}
		settled = append(settled, arg)
		raw := &graph.Edge{From: source, To: "unresolved::Sink", Kind: graph.EdgeArgOf, FilePath: goFile, Line: arg.Line,
			Origin: graph.OriginASTInferred}
		if pos, ok := meta["arg_position"]; ok {
			raw.Meta = map[string]any{"arg_position": pos}
		}
		caller.Edges = append(caller.Edges, raw)
	}
	return caller, callee, settled
}

func TestRebuildArgOfRejectsUnprovedContender(t *testing.T) {
	for _, tc := range []struct{ name, language, path string }{
		{"deferred competitor at its own path", "python", "probe.py"},
		{"supported but cross-recorded duplicate", "typescript", "foreign.ts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caller, callee, args := rebuildArgOfOrderFixture(false)
			build, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "working"), store_pebble.FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, build.Close()) })
			// The second candidate loses in complete-key order. A supported
			// winner must not hide a distinct, unproved contributor to it.
			edge := *callee.Edges[1]
			if tc.language == "python" {
				edge.FilePath = tc.path
			}
			duplicate := store_pebble.ParseFacts{
				File: graph.FileMetaRow{FilePath: tc.path}, EffectiveLanguage: tc.language,
				Edges: []*graph.Edge{&edge},
			}
			build.AddBatch(caller.Nodes, args)
			build.AddBatch(callee.Nodes, callee.Edges)
			build.AddBatch(duplicate.Nodes, duplicate.Edges)
			input := []store_pebble.ParseFacts{duplicate, caller, callee}
			err = runRebuildArgOf(t.Context(), build, func(yield func(store_pebble.ParseFacts, error) bool) {
				for _, receipt := range input {
					if !yield(receipt, nil) {
						return
					}
				}
			})
			require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
			var refusal *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refusal)
			require.Equal(t, uint64(1), refusal.Count, "count arguments, not possible producers")
			require.Equal(t, graph.EdgeIdentityFor(args[0]), refusal.First)
			var rows []*graph.Edge
			require.NoError(t, build.RebuildView().VisitEdges(t.Context(), func(e *graph.Edge) error {
				rows = append(rows, e)
				return nil
			}))
			want := slices.Concat(args, callee.Edges)
			if tc.language == "python" {
				want = append(want, &edge)
			}
			require.ElementsMatch(t, want, rows)
		})
	}
}

func TestRebuildArgOfRejectsMovingDestination(t *testing.T) {
	const file = "p.go"
	// Noncanonical but admitted parameter IDs can also be callee IDs.
	// The planned destination of the first row is the second old key.
	args := []*graph.Edge{
		{From: "caller", To: "a", Kind: graph.EdgeArgOf, FilePath: file, Line: 5, Meta: map[string]any{"arg_position": 0}},
		{From: "caller", To: "b", Kind: graph.EdgeArgOf, FilePath: file, Line: 5, Meta: map[string]any{"arg_position": 0}},
	}
	params := []*graph.Edge{
		{From: "b", To: "a", Kind: graph.EdgeParamOf, FilePath: file, Line: 2},
		{From: "c", To: "b", Kind: graph.EdgeParamOf, FilePath: file, Line: 3},
	}
	facts := store_pebble.ParseFacts{
		File: graph.FileMetaRow{FilePath: file, NodeCount: 2}, EffectiveLanguage: "go",
		Nodes: []*graph.Node{
			{ID: "b", FilePath: file, Kind: graph.KindParam, Meta: map[string]any{"position": 0}},
			{ID: "c", FilePath: file, Kind: graph.KindParam, Meta: map[string]any{"position": 0}},
		},
		Edges: slices.Concat(args, params),
	}
	build, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "working"), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, build.Close()) })
	require.NoError(t, build.AddParsedFile("", facts))
	err = runRebuildArgOf(t.Context(), build, func(yield func(store_pebble.ParseFacts, error) bool) {
		yield(facts, nil)
	})
	var chain *store_pebble.RebuildAttributionChainError
	require.ErrorAs(t, err, &chain)
	require.Equal(t, uint64(1), chain.Count)
	require.Equal(t, graph.EdgeIdentityFor(args[0]), chain.Old)
	require.Equal(t, graph.EdgeIdentityFor(args[1]), chain.Destination)
	var rows []*graph.Edge
	require.NoError(t, build.RebuildView().VisitEdges(t.Context(), func(e *graph.Edge) error {
		rows = append(rows, e)
		return nil
	}))
	require.ElementsMatch(t, facts.Edges, rows)
}

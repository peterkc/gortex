package resolver

import (
	"iter"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// rebuildSvelteKitRoute is one route directory with two page and two load
// candidates. The winners sort last by ID (+page.ts after +page.svelte, and
// +page.server.ts after +layout.server.js), so native last-wins election
// picks them while their receipts come first.
type rebuildSvelteKitRoute struct {
	page, loserPage         *graph.Node
	load, loserLoad         *graph.Node
	loadFile, loserLoadFile *graph.Node
}

func newRebuildSvelteKitRoute(dir, workspace, loadWorkspace string) rebuildSvelteKitRoute {
	node := func(base, name string, kind graph.NodeKind, language, ws string, line int) *graph.Node {
		file := dir + base
		id := file
		if kind != graph.KindFile {
			id += "::" + name
		}
		return &graph.Node{ID: id, Name: name, Kind: kind, FilePath: file, Language: language, WorkspaceID: ws, StartLine: line}
	}
	r := rebuildSvelteKitRoute{
		page:          node("+page.ts", "+page", graph.KindType, "typescript", workspace, 3),
		loserPage:     node("+page.svelte", "+page", graph.KindType, "svelte", workspace, 1),
		load:          node("+page.server.ts", "load", graph.KindFunction, "typescript", loadWorkspace, 5),
		loserLoad:     node("+layout.server.js", "load", graph.KindFunction, "javascript", workspace, 7),
		loadFile:      node("+page.server.ts", "+page.server.ts", graph.KindFile, "typescript", workspace, 0),
		loserLoadFile: node("+layout.server.js", "+layout.server.js", graph.KindFile, "javascript", workspace, 0),
	}
	for _, page := range []*graph.Node{r.page, r.loserPage} {
		page.Meta = map[string]any{"component": true}
	}
	return r
}

// receipts lists the winning files first, opposite to native ID order.
func (r rebuildSvelteKitRoute) receipts(pageEdges ...*graph.Edge) []store_pebble.ParseFacts {
	return []store_pebble.ParseFacts{
		rebuildEventFacts(r.page.FilePath, r.page.Language, []*graph.Node{r.page}),
		rebuildEventFacts(r.loserPage.FilePath, r.loserPage.Language, []*graph.Node{r.loserPage}, pageEdges...),
		rebuildEventFacts(r.load.FilePath, r.load.Language, []*graph.Node{r.load, r.loadFile}),
		rebuildEventFacts(r.loserLoad.FilePath, r.loserLoad.Language, []*graph.Node{r.loserLoad, r.loserLoadFile}),
	}
}

// rebuildSvelteKitReference is the native Reference payload, written out so
// the oracle pins it independently of the resolver code that builds it.
func rebuildSvelteKitReference(page, load *graph.Node) *graph.Edge {
	return &graph.Edge{From: page.ID, To: load.ID, Kind: graph.EdgeReferences, FilePath: page.FilePath, Line: page.StartLine,
		Confidence: 0.6, ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeReferences, 0.6), Origin: graph.OriginASTInferred,
		Meta: map[string]any{"via": sveltekitLoadVia, MetaSynthesizedBy: SynthSvelteKitLoad, MetaProvenance: ProvenanceHeuristic}}
}

func rebuildSvelteKitEdges(rows rebuildEventRows) []*graph.Edge {
	var out []*graph.Edge
	for _, e := range rows.edges {
		if e.Meta["via"] == sveltekitLoadVia {
			out = append(out, e)
		}
	}
	return out
}

func rebuildSvelteKitSelection(t *testing.T, b *store_pebble.BuildStore) FrameworkSynthesizerSelection {
	t.Helper()
	selected, err := NewFrameworkSynthesizerSelection([]string{SynthSvelteKitLoad})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selected)
	require.NoError(t, err)
	require.True(t, frozen.allows(SynthSvelteKitLoad))
	return frozen
}

// rebuildSvelteKitOmit drops one node from the receipt of its own file and
// recounts it, as a decoded receipt would.
func rebuildSvelteKitOmit(receipts []store_pebble.ParseFacts, omitted *graph.Node) {
	for i, f := range receipts {
		if f.File.FilePath == omitted.FilePath {
			receipts[i].Nodes = slices.DeleteFunc(slices.Clone(f.Nodes), func(n *graph.Node) bool { return n == omitted })
			receipts[i].File.NodeCount = len(receipts[i].Nodes)
		}
	}
}

func TestRebuildSvelteKitLoadSQLiteOracle(t *testing.T) {
	matched := newRebuildSvelteKitRoute("src/routes/blog/", "web", "web")
	// The elected load is in another workspace: the route pairs nothing, and
	// neither the same-workspace layout load nor a server file stands in.
	foreign := newRebuildSvelteKitRoute("src/routes/admin/", "web", "remote")
	bystander := &graph.Edge{From: matched.loserPage.ID, To: matched.loserLoadFile.ID, Kind: graph.EdgeReferences,
		FilePath: matched.loserPage.FilePath, Line: 2, Meta: map[string]any{"bystander": true}}
	facts := append(matched.receipts(bystander), foreign.receipts()...)
	b, native := openRebuildEventStores(t, facts...)
	selection := rebuildSvelteKitSelection(t, b)
	require.Equal(t, 1, ResolveSvelteKitLoad(native))
	want := rebuildEventSQLiteRows(native)
	require.Equal(t, []*graph.Edge{rebuildSvelteKitReference(matched.page, matched.load)}, rebuildSvelteKitEdges(want),
		"the greatest page and load IDs win, not the first receipt")
	require.Contains(t, want.edges, bystander)
	for _, expected := range []uint64{1, 0} {
		count, err := RunRebuildSvelteKitLoad(t.Context(), b, rebuildGRPCReceipts(facts...), selection, nil)
		require.NoError(t, err)
		require.Equal(t, expected, count)
		require.Equal(t, want, rebuildEventSavedRows(t, b))
	}
}

func TestRebuildSvelteKitLoadReceiptOmission(t *testing.T) {
	matched := newRebuildSvelteKitRoute("src/routes/blog/", "web", "web")
	foreign := newRebuildSvelteKitRoute("src/routes/admin/", "web", "remote")
	live := []*graph.Edge{rebuildSvelteKitReference(matched.page, matched.load)}
	for _, tc := range []struct {
		name    string
		omitted *graph.Node
		proof   []*graph.Edge // native result over the receipts alone
	}{
		{"winning page", matched.page, []*graph.Edge{rebuildSvelteKitReference(matched.loserPage, matched.load)}},
		{"winning load", matched.load, []*graph.Edge{rebuildSvelteKitReference(matched.page, matched.loserLoad)}},
		{"suppressing load", foreign.load, []*graph.Edge{
			rebuildSvelteKitReference(foreign.page, foreign.loserLoad), rebuildSvelteKitReference(matched.page, matched.load),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := append(matched.receipts(), foreign.receipts()...)
			receipts := slices.Clone(facts)
			rebuildSvelteKitOmit(receipts, tc.omitted)
			b, native := openRebuildEventStores(t, facts...)
			_, proof := openRebuildEventStores(t, receipts...)
			ResolveSvelteKitLoad(native)
			ResolveSvelteKitLoad(proof)
			require.Equal(t, live, rebuildSvelteKitEdges(rebuildEventSQLiteRows(native)))
			require.Equal(t, tc.proof, rebuildSvelteKitEdges(rebuildEventSQLiteRows(proof)), "the omitted row changes the election")
			before := rebuildEventSavedRows(t, b)
			count, err := RunRebuildSvelteKitLoad(t.Context(), b, rebuildGRPCReceipts(receipts...), rebuildSvelteKitSelection(t, b), nil)
			require.ErrorIs(t, err, store_pebble.ErrRebuildSvelteKitLoad)
			require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
			require.Zero(t, count)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildSvelteKitLoadFileFallback(t *testing.T) {
	const dir = "src/routes/fallback/"
	page := &graph.Node{ID: dir + "+page.svelte::+page", Kind: graph.KindType, FilePath: dir + "+page.svelte",
		Language: "svelte", Meta: map[string]any{"component": true}, StartLine: 3}
	older := &graph.Node{ID: dir + "+layout.server.js", Kind: graph.KindFile, FilePath: dir + "+layout.server.js", Language: "javascript"}
	server := &graph.Node{ID: dir + "+page.server.ts", Kind: graph.KindFile, FilePath: dir + "+page.server.ts", Language: "typescript"}
	for _, tc := range []struct {
		name, mode string
		refuse     bool
	}{
		{"parse receipt", "parsed", false},
		{"checked skip outvotes parsed file", "checked", false},
		{"missing skip witness", "unproved", true},
		{"test-file stamp differs from receipt", "changed", false},
		{"test-file stamp differs from checked skip", "checked and stamped", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			facts := []store_pebble.ParseFacts{
				rebuildEventFacts(page.FilePath, page.Language, []*graph.Node{page}),
				rebuildEventFacts(older.FilePath, older.Language, []*graph.Node{older}),
			}
			live := *server
			var skips iter.Seq[*graph.Node]
			if tc.mode == "parsed" || tc.mode == "changed" {
				facts = append(facts, rebuildEventFacts(server.FilePath, server.Language, []*graph.Node{server}))
			}
			b, native := openRebuildEventStores(t, facts...)
			switch tc.mode {
			case "checked", "checked and stamped":
				raw := *server
				raw.Meta = map[string]any{"witness": "checked skip"}
				live = raw
				if tc.mode == "checked and stamped" {
					live.Meta = map[string]any{"witness": "checked skip", "is_test_file": true}
				}
				skips = func(yield func(*graph.Node) bool) { yield(&raw) }
			case "changed":
				live.Meta = map[string]any{"is_test_file": true}
			}
			if tc.mode != "parsed" {
				b.AddNode(&live)
				require.NoError(t, native.AddBatchChecked([]*graph.Node{&live}, nil))
			}
			selection := rebuildSvelteKitSelection(t, b)
			before := rebuildEventSavedRows(t, b)
			require.Equal(t, 1, ResolveSvelteKitLoad(native))
			want := rebuildEventSQLiteRows(native)
			require.Equal(t, []*graph.Edge{rebuildSvelteKitReference(page, &live)}, rebuildSvelteKitEdges(want))
			adds, err := RunRebuildSvelteKitLoad(ctx, b, rebuildGRPCReceipts(facts...), selection, skips)
			if tc.refuse {
				require.ErrorIs(t, err, store_pebble.ErrRebuildSvelteKitLoad)
				require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
				require.Zero(t, adds)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 1, adds)
			require.Equal(t, want, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildSvelteKitLoadKeepsIncumbent(t *testing.T) {
	route := newRebuildSvelteKitRoute("src/routes/occupied/", "web", "web")
	facts := route.receipts()
	incumbent := rebuildSvelteKitReference(route.page, route.load)
	incumbent.Meta = map[string]any{"witness": "ordinary reference"}
	incumbent.Confidence = 0
	incumbent.ConfidenceLabel = ""
	facts[0].Edges = []*graph.Edge{incumbent}
	b, native := openRebuildEventStores(t, facts...)
	ResolveSvelteKitLoad(native)
	want := rebuildEventSQLiteRows(native)
	require.Contains(t, want.edges, incumbent)
	count, err := RunRebuildSvelteKitLoad(t.Context(), b, rebuildGRPCReceipts(facts...), rebuildSvelteKitSelection(t, b), nil)
	require.NoError(t, err)
	require.Zero(t, count)
	require.Equal(t, want, rebuildEventSavedRows(t, b))
}

func TestRebuildSvelteKitLoadDuplicateNodeReceipts(t *testing.T) {
	route := newRebuildSvelteKitRoute("src/routes/duplicate/", "web", "web")
	other := *route.page
	other.StartLine++
	for _, first := range []*graph.Node{route.page, &other} {
		last := &other
		if first == &other {
			last = route.page
		}
		facts := route.receipts()
		facts[0].Nodes = []*graph.Node{first, last}
		facts[0].File.NodeCount = len(facts[0].Nodes)
		build, native := openRebuildEventStores(t, facts...)
		require.Equal(t, 1, ResolveSvelteKitLoad(native))
		want := rebuildEventSQLiteRows(native)
		require.Equal(t, []*graph.Edge{rebuildSvelteKitReference(last, route.load)}, rebuildSvelteKitEdges(want))
		adds, err := RunRebuildSvelteKitLoad(t.Context(), build, rebuildGRPCReceipts(facts...), rebuildSvelteKitSelection(t, build), nil)
		require.NoError(t, err)
		require.EqualValues(t, 1, adds)
		require.Equal(t, want, rebuildEventSavedRows(t, build))
	}
}

func TestRebuildSvelteKitLoadWinnerWorkspaceProof(t *testing.T) {
	route := newRebuildSvelteKitRoute("src/routes/workspace/", "web", "web")
	page := rebuildEventFacts(route.page.FilePath, route.page.Language, []*graph.Node{route.page})
	server := rebuildEventFacts(route.loadFile.FilePath, route.loadFile.Language, []*graph.Node{route.loadFile})
	b := openRebuildGRPCBuild(t, page, server)
	before := rebuildEventSavedRows(t, b)
	// Both facts still elect the same edge identity, but neither proves
	// the workspace that native reads from the saved winners.
	rawPage, rawFile := *route.page, *route.loadFile
	rawPage.WorkspaceID, rawFile.WorkspaceID = "other", "other"
	page.Nodes = []*graph.Node{&rawPage}
	server.Nodes = []*graph.Node{&rawFile}
	adds, err := RunRebuildSvelteKitLoad(t.Context(), b, rebuildGRPCReceipts(page, server), rebuildSvelteKitSelection(t, b), nil)
	require.ErrorIs(t, err, store_pebble.ErrRebuildSvelteKitLoad)
	require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
	require.Zero(t, adds)
	require.Equal(t, before, rebuildEventSavedRows(t, b))
}

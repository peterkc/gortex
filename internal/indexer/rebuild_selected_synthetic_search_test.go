package indexer

import (
	"cmp"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/resolver"
	"github.com/zzet/gortex/internal/search"
)

// The terminals are typed inputs to the native synthesis pass, not evidence
// of actual Go/Python extraction or production relation selection.
func TestRebuildSelectedSyntheticSearchInput(t *testing.T) {
	ctx := t.Context()
	const repo = "workbench"
	goDecl := &graph.Node{ID: repo + "/zeta.go::zetaentry", Kind: graph.KindFunction, Name: "zetaentry", FilePath: repo + "/zeta.go", RepoPrefix: repo, Language: "go"}
	pyDecl := &graph.Node{ID: repo + "/alpha.py::alphaentry", Kind: graph.KindFunction, Name: "alphaentry", FilePath: repo + "/alpha.py", RepoPrefix: repo, Language: "python"}
	decls := []*graph.Node{pyDecl, goDecl}
	goRaw := &graph.Edge{From: goDecl.ID, To: "dep::example.org/needle::Run", Kind: graph.EdgeCalls, FilePath: goDecl.FilePath, Line: 2}
	pyRaw := &graph.Edge{From: pyDecl.ID, To: "dep::example.org/needle-extra::Run", Kind: graph.EdgeCalls, FilePath: pyDecl.FilePath, Line: 2}
	pySharedRaw := *pyRaw
	pySharedRaw.To = goRaw.To

	kept := &graph.Node{ID: "external-call::dep::example.org/needle", Kind: graph.KindModule, Name: "example.org/needle", QualName: "example.org/needle", FilePath: "external-call::dep:example.org/needle", Language: "go", Meta: map[string]any{"synthetic": true, "external_call": true, "import_path": "example.org/needle", "ecosystem": "dep"}}
	dropped := &graph.Node{ID: "external-call::dep::example.org/needle-extra", Kind: graph.KindModule, Name: "example.org/needle-extra", QualName: "example.org/needle-extra", FilePath: "external-call::dep:example.org/needle-extra", Language: "python", Meta: map[string]any{"synthetic": true, "external_call": true, "import_path": "example.org/needle-extra", "ecosystem": "dep"}}
	goBound := *goRaw
	goBound.To, goBound.Origin, goBound.Confidence, goBound.ConfidenceLabel = kept.ID, graph.OriginTextMatched, 0.5, "INFERRED"
	goBound.Meta = map[string]any{"external_call": true, "go_extern_target": goRaw.To}
	pyBound := *pyRaw
	pyBound.To, pyBound.Origin, pyBound.Confidence, pyBound.ConfidenceLabel = dropped.ID, graph.OriginTextMatched, 0.5, "INFERRED"
	pyBound.Meta = map[string]any{"external_call": true}
	pySharedBound := pyBound
	pySharedBound.To = kept.ID
	shared := *kept
	shared.Language = "python" // alpha.py sorts before zeta.go in native first-creator order.

	selectedNodes := []*graph.Node{pyDecl, goDecl, kept}
	selectedEdges := []*graph.Edge{&goBound}
	replay := func(raw []*graph.Edge, nodes []*graph.Node, edges []*graph.Edge) *graph.Graph {
		t.Helper()
		g := graph.New()
		// Fresh typed facts in each arm. The inputs have no mutable metadata.
		for _, n := range decls {
			copy := *n
			g.AddNode(&copy)
		}
		for _, e := range raw {
			copy := *e
			g.AddEdge(&copy)
		}
		require.ElementsMatch(t, decls, g.AllNodes())
		require.ElementsMatch(t, raw, g.AllEdges())
		require.Equal(t, len(edges), resolver.SynthesizeExternalCalls(g, true))
		require.ElementsMatch(t, nodes, g.AllNodes())
		require.ElementsMatch(t, edges, g.AllEdges())
		return g
	}
	full := replay([]*graph.Edge{goRaw, pyRaw}, []*graph.Node{pyDecl, goDecl, kept, dropped}, []*graph.Edge{&goBound, &pyBound})
	selected := replay([]*graph.Edge{goRaw}, selectedNodes, selectedEdges)
	// Separate counterfactual: suppressing a terminal can change the whole
	// retained node, even at the same ID. Do not normalize its Language.
	sharedFull := replay([]*graph.Edge{goRaw, &pySharedRaw}, []*graph.Node{pyDecl, goDecl, &shared}, []*graph.Edge{&goBound, &pySharedBound})
	sharedSelected := replay([]*graph.Edge{goRaw}, selectedNodes, selectedEdges)
	require.NotEqual(t, sharedFull.GetNode(kept.ID), sharedSelected.GetNode(kept.ID))

	// Declare the complete document text independently of the private store.
	// Use the native optional FTS normalization on the expected token lists.
	wantDocs := map[string]string{
		pyDecl.ID:  "alphaentry alpha py",
		goDecl.ID:  "zetaentry zeta go",
		kept.ID:    "example org needle external call dep example org needle example org needle",
		dropped.ID: "example org needle extra external call dep example org needle extra example org needle extra",
	}
	for id, text := range wantDocs {
		wantDocs[id] = strings.Join(search.NormalizeFTSTokens(strings.Fields(text)), " ")
	}
	idx := &Indexer{}
	ranks := make([]*store_sqlite.Store, 0, 2)
	for _, g := range []*graph.Graph{full, selected} {
		nodes := g.AllNodes()
		slices.SortFunc(nodes, func(a, b *graph.Node) int { return cmp.Compare(a.ID, b.ID) })
		items := make([]graph.SymbolFTSItem, 0, len(nodes))
		for _, n := range nodes {
			require.True(t, idx.shouldIndexForSearch(n), n.ID)
			text := ftsTokensFor(n, repo)
			require.Equal(t, wantDocs[n.ID], text, n.ID)
			items = append(items, graph.SymbolFTSItem{NodeID: n.ID, Tokens: text})
		}
		// SQLite supplies native rank only; the native Graph supplies the
		// already-checked node union, not the other way around.
		rank := newFTSStore(t)
		require.NoError(t, rank.AddBatchCheckedContext(ctx, nodes, nil))
		require.NoError(t, rank.BatchUpsertSymbolFTS(items))
		rows, next, err := rank.SymbolFTSGenerationRows(ctx, 0, "", 0, 100)
		require.NoError(t, err)
		require.Zero(t, next)
		require.Len(t, rows, len(nodes))
		for i, row := range rows {
			require.Equal(t, items[i].NodeID, row.NodeID)
			require.Equal(t, items[i].Tokens, row.Tokens)
			require.Equal(t, nodes[i].RepoPrefix, row.RepoPrefix)
		}
		ranks = append(ranks, rank)
	}

	build, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "working"), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, build.Close()) })
	headDir := filepath.Join(t.TempDir(), "head")
	head, err := store_pebble.OpenHead(headDir, rebuildTestIncarnation, store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() {
		if head != nil {
			require.NoError(t, head.Close())
		}
	})
	manifest, err := store_pebble.PrepareManifest(ctx, t.TempDir(), []store_pebble.ManifestFile{
		{Path: pyDecl.FilePath, Kind: 'f', Language: pyDecl.Language}, {Path: goDecl.FilePath, Kind: 'f', Language: goDecl.Language},
	}, store_pebble.Witness{Input: store_pebble.HeadDigest{1}, Recipe: store_pebble.HeadDigest{2}, Stage: store_pebble.StageManifest, Profile: store_pebble.FullProfile}, rebuildTestIncarnation)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, manifest)
	require.NoError(t, err)
	for _, n := range decls {
		require.NoError(t, build.AddParsedFile(repo, store_pebble.ParseFacts{Nodes: []*graph.Node{n}, File: graph.FileMetaRow{FilePath: n.FilePath, NodeCount: 1}}))
	}
	fileGraph, err := build.PrepareFileGraph(ctx, t.TempDir(), manifest.Witness, rebuildTestIncarnation)
	require.NoError(t, err)
	base, err := build.RetainFileGraph(ctx, fileGraph)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, fileGraph)
	require.NoError(t, err)
	// Test-only preselected input: no general selector runs here.
	build.AddBatch([]*graph.Node{selected.GetNode(kept.ID)}, selected.AllEdges())
	view := build.RebuildView()
	var nodes []*graph.Node
	gotDocs := make(map[string]string)
	require.NoError(t, view.VisitNodes(ctx, func(n *graph.Node) error {
		nodes = append(nodes, n)
		if idx.shouldIndexForSearch(n) {
			require.NotContains(t, gotDocs, n.ID)
			gotDocs[n.ID] = ftsTokensFor(n, repo)
		}
		return nil
	}))
	delete(wantDocs, dropped.ID)
	require.Equal(t, wantDocs, gotDocs, "complete preselected searchable document text")
	require.ElementsMatch(t, selectedNodes, nodes)
	var edges []*graph.Edge
	require.NoError(t, view.VisitEdges(ctx, func(e *graph.Edge) error { edges = append(edges, e); return nil }))
	require.ElementsMatch(t, selectedEdges, edges)
	relations, err := build.PrepareRelations(ctx, t.TempDir(), base, nil)
	require.NoError(t, err)
	digest, err := head.PublishStage(ctx, relations)
	require.NoError(t, err)
	derived, err := store_pebble.PrepareDeferredDerived(t.TempDir(), relations)
	require.NoError(t, err)
	require.Equal(t, digest, derived.Base)
	require.Equal(t, relations.Witness.Families, derived.Witness.Families)
	require.Equal(t, []store_pebble.UnsupportedFamily{
		{Family: "analysis_cache", Reason: "D64"}, {Family: "blame", Reason: "D64"}, {Family: "churn", Reason: "D64"},
		{Family: "content_search", Reason: "D64"}, {Family: "contracts", Reason: "D64"}, {Family: "coverage", Reason: "D64"}, {Family: "releases", Reason: "D64"},
	}, derived.Witness.Unsupported)
	require.Nil(t, derived.Witness.Relations)
	_, err = head.PublishStage(ctx, derived)
	require.NoError(t, err)
	stage, err := idx.prepareRebuildSearch(ctx, build, t.TempDir(), derived.Witness, rebuildTestIncarnation, repo)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, stage)
	require.NoError(t, err)
	require.NoError(t, head.Close())
	head = nil
	head, err = store_pebble.OpenHead(headDir, rebuildTestIncarnation, store_pebble.FullProfile)
	require.NoError(t, err)
	snap := head.OpenSnapshot()
	t.Cleanup(func() { require.NoError(t, snap.Close()) })
	saved, err := snap.Publication()
	require.NoError(t, err)
	require.Equal(t, stage.Witness, saved)
	require.Equal(t, derived.Witness.Unsupported, saved.Unsupported)
	require.Nil(t, saved.Relations)
	var unproved *store_pebble.ErrLinksNotAvailable
	require.ErrorAs(t, saved.RequireRelationCoverage(nil), &unproved)
	require.ElementsMatch(t, selectedNodes, snap.AllNodes())
	require.ElementsMatch(t, selectedEdges, snap.AllEdges())
	require.Equal(t, map[graph.EdgeKind]int{graph.EdgeCalls: 1}, snap.EdgeKindCounts())
	ids := []string{pyDecl.ID, goDecl.ID, kept.ID, dropped.ID}
	require.ElementsMatch(t, []graph.NodeDegreeRow{{NodeID: pyDecl.ID}, {NodeID: goDecl.ID, OutCount: 1}, {NodeID: kept.ID, InCount: 1, UsageInCount: 1}}, snap.NodeDegreeCounts(ids, []graph.EdgeKind{graph.EdgeCalls}))
	require.NoError(t, snap.ReadError())

	// All three selected documents participate in raw rank, including the
	// independent Python declaration and unowned module. Use exact arrays,
	// not a digest or a filter applied to the full-corpus hits.
	for _, tc := range []struct {
		query string
		ids   []string
		exact bool
	}{
		{"zetaentry", []string{goDecl.ID}, true}, {"alphaentry", []string{pyDecl.ID}, true},
		{"zetaentry alphaentry needle", []string{goDecl.ID, pyDecl.ID, kept.ID}, false},
		{"needle needle", []string{kept.ID}, false}, {"extra", nil, false},
	} {
		want, err := ranks[1].SearchSymbolsRepoScopedContext(ctx, tc.query, nil, 100)
		require.NoError(t, err)
		got, err := snap.SearchSymbolsRepoScopedContext(ctx, tc.query, nil, 100)
		require.NoError(t, err)
		var ids []string
		for _, h := range want {
			ids = append(ids, h.NodeID)
			if tc.exact {
				require.Equal(t, float64(100), h.Score)
			} else {
				require.Positive(t, h.Score)
				require.Less(t, h.Score, float64(100))
			}
		}
		require.ElementsMatch(t, tc.ids, ids, tc.query)
		require.Equal(t, want, got, tc.query)
	}
	fullHits, err := ranks[0].SearchSymbolsRepoScopedContext(ctx, "needle needle", nil, 100)
	require.NoError(t, err)
	require.Len(t, fullHits, 2)
	fullKept := slices.DeleteFunc(fullHits, func(h graph.SymbolHit) bool { return h.NodeID == dropped.ID })
	require.Len(t, fullKept, 1)
	require.Equal(t, kept.ID, fullKept[0].NodeID)
	selectedHits, err := ranks[1].SearchSymbolsRepoScopedContext(ctx, "needle needle", nil, 100)
	require.NoError(t, err)
	require.Len(t, selectedHits, 1)
	require.NotEqual(t, fullKept[0].Score, selectedHits[0].Score, "removing the deferred hit after ranking does not recover the selected score")
}

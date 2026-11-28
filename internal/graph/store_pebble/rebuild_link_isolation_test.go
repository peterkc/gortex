package store_pebble

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildLinkIsolationPreservesRawWinner(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	const py, goFile, rb = "a.py", "b.go", "c.rb"
	parent := &graph.Node{ID: py, Name: py, FilePath: py, Language: "python", Kind: graph.KindFile}
	win := &graph.Node{ID: "shared", Name: "PythonWinner", FilePath: py, Language: "python", Kind: graph.KindString}
	loser := &graph.Node{ID: win.ID, Name: "GoRunnerUp", FilePath: goFile, Language: "go", Kind: graph.KindString}
	g := &graph.Node{ID: "b.go::Run", Name: "Run", FilePath: goFile, Language: "go", Kind: graph.KindFunction}
	r := &graph.Node{ID: "c.rb::Run", Name: "RubyRun", FilePath: rb, Language: "ruby", Kind: graph.KindFunction}
	skip := &graph.Node{ID: "skip.py", Name: "skip.py", FilePath: "skip.py", Language: "python", Kind: graph.KindFile}
	defines := &graph.Edge{From: parent.ID, To: win.ID, Kind: graph.EdgeDefines, FilePath: py, Origin: graph.OriginASTResolved}
	later := *defines
	later.Origin = graph.OriginASTInferred
	contains := &graph.Edge{From: parent.ID, To: win.ID, Kind: graph.EdgeContains, FilePath: py}
	cross := &graph.Edge{From: parent.ID, To: r.ID, Kind: graph.EdgeContains, FilePath: py}
	call := &graph.Edge{From: win.ID, To: g.ID, Kind: graph.EdgeCalls, FilePath: py}
	ref := &graph.Edge{From: g.ID, To: win.ID, Kind: graph.EdgeReferences, FilePath: goFile, Meta: map[string]any{"receiver_expr": "x"}}
	evidence := &graph.Edge{From: win.ID, To: "unresolved::extern::net/http::Foo", Kind: graph.EdgeCalls, FilePath: goFile, Meta: map[string]any{"receiver_expr": "x"}}
	imports := &graph.Edge{From: parent.ID, To: "unresolved::pyrel::foo", Kind: graph.EdgeImports, FilePath: py}
	arg := &graph.Edge{From: win.ID, To: g.ID, Kind: graph.EdgeArgOf, FilePath: py}
	facts := []ParseFacts{
		{File: graph.FileMetaRow{FilePath: py, NodeCount: 2}, EffectiveLanguage: "python", Nodes: []*graph.Node{parent, win},
			Edges: []*graph.Edge{defines, &later, contains, cross, call, imports, arg},
			Constants: []graph.ConstantValueRow{{NodeID: parent.ID, FilePath: py, Value: "python"}, {NodeID: g.ID, FilePath: py, Value: "not a Go input"}},
			Shingles:  map[string][]uint64{parent.ID: {11}}},
		{File: graph.FileMetaRow{FilePath: goFile, NodeCount: 2}, EffectiveLanguage: "go", Nodes: []*graph.Node{loser, g}, Edges: []*graph.Edge{ref, evidence},
			Constants: []graph.ConstantValueRow{{NodeID: g.ID, FilePath: goFile, Value: "go"}}, Shingles: map[string][]uint64{g.ID: {22}}},
		{File: graph.FileMetaRow{FilePath: rb, NodeCount: 1}, EffectiveLanguage: "ruby", Nodes: []*graph.Node{r}},
	}
	seq := func(yield func(ParseFacts, error) bool) {
		for _, f := range facts {
			if !yield(f, nil) {
				return
			}
		}
	}
	claims, err := b.ReduceRebuildSharedClaims(ctx, 1<<20, seq, func(keep func(string, uint64) (bool, error)) error {
		for i, f := range facts {
			add := b.AddParsedFileSelected
			if f.EffectiveLanguage != "go" {
				add = b.AddRebuildDeferredFile
			}
			if err := add("", f, func(id string) (bool, error) { return keep(id, uint64(i+1)) }); err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, claims.AcceptedStrings)
	require.NoError(t, b.AddRebuildHeldSkip(skip))
	require.NoError(t, b.UpsertSymbolFTS(win.ID, "deferred token"))
	var files []ManifestFile
	for _, f := range facts {
		files = append(files, ManifestFile{Path: f.File.FilePath, Language: f.EffectiveLanguage, Size: 1, Kind: 'f', Content: HeadDigest{3}, Extractor: HeadDigest{4}})
	}
	w := Witness{Input: HeadDigest{1}, Recipe: HeadDigest{2}, Profile: FullProfile, Stage: StageManifest}
	manifest, err := PrepareManifest(ctx, t.TempDir(), files, w, testHeadIncarnation)
	require.NoError(t, err)
	h, err := OpenHead(filepath.Join(t.TempDir(), "head"), testHeadIncarnation, FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	_, err = h.PublishStage(ctx, manifest)
	require.NoError(t, err)
	fg, err := b.PrepareFileGraph(ctx, t.TempDir(), manifest.Witness, testHeadIncarnation)
	require.NoError(t, err)
	base, err := b.RetainFileGraph(ctx, fg)
	require.NoError(t, err)
	_, err = h.PublishStage(ctx, fg)
	require.NoError(t, err)
	published := stageRead(t, h)
	require.Zero(t, published.EdgeCount())
	require.Equal(t, win.Name, published.GetNode(win.ID).Name)
	require.NotNil(t, published.GetNode(skip.ID))

	var rawNodes, rawEdges [][]byte
	for _, n := range []*graph.Node{parent, win, r} {
		raw, err := pendingGet(b.pending, pendingNodeKey(n.ID), copyValue)
		require.NoError(t, err)
		require.NotEmpty(t, raw)
		rawNodes = append(rawNodes, raw)
	}
	for _, e := range []*graph.Edge{defines, contains} {
		raw, err := pendingGet(b.pending, pendingEdgeKey(e), copyValue)
		require.NoError(t, err)
		want, err := encodeEdge(e)
		require.NoError(t, err)
		require.Equal(t, want, raw, "raw first payload wins")
		rawEdges = append(rawEdges, raw)
	}
	for _, prefix := range [][]byte{factoryChainReceiptKey(pendingEdgeKey(evidence)), rebuildExternalEvidenceKey('r', pendingEdgeKey(evidence))} {
		found, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
		require.NoError(t, err)
		require.True(t, found, "ordinary raw receipt installed evidence at %x", prefix)
	}
	require.NoError(t, b.IsolateRebuildLinks(ctx))
	_, err = b.PrepareRelations(ctx, t.TempDir(), base, nil)
	require.ErrorIs(t, err, ErrRebuildLinkIsolation)
	view := b.RebuildView()
	var linkedIDs []string
	require.NoError(t, view.VisitNodes(ctx, func(n *graph.Node) error { linkedIDs = append(linkedIDs, n.ID); return nil }))
	require.Equal(t, []string{g.ID}, linkedIDs)
	for _, prefix := range [][]byte{
		{familyEdge}, keyStrings([]byte{familyFile}, py), keyStrings([]byte{familyFile}, rb), keyStrings([]byte{familyFile}, skip.FilePath),
		keyStrings([]byte{familyRepo}, "", win.ID), pendingSymbolKey(win.ID), parsedConstantKey(parent.ID), shingleKey("", parent.ID),
		factoryChainReceiptKey(pendingEdgeKey(evidence)), rebuildExternalEvidenceKey('r', pendingEdgeKey(evidence)), rebuildExternalEvidenceKey('r', pendingEdgeKey(imports)),
	} {
		have, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
		require.NoError(t, err)
		require.False(t, have, "link input survived at %x", prefix)
	}
	for _, name := range []string{win.Name, loser.Name} {
		require.NoError(t, view.VisitNameCandidates(ctx, name, nil, func(*graph.Node) error {
			t.Fatalf("held candidate remains for %s", name)
			return nil
		}))
	}
	value, err := view.ConstantValue(ctx, g.ID)
	require.NoError(t, err)
	require.NotNil(t, value)
	require.Equal(t, "go", value.Value)
	var shingles []string
	require.NoError(t, view.VisitCloneShingles(ctx, "", func(id string, _ []uint64) error { shingles = append(shingles, id); return nil }))
	require.Equal(t, []string{g.ID}, shingles)

	g.Name = "LinkedGo"
	b.AddNode(g)
	require.NoError(t, b.RestoreRebuildDeclarations(ctx))
	for i, n := range []*graph.Node{parent, win, r} {
		raw, err := pendingGet(b.pending, pendingNodeKey(n.ID), copyValue)
		require.NoError(t, err)
		require.Equal(t, rawNodes[i], raw)
	}
	for i, e := range []*graph.Edge{defines, contains} {
		raw, err := pendingGet(b.pending, pendingEdgeKey(e), copyValue)
		require.NoError(t, err)
		require.Equal(t, rawEdges[i], raw)
		requireRebuildLinkCustody(t, b, e, rebuildEdgeCustody{})
	}
	relations, err := b.PrepareRelations(ctx, t.TempDir(), base, nil)
	require.NoError(t, err)
	relations.Witness.Relations = &RelationCoverage{DeferredLanguages: []string{"python", "ruby"}}
	_, err = h.PublishStage(ctx, relations)
	require.NoError(t, err)
	selected := stageRead(t, h)
	require.Equal(t, 2, selected.EdgeCount())
	require.Equal(t, win.Name, selected.GetNode(win.ID).Name)
	require.Equal(t, g.Name, selected.GetNode(g.ID).Name)
	require.Nil(t, selected.GetNode(skip.ID))
	require.NotNil(t, published.GetNode(skip.ID), "the pinned FileGraph cut remains complete")
	metadata, err := selected.FileMetadata("", py)
	require.NoError(t, err)
	require.Equal(t, facts[0].File, metadata)
}

func TestRebuildLinkIsolationRefusesChangedCustody(t *testing.T) {
	for _, name := range []string{"changed raw bytes", "node collision", "edge to held node"} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			b := newBuild(t, FullProfile)
			a := &graph.Node{ID: "a.py", Name: "A", Kind: graph.KindFile, FilePath: "a.py"}
			z := &graph.Node{ID: "z.py", Name: "Z", Kind: graph.KindFile, FilePath: "a.py"}
			require.NoError(t, b.AddRebuildDeferredFile("", ParseFacts{
				Nodes: []*graph.Node{a, z}, File: graph.FileMetaRow{FilePath: "a.py", NodeCount: 2}, EffectiveLanguage: "python",
			}, func(string) (bool, error) { return true, nil }))
			rewrite := *z
			rewrite.Name = "Changed"
			if name == "changed raw bytes" {
				b.AddNode(&rewrite)
				require.ErrorIs(t, b.IsolateRebuildLinks(ctx), ErrRebuildLinkIsolation)
				stillRaw, err := b.pendingNode(a.ID)
				require.NoError(t, err)
				require.NotNil(t, stillRaw, "prove all capture bytes before deleting")
				return
			}
			require.NoError(t, b.IsolateRebuildLinks(ctx))
			if name == "node collision" {
				b.AddNode(&rewrite)
			} else {
				b.AddEdge(&graph.Edge{From: "new.go::Run", To: z.ID, Kind: graph.EdgeCalls, FilePath: "new.go"})
			}
			require.ErrorIs(t, b.RestoreRebuildDeclarations(ctx), ErrRebuildLinkIsolation)
			stillAbsent, err := b.pendingNode(a.ID)
			require.NoError(t, err)
			require.Nil(t, stillAbsent, "prove all destinations before restoring")
			incumbent, err := b.pendingNode(z.ID)
			require.NoError(t, err)
			if name == "node collision" {
				require.Equal(t, rewrite.Name, incumbent.Name)
			} else {
				require.Nil(t, incumbent, "do not validate a link to an omitted input by restoring it")
			}
		})
	}
}

func TestRebuildLinkIsolationSupportedOnlyUnchanged(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	a := &graph.Node{ID: "a.ts", Name: "A", Kind: graph.KindFile, FilePath: "a.ts"}
	require.NoError(t, b.AddParsedFile("", ParseFacts{
		Nodes: []*graph.Node{a}, File: graph.FileMetaRow{FilePath: "a.ts", NodeCount: 1}, EffectiveLanguage: "typescript",
		Edges: []*graph.Edge{{From: a.ID, To: "unresolved::x", Kind: graph.EdgeCalls, FilePath: "a.ts"}},
	}))
	all := func() []row {
		var out []row
		for _, family := range []byte{familyNode, familyEdge, familyFile, familyRepo, familyFileMeta} {
			require.NoError(t, pendingVisit(ctx, b.pending, []byte{family}, func(r row) error {
				out = append(out, row{bytes.Clone(r.key), bytes.Clone(r.value)})
				return nil
			}))
		}
		return out
	}
	before := all()
	require.NoError(t, b.IsolateRebuildLinks(ctx))
	require.Equal(t, before, all())
	require.NoError(t, b.RestoreRebuildDeclarations(ctx))
	require.Equal(t, before, all())
}

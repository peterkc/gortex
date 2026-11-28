package store_pebble

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/cockroachdb/pebble/v2"
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
	flow := &graph.Edge{From: "unresolved::extern::net/http::Foo", To: g.ID, Kind: graph.EdgeValueFlow, FilePath: goFile, Line: 2, Origin: graph.OriginASTResolved}
	ref := &graph.Edge{From: g.ID, To: win.ID, Kind: graph.EdgeReferences, FilePath: goFile, Meta: map[string]any{"receiver_expr": "x"}}
	evidence := &graph.Edge{From: win.ID, To: "unresolved::extern::net/http::Foo", Kind: graph.EdgeCalls, FilePath: goFile, Meta: map[string]any{"receiver_expr": "x"}}
	imports := &graph.Edge{From: parent.ID, To: "unresolved::pyrel::foo", Kind: graph.EdgeImports, FilePath: py}
	arg := &graph.Edge{From: win.ID, To: g.ID, Kind: graph.EdgeArgOf, FilePath: py}
	facts := []ParseFacts{
		{File: graph.FileMetaRow{FilePath: py, NodeCount: 2}, EffectiveLanguage: "python", Nodes: []*graph.Node{parent, win},
			Edges:     []*graph.Edge{defines, &later, contains, cross, call, imports, arg},
			Constants: []graph.ConstantValueRow{{NodeID: parent.ID, FilePath: py, Value: "python"}, {NodeID: g.ID, FilePath: py, Value: "not a Go input"}},
			Shingles:  map[string][]uint64{parent.ID: {11}}},
		{File: graph.FileMetaRow{FilePath: goFile, NodeCount: 2}, EffectiveLanguage: "go", Nodes: []*graph.Node{loser, g}, Edges: []*graph.Edge{ref, evidence, flow},
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
			selected := func(id string) (bool, error) { return keep(id, uint64(i+1)) }
			if f.EffectiveLanguage != "go" {
				if err := b.AddRebuildDeferredFile("", f, selected); err != nil {
					return err
				}
			} else if err := b.AddParsedFileSelected("", f, selected, true); err != nil {
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
	flowRaw, err := pendingGet(b.pending, pendingEdgeKey(flow), copyValue)
	require.NoError(t, err)
	require.NotEmpty(t, flowRaw)
	requireRebuildLinkCustody(t, b, flow, rebuildEdgeCustody{proof: rebuildProofGoDataflowSource})
	require.NoError(t, b.IsolateRebuildLinks(ctx))
	kept, err := pendingGet(b.pending, pendingEdgeKey(flow), copyValue)
	require.NoError(t, err)
	require.Equal(t, flowRaw, kept)
	requireRebuildLinkCustody(t, b, flow, rebuildEdgeCustody{proof: rebuildProofGoDataflowSource})
	_, err = b.PrepareRelations(ctx, t.TempDir(), base, nil)
	require.ErrorIs(t, err, ErrRebuildLinkIsolation)
	view := b.RebuildView()
	var linkedIDs []string
	require.NoError(t, view.VisitNodes(ctx, func(n *graph.Node) error { linkedIDs = append(linkedIDs, n.ID); return nil }))
	require.Equal(t, []string{g.ID}, linkedIDs)
	linkCopy, err := b.SelectRebuildLinkFacts(ctx, ParseFacts{
		File: facts[1].File, Nodes: []*graph.Node{loser, g}, Edges: []*graph.Edge{ref, evidence, flow},
		Constants: []graph.ConstantValueRow{{NodeID: loser.ID}, {NodeID: g.ID}},
		Shingles:  map[string][]uint64{loser.ID: {10}, g.ID: {22}},
		Symbols:   []graph.SymbolFTSItem{{NodeID: loser.ID}, {NodeID: g.ID}},
	})
	require.NoError(t, err)
	require.Equal(t, []*graph.Node{g}, linkCopy.Nodes)
	require.Equal(t, 1, linkCopy.File.NodeCount)
	require.Equal(t, []*graph.Edge{flow}, linkCopy.Edges)
	for _, e := range []*graph.Edge{defines, contains, cross, call, ref, evidence, imports, arg} {
		key := pendingEdgeKey(e)
		n, err := b.pendingEdge(key)
		require.NoError(t, err)
		require.Nil(t, n)
		reverse, err := pendingGet(b.pending, pendingReverseKey(e, key), copyValue)
		require.NoError(t, err)
		require.Nil(t, reverse, "held edge reverse row must not survive the cut")
	}
	require.Equal(t, []graph.ConstantValueRow{{NodeID: g.ID}}, linkCopy.Constants)
	require.Equal(t, map[string][]uint64{g.ID: {22}}, linkCopy.Shingles)
	require.Equal(t, []graph.SymbolFTSItem{{NodeID: g.ID}}, linkCopy.Symbols)
	for _, prefix := range [][]byte{
		keyStrings([]byte{familyFile}, py), keyStrings([]byte{familyFile}, rb), keyStrings([]byte{familyFile}, skip.FilePath),
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
	kept, err = pendingGet(b.pending, pendingEdgeKey(flow), copyValue)
	require.NoError(t, err)
	require.Equal(t, flowRaw, kept)
	requireRebuildLinkCustody(t, b, flow, rebuildEdgeCustody{proof: rebuildProofGoDataflowSource})
	require.NoError(t, b.CheckRebuildIsolationCoverage([]string{"python", "ruby"}))
	relations, err := b.PrepareRelations(ctx, t.TempDir(), base, func(path string) string {
		if path == goFile {
			return "go"
		}
		return ""
	})
	require.NoError(t, err)
	require.True(t, relations.Witness.Relations.IsolationChecked)
	_, err = h.PublishStage(ctx, relations)
	require.NoError(t, err)
	selected := stageRead(t, h)
	require.Equal(t, 3, selected.EdgeCount())
	require.Equal(t, win.Name, selected.GetNode(win.ID).Name)
	require.Equal(t, g.Name, selected.GetNode(g.ID).Name)
	require.Equal(t, published.GetNode(skip.ID), selected.GetNode(skip.ID))
	require.NotNil(t, published.GetNode(skip.ID), "the pinned FileGraph cut remains complete")
	metadata, err := selected.FileMetadata("", py)
	require.NoError(t, err)
	require.Equal(t, facts[0].File, metadata)
}

func TestRebuildLinkIsolationRefusesChangedRowsAndCollisions(t *testing.T) {
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
	require.NoError(t, b.AddParsedFileSelected("", ParseFacts{
		Nodes: []*graph.Node{a}, File: graph.FileMetaRow{FilePath: "a.ts", NodeCount: 1}, EffectiveLanguage: "typescript",
		Edges: []*graph.Edge{{From: a.ID, To: "unresolved::x", Kind: graph.EdgeCalls, FilePath: "a.ts"}},
	}, func(string) (bool, error) { return true, nil }, false))
	have, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildHoldoutPrefix)
	require.NoError(t, err)
	require.False(t, have, "supported-only replay must not write attribution scratch")
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

func TestRebuildHeldDeclarationAttribution(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	api := &graph.Node{ID: "a.py::API", FilePath: "a.py", Name: "API", Kind: graph.KindConstant, Language: "go", StartLine: 7}
	other := &graph.Node{ID: "a.py::API()", FilePath: "a.py", Name: "API", Kind: graph.KindFunction}
	second := &graph.Node{ID: "a.py::API@2", FilePath: "a.py", Name: "API", Kind: graph.KindConstant}
	adjacent := &graph.Node{ID: "a.py::API2", FilePath: "a.py", Name: "API2", Kind: graph.KindConstant}
	require.NoError(t, b.AddRebuildDeferredFile("", ParseFacts{
		File: graph.FileMetaRow{FilePath: "a.py", NodeCount: 4}, EffectiveLanguage: "python", Nodes: []*graph.Node{api, other, second, adjacent},
	}, func(string) (bool, error) { return true, nil }))
	ruby := &graph.Node{ID: "b.rb::API", FilePath: "b.rb", Name: "API", Kind: graph.KindFunction}
	require.NoError(t, b.AddRebuildDeferredFile("", ParseFacts{
		File: graph.FileMetaRow{FilePath: "b.rb", NodeCount: 1}, EffectiveLanguage: "ruby", Nodes: []*graph.Node{ruby},
	}, func(string) (bool, error) { return true, nil }))
	skip := &graph.Node{ID: "skip.py", FilePath: "skip.py", Name: "API", Kind: graph.KindFile, Language: "python"}
	require.NoError(t, b.AddRebuildHeldSkip(skip))
	view := b.RebuildView()
	heldIDs := func(name string, kind graph.NodeKind) []string {
		t.Helper()
		var ids []string
		require.NoError(t, view.VisitRebuildHeldNameIDs(ctx, name, kind, func(id string) error {
			ids = append(ids, id)
			return nil
		}))
		return ids
	}
	require.Empty(t, heldIDs("API", graph.KindConstant))
	require.NoError(t, b.IsolateRebuildLinks(ctx))
	require.Equal(t, []string{api.ID, second.ID}, heldIDs("API", graph.KindConstant))
	require.Equal(t, []string{other.ID, ruby.ID}, heldIDs("API", graph.KindFunction))
	require.Equal(t, []string{adjacent.ID}, heldIDs("API2", graph.KindConstant))
	require.Empty(t, heldIDs("API", graph.KindFile), "checked skip is not a declaration")
	file, language, kind, line, err := view.RebuildHeldDeclaration(api.ID)
	require.NoError(t, err)
	require.Equal(t, api.FilePath, file)
	require.Equal(t, "python", language, "effective receipt, not language tag")
	require.Equal(t, api.Kind, kind)
	require.Equal(t, api.StartLine, line)
	for _, id := range []string{skip.ID, "missing"} {
		file, language, kind, line, err := view.RebuildHeldDeclaration(id)
		require.NoError(t, err)
		require.Empty(t, file)
		require.Empty(t, language)
		require.Empty(t, kind)
		require.Zero(t, line)
	}
	languages, err := view.RebuildHeldNameLanguages(ctx, "API", graph.KindConstant)
	require.NoError(t, err)
	require.Equal(t, []string{"python"}, languages)
	languages, err = view.RebuildHeldNameLanguages(ctx, "API", graph.KindFunction)
	require.NoError(t, err)
	require.Equal(t, []string{"python", "ruby"}, languages)
	languages, err = view.RebuildHeldNameLanguages(ctx, "API", graph.KindFile)
	require.NoError(t, err)
	require.Empty(t, languages)
	languages, err = view.RebuildHeldNameLanguages(ctx, "API2", graph.KindConstant)
	require.NoError(t, err)
	require.Equal(t, []string{"python"}, languages)
	languages, err = view.RebuildHeldNameLanguages(ctx, "API3", graph.KindConstant)
	require.NoError(t, err)
	require.Empty(t, languages)
	for _, n := range []*graph.Node{api, other, second, adjacent, ruby} {
		live, err := view.Node(ctx, n.ID)
		require.NoError(t, err)
		require.Nil(t, live, "attribution must not expose a link candidate")
	}
	stopped := errors.New("stop held ID visit")
	require.ErrorIs(t, view.VisitRebuildHeldNameIDs(ctx, "API", graph.KindConstant, func(string) error { return stopped }), stopped)
	bad := keyStrings(rebuildHoldoutKey('m', nil), "Broken", string(graph.KindConstant), "")
	require.NoError(t, b.pending.Set(bad, []byte("python"), pebble.NoSync))
	err = view.VisitRebuildHeldNameIDs(ctx, "Broken", graph.KindConstant, func(string) error {
		t.Fatal("malformed held ID must not enter the callback")
		return nil
	})
	require.ErrorIs(t, err, ErrRebuildView)
	require.ErrorContains(t, err, "held name attribution key")
	_, err = view.RebuildHeldNameLanguages(ctx, "Broken", graph.KindConstant)
	require.ErrorContains(t, err, "held name attribution key")
	require.NoError(t, b.pending.Delete(bad, pebble.NoSync))
	require.NoError(t, b.RestoreRebuildDeclarations(ctx))
	require.Empty(t, heldIDs("API", graph.KindConstant))
	languages, err = view.RebuildHeldNameLanguages(ctx, "API")
	require.NoError(t, err)
	require.Empty(t, languages)
	file, _, _, _, err = view.RebuildHeldDeclaration(api.ID)
	require.NoError(t, err)
	require.Empty(t, file)
}

func TestRebuildHeldSkipRestoresNativeUpsert(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	file := &graph.Node{ID: "asset.txt", FilePath: "asset.txt", Kind: graph.KindFile, Name: "raw"}
	b.AddNode(file)
	skip := *file
	skip.Name = "asset.txt"
	skip.Meta = map[string]any{"skipped_due_to_content": true}
	require.NoError(t, b.AddRebuildHeldSkip(&skip))
	raw, err := pendingGet(b.pending, pendingNodeKey(skip.ID), copyValue)
	require.NoError(t, err)
	require.NoError(t, b.IsolateRebuildLinks(ctx))
	n, err := b.RebuildView().Node(ctx, skip.ID)
	require.NoError(t, err)
	require.Nil(t, n)
	require.NoError(t, b.RestoreRebuildDeclarations(ctx))
	restored, err := pendingGet(b.pending, pendingNodeKey(skip.ID), copyValue)
	require.NoError(t, err)
	require.Equal(t, raw, restored)
	n, err = b.RebuildView().Node(ctx, skip.ID)
	require.NoError(t, err)
	require.Equal(t, skip.Name, n.Name)
	require.Equal(t, true, n.Meta["skipped_due_to_content"])
}

func TestRebuildHeldSkipSupersedesDeferredCrossFileWinner(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	solution := &graph.Node{ID: "app.sln", FilePath: "app.sln", Kind: graph.KindFile, Language: "dotnet_project"}
	project := &graph.Node{ID: "app.csproj", FilePath: "app.csproj", Kind: graph.KindFile, Name: "raw project", Language: "dotnet_project"}
	defines := &graph.Edge{From: solution.ID, To: project.ID, Kind: graph.EdgeDefines, FilePath: solution.FilePath}
	require.NoError(t, b.AddRebuildDeferredFile("", ParseFacts{
		File: graph.FileMetaRow{FilePath: solution.FilePath, NodeCount: 2}, EffectiveLanguage: "dotnet_project",
		Nodes: []*graph.Node{solution, project}, Edges: []*graph.Edge{defines},
	}, func(string) (bool, error) { return true, nil }))
	skip := *project
	skip.Name = "app.csproj"
	skip.Meta = map[string]any{"skipped_due_to_size": true}
	require.NoError(t, b.AddRebuildHeldSkip(&skip))
	want, err := pendingGet(b.pending, pendingNodeKey(skip.ID), copyValue)
	require.NoError(t, err)
	require.NotEmpty(t, want)
	require.NoError(t, b.IsolateRebuildLinks(ctx))
	require.NoError(t, b.RestoreRebuildDeclarations(ctx))
	got, err := pendingGet(b.pending, pendingNodeKey(skip.ID), copyValue)
	require.NoError(t, err)
	require.Equal(t, want, got, "skip supersedes the held raw capture")
	key := pendingEdgeKey(defines)
	edge, err := b.pendingEdge(key)
	require.NoError(t, err)
	require.Nil(t, edge, "cross-receipt Defines is not restorable")
	reverse, err := pendingGet(b.pending, pendingReverseKey(defines, key), copyValue)
	require.NoError(t, err)
	require.Nil(t, reverse)
}

func TestRebuildHeldManifestSupersession(t *testing.T) {
	for _, mode := range []string{"native writer", "modules disabled", "unrelated edge", "changed node", "changed edge", "unrelated node"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			b := newBuild(t, FullProfile)
			const file = "package.json"
			raw := &graph.Node{ID: file, Name: file, FilePath: file, Kind: graph.KindFile, Language: "json", StartLine: 1}
			decl := &graph.Node{ID: file + "::name", Name: "name", FilePath: file, Kind: graph.KindVariable, Language: "json"}
			defines := &graph.Edge{From: file, To: decl.ID, Kind: graph.EdgeDefines, FilePath: file}
			require.NoError(t, b.AddRebuildDeferredFile("", ParseFacts{
				File: graph.FileMetaRow{FilePath: file, NodeCount: 2}, EffectiveLanguage: "json", Nodes: []*graph.Node{raw, decl}, Edges: []*graph.Edge{defines},
			}, func(string) (bool, error) { return true, nil }))
			require.NoError(t, b.IsolateRebuildLinks(ctx))
			winner := &graph.Node{ID: file, Name: file, FilePath: file, Kind: graph.KindFile, Language: "json"}
			module := &graph.Node{ID: "module::npm:a@1", Name: "a", Kind: graph.KindModule, FilePath: file, Language: "typescript"}
			dependency := &graph.Edge{From: file, To: module.ID, Kind: graph.EdgeDependsOnModule, FilePath: file, Origin: graph.OriginASTResolved}
			if mode == "unrelated node" {
				b.AddNode(winner)
				require.ErrorIs(t, b.AddRebuildManifestArtifacts(ctx, []*graph.Node{winner, module}, []*graph.Edge{dependency}), ErrRebuildLinkIsolation)
				return
			}
			if mode != "modules disabled" {
				require.NoError(t, b.AddRebuildManifestArtifacts(ctx, []*graph.Node{winner, module}, []*graph.Edge{dependency}))
				n, err := b.RebuildView().Node(ctx, file)
				require.NoError(t, err)
				require.Equal(t, winner, n)
			}
			n, err := b.RebuildView().Node(ctx, decl.ID)
			require.NoError(t, err)
			require.Nil(t, n, "the raw declaration never enters the cut")
			switch mode {
			case "unrelated edge":
				b.AddEdge(&graph.Edge{From: module.ID, To: file, Kind: graph.EdgeReferences, FilePath: "other.go"})
			case "changed node":
				changed := *winner
				changed.Name = "changed later"
				b.AddNode(&changed)
			case "changed edge":
				changed := *dependency
				changed.Meta = map[string]any{"changed": true}
				b.AddEdge(&changed)
			}
			err = b.RestoreRebuildDeclarations(ctx)
			if mode != "native writer" && mode != "modules disabled" {
				require.ErrorIs(t, err, ErrRebuildLinkIsolation)
				return
			}
			require.NoError(t, err)
			n, err = b.RebuildView().Node(ctx, file)
			require.NoError(t, err)
			if mode == "modules disabled" {
				require.Equal(t, raw, n)
			} else {
				require.Equal(t, winner, n)
				edge, err := b.pendingEdge(pendingEdgeKey(dependency))
				require.NoError(t, err)
				require.Equal(t, dependency, edge)
			}
			edge, err := b.pendingEdge(pendingEdgeKey(defines))
			require.NoError(t, err)
			require.Equal(t, defines, edge)
		})
	}
}

func TestRebuildHeldEndpointCoverageUsesFirstReceipt(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	held := &graph.Node{ID: "a.py::A", FilePath: "a.py", Kind: graph.KindFunction, Language: "go"}
	caller := &graph.Node{ID: "caller.go::Run", FilePath: "caller.go", Kind: graph.KindFunction, Language: "python"}
	target := &graph.Node{ID: "target.go::Run", FilePath: "target.go", Kind: graph.KindFunction}
	keep := func(string) (bool, error) { return true, nil }
	incoming := &graph.Edge{From: held.ID, To: target.ID, FilePath: held.FilePath, Kind: graph.EdgeReferences}
	outgoing := &graph.Edge{From: caller.ID, To: held.ID, FilePath: "shared.go", Kind: graph.EdgeCalls}
	duplicate := *outgoing
	duplicate.Origin = graph.OriginASTInferred
	require.NoError(t, b.AddRebuildDeferredFile("", ParseFacts{
		File: graph.FileMetaRow{FilePath: held.FilePath, NodeCount: 1}, EffectiveLanguage: "python",
		Nodes: []*graph.Node{held}, Edges: []*graph.Edge{incoming},
	}, keep))
	for _, f := range []ParseFacts{
		{File: graph.FileMetaRow{FilePath: "first.go"}, EffectiveLanguage: "go", Edges: []*graph.Edge{outgoing}},
		{File: graph.FileMetaRow{FilePath: "second.go"}, EffectiveLanguage: "go", Edges: []*graph.Edge{&duplicate}},
		{File: graph.FileMetaRow{FilePath: caller.FilePath, NodeCount: 1}, EffectiveLanguage: "go", Nodes: []*graph.Node{caller}},
		{File: graph.FileMetaRow{FilePath: target.FilePath, NodeCount: 1}, EffectiveLanguage: "go", Nodes: []*graph.Node{target}},
	} {
		require.NoError(t, b.AddParsedFileSelected("", f, keep, true))
	}
	require.NoError(t, b.IsolateRebuildLinks(ctx))
	require.NoError(t, b.RestoreRebuildDeclarations(ctx))
	require.NoError(t, b.CheckRebuildIsolationCoverage([]string{"python"}))
	coverage, err := b.prepareIsolationScopes(ctx)
	require.NoError(t, err)
	require.Equal(t, []RebuildIsolationDependency{{SupportedLanguage: "go", DeferredLanguage: "python"}}, coverage.IsolationDependencies)
	var scopes []RebuildIsolationAffectedScope
	require.NoError(t, pendingVisit(ctx, b.pending, []byte{familyIsolation}, func(r row) error {
		scope, err := decodeIsolationScope(r.key, r.value)
		scopes = append(scopes, scope)
		return err
	}))
	require.Equal(t, []RebuildIsolationAffectedScope{
		{FilePath: "caller.go", SupportedLanguage: "go", DeferredLanguage: "python", Cause: "held-endpoint", Count: 1},
		{FilePath: "first.go", SupportedLanguage: "go", DeferredLanguage: "python", Cause: "held-endpoint", Count: 1},
		{FilePath: "target.go", SupportedLanguage: "go", DeferredLanguage: "python", Cause: "held-endpoint", Count: 1},
	}, scopes)
}

func TestRebuildHeldEndpointCoverageCheckedSkip(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	held := &graph.Node{ID: "skip.py::A", FilePath: "skip.py", Kind: graph.KindFunction}
	skip := &graph.Node{ID: "skip.go", FilePath: "skip.go", Kind: graph.KindFile, Language: "go"}
	edge := &graph.Edge{From: held.ID, To: skip.ID, FilePath: held.FilePath, Kind: graph.EdgeReferences}
	require.NoError(t, b.AddRebuildDeferredFile("", ParseFacts{
		File: graph.FileMetaRow{FilePath: held.FilePath, NodeCount: 1}, EffectiveLanguage: "python",
		Nodes: []*graph.Node{held}, Edges: []*graph.Edge{edge},
	}, func(string) (bool, error) { return true, nil }))
	require.NoError(t, b.AddRebuildSelectedSkip(skip))
	require.NoError(t, b.IsolateRebuildLinks(ctx))
	require.NoError(t, b.RestoreRebuildDeclarations(ctx))
	require.NoError(t, b.CheckRebuildIsolationCoverage([]string{"python"}))
	_, err := b.prepareIsolationScopes(ctx)
	require.NoError(t, err)
	var scopes []RebuildIsolationAffectedScope
	require.NoError(t, pendingVisit(ctx, b.pending, []byte{familyIsolation}, func(r row) error {
		scope, err := decodeIsolationScope(r.key, r.value)
		scopes = append(scopes, scope)
		return err
	}))
	require.Equal(t, []RebuildIsolationAffectedScope{{FilePath: skip.FilePath, SupportedLanguage: "go", DeferredLanguage: "python", Cause: "held-endpoint", Count: 1}}, scopes)
}

func TestRebuildHeldEndpointWithoutReceiptAttributionRefuses(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	held := &graph.Node{ID: "held.py::A", FilePath: "held.py", Kind: graph.KindFunction}
	require.NoError(t, b.AddRebuildDeferredFile("", ParseFacts{
		File: graph.FileMetaRow{FilePath: held.FilePath, NodeCount: 1}, EffectiveLanguage: "python", Nodes: []*graph.Node{held},
	}, func(string) (bool, error) { return true, nil }))
	b.AddEdge(&graph.Edge{From: "a.go::Run", To: held.ID, FilePath: "a.go", Kind: graph.EdgeReferences})
	err := b.IsolateRebuildLinks(ctx)
	require.ErrorIs(t, err, ErrRebuildLinkIsolation)
	require.ErrorContains(t, err, "supported held-endpoint edge lacks receipt attribution")
}

package indexer

import (
	"cmp"
	"encoding/hex"
	"maps"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

func TestRebuildTestFilesCheckedSizeSkip(t *testing.T) {
	ctx := t.Context()
	const (
		path        = "pkg/a_test.go"
		size  int64 = 128
		limit int64 = 64
	)
	row := rebuildClassificationSidecarRow(path, "go", size, limit, nil, false)
	require.NoError(t, row.checkClassification())
	classifications := map[string]rebuildClassificationFact{path: *row.Classification}

	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	cfg := config.Default().Index
	cfg.MaxFileSize = limit
	idx := New(graph.New(), reg, cfg, zap.NewNop())
	t.Cleanup(idx.Close)

	build, workDir := openRebuildTestFileStore(t)
	emptyRoot, err := rebuildInputRoot(nil)
	require.NoError(t, err)
	rootBytes, err := hex.DecodeString(emptyRoot)
	require.NoError(t, err)
	job := rebuildFileStages{
		workDir: workDir, incarnation: rebuildTestIncarnation, classifications: classifications,
		witness: store_pebble.Witness{
			Input: store_pebble.HeadDigest(rootBytes), Recipe: store_pebble.HeadDigest{1},
			Profile: store_pebble.FullProfile, Stage: store_pebble.StageManifest,
		},
	}
	// The real assembly checks the size fact. It must not need a content
	// source or produce a receipt for this manifest with no parse leaves.
	counts, err := idx.buildRebuildFileStages(ctx, nil, rebuildParseStore{}, build, job, func(store_pebble.StageTables) error { return nil })
	require.NoError(t, err)
	require.Zero(t, counts.parsed)
	require.Zero(t, counts.reused)
	require.NotNil(t, counts.census)
	require.Empty(t, counts.census.byPath)
	require.Zero(t, counts.census.admitted)

	nativeStub := sizeSkipNode(skippedFile{relPath: path, lang: "go", size: size}, limit)
	saved, err := build.RebuildView().Node(ctx, path)
	require.NoError(t, err)
	require.Equal(t, nativeStub, saved)
	want := &graph.Node{
		ID: path, Kind: graph.KindFile, Name: "a_test.go", FilePath: path, Language: "go",
		Meta: map[string]any{
			"skip_reason": "size", "skipped_due_to_size": true,
			"file_size_bytes": size, "max_file_size_bytes": limit,
			"is_test_file": true, "test_runner": "gotest",
		},
	}
	require.Equal(t, want, nativeTestFileStamp(t, nativeStub, nil, "gotest"))
	require.NoError(t, runRebuildTestFiles(ctx, build, noRebuildTestFileReceipts, classifications))
	requireRebuildTestFileRows(t, build, []*graph.Node{want}, nil)

	t.Run("unmatched_synthetic_size_lookalike", func(t *testing.T) {
		unproved, _ := openRebuildTestFileStore(t)
		unproved.AddNode(nativeStub)
		requireRebuildTestFileRows(t, unproved, []*graph.Node{nativeStub}, nil)
		err := runRebuildTestFiles(t.Context(), unproved, noRebuildTestFileReceipts, nil)
		var unknown *store_pebble.RebuildUnknownProvenanceError
		require.ErrorAs(t, err, &unknown)
		require.EqualValues(t, 1, unknown.Count)
		requireRebuildTestFileRows(t, unproved, []*graph.Node{nativeStub}, nil)
	})
}

func TestRebuildTestFilesPrivateGoScope(t *testing.T) {
	ctx := t.Context()
	const path = "pkg/a_test.go"
	src := []byte("package p\nfunc use(xs []int) int { return len(xs) }\n")
	idx := newTestIndexer(graph.New())
	t.Cleanup(idx.Close)
	idx.SetRootPath(t.TempDir())
	facts, err := idx.extractPreparedRebuildFile(ctx, path, languages.NewGoExtractor(), src)
	require.NoError(t, err)
	const key = store_pebble.RebuildGoBuiltinScopeReceiptMeta
	var file *graph.Node
	for _, n := range facts.Nodes {
		if n.Kind == graph.KindFile && n.ID == path {
			file = n
			break
		}
	}
	require.NotNil(t, file)
	require.Equal(t, "p", file.Meta[key], "real extraction must carry package evidence")
	receipts := func(yield func(store_pebble.ParseFacts, error) bool) { yield(facts, nil) }
	for _, changed := range []bool{false, true} {
		name := "private_key_only"
		if changed {
			name = "changed_pending_metadata"
		}
		t.Run(name, func(t *testing.T) {
			build, _ := openRebuildTestFileStore(t)
			require.NoError(t, build.AddParsedFile("", facts))
			view := build.RebuildView()
			before, err := view.Node(ctx, path)
			require.NoError(t, err)
			require.NotNil(t, before)
			require.NotContains(t, before.Meta, key)
			if changed {
				before.Meta["unrelated"] = "changed"
				build.AddNode(before)
			}
			err = runRebuildTestFiles(ctx, build, receipts, nil)
			if changed {
				var unknown *store_pebble.RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &unknown)
				require.EqualValues(t, 1, unknown.Count)
				require.Equal(t, path, unknown.First.From)
				after, err := view.Node(ctx, path)
				require.NoError(t, err)
				require.Equal(t, before, after)
				return
			}
			require.NoError(t, err)
			after, err := view.Node(ctx, path)
			require.NoError(t, err)
			require.Equal(t, nativeTestFileStamp(t, before, nil, "gotest"), after)
			require.Equal(t, "p", file.Meta[key], "proof must not consume the receipt")
		})
	}
}

func TestRebuildTestFilesCheckedDeferredFile(t *testing.T) {
	const py = "test_x.py"
	for _, tc := range []struct {
		name, path, language, runner string
		stamped                      bool
		receiptImport, workingImport bool
		legacy, parseError, conflict bool
	}{
		{name: "python_without_imports", path: py, language: "python", stamped: true, runner: "pytest"},
		{name: "java_without_imports", path: "FooTest.java", language: "java", stamped: true},
		{name: "import_dependent", path: py, language: "python", receiptImport: true, workingImport: true},
		{name: "receipt_import_missing_from_working_graph", path: py, language: "python", receiptImport: true},
		{name: "working_import_missing_from_receipt", path: py, language: "python", workingImport: true},
		{name: "missing_extractor_authority", path: py, language: "python", legacy: true},
		{name: "parse_error_does_not_prove_import_absence", path: py, language: "python", parseError: true},
		{name: "conflicting_duplicate", path: py, language: "python", conflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			build, _ := openRebuildTestFileStore(t)
			file := &graph.Node{ID: tc.path, Kind: graph.KindFile, Name: tc.path, FilePath: tc.path,
				Language: tc.language, Meta: map[string]any{"keep": "raw metadata"}}
			imp := &graph.Edge{From: tc.path, To: "unresolved::import::unittest", Kind: graph.EdgeImports,
				FilePath: tc.path, Line: 1, Origin: graph.OriginASTResolved}
			facts := store_pebble.ParseFacts{
				File:              graph.FileMetaRow{FilePath: tc.path, NodeCount: 1},
				EffectiveLanguage: tc.language,
				Nodes:             []*graph.Node{file},
			}
			if tc.legacy {
				facts.EffectiveLanguage = ""
			}
			if tc.parseError {
				facts.File.Errors = "syntax error"
			}
			if tc.receiptImport {
				facts.Edges = []*graph.Edge{imp}
				nativeTestFileStamp(t, file, facts.Edges, "unittest")
			}
			working := facts
			working.Edges = nil
			if tc.workingImport {
				working.Edges = []*graph.Edge{imp}
			}
			require.NoError(t, build.AddParsedFile("", working))
			requireRebuildTestFileRows(t, build, []*graph.Node{file}, working.Edges)

			receipts := func(yield func(store_pebble.ParseFacts, error) bool) {
				if yield(facts, nil) && tc.conflict {
					other := facts
					other.EffectiveLanguage = "ruby"
					yield(other, nil)
				}
			}
			err := runRebuildTestFiles(t.Context(), build, receipts, nil)
			if tc.conflict {
				var unknown *store_pebble.RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &unknown)
				require.EqualValues(t, 1, unknown.Count)
				require.Equal(t, tc.path, unknown.First.From)
			} else {
				require.NoError(t, err)
			}
			want := file
			if tc.stamped {
				want = nativeTestFileStamp(t, file, nil, tc.runner)
			}
			requireRebuildTestFileRows(t, build, []*graph.Node{want}, working.Edges)
			require.Equal(t, map[string]any{"keep": "raw metadata"}, file.Meta, "raw node is not mutated")
		})
	}
}

func noRebuildTestFileReceipts(func(store_pebble.ParseFacts, error) bool) {}

const rebuildTestTSPath = "case.test.ts"

func TestRebuildTestFilesRunnerUsesPendingKeyOrder(t *testing.T) {
	const mocha, vitest = "mocha/index.ts", "vitest/index.ts"
	file := &graph.Node{ID: rebuildTestTSPath, Kind: graph.KindFile, Name: rebuildTestTSPath,
		FilePath: rebuildTestTSPath, Language: "typescript"}
	// Line order is the opposite of complete pending-key order.
	v := rebuildTestFileImport(vitest, 2)
	m := rebuildTestFileImport(mocha, 9)
	require.NotContains(t, file.Meta, "test_runner")
	nativeTestFileStamp(t, file, []*graph.Edge{v, m}, "vitest")
	nativeTestFileStamp(t, file, []*graph.Edge{m, v}, "mocha")

	for _, tc := range []struct {
		name, parserRunner, wantRunner string
		imports                        []*graph.Edge
	}{
		{name: "pending_key", wantRunner: "mocha", imports: []*graph.Edge{v, m}},
		{name: "parser_stamp", parserRunner: "jest", wantRunner: "jest", imports: []*graph.Edge{v, m}},
		{name: "no_signal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := *file
			if tc.parserRunner != "" {
				input.Meta = map[string]any{"test_runner": tc.parserRunner}
			}
			ordered := slices.Clone(tc.imports)
			slices.SortFunc(ordered, compareRebuildTestFileEdgeKeys)
			if len(ordered) != 0 {
				require.Equal(t, []*graph.Edge{m, v}, ordered)
			}
			want := nativeTestFileStamp(t, &input, ordered, tc.wantRunner)
			build, _ := openRebuildTestFileStore(t)
			facts := seedRebuildTestFileImports(t, build, &input, tc.imports)
			requireRebuildTestFileRows(t, build, []*graph.Node{&input}, ordered)
			receipts := func(yield func(store_pebble.ParseFacts, error) bool) { yield(facts, nil) }
			require.NoError(t, runRebuildTestFiles(t.Context(), build, receipts, nil))
			requireRebuildTestFileRows(t, build, []*graph.Node{want}, ordered)
			require.NotContains(t, facts.Nodes[0].Meta, "is_test_file", "raw receipt stays unstamped")
		})
	}
}

func TestRebuildTestFilesRefusesMaterialUnknownRunner(t *testing.T) {
	file := &graph.Node{ID: rebuildTestTSPath, Kind: graph.KindFile, Name: rebuildTestTSPath,
		FilePath: rebuildTestTSPath, Language: "typescript"}
	mocha := rebuildTestFileImport("mocha/index.ts", 9)
	unknownMocha := rebuildTestFileImport("@types/mocha", 2)
	unknownCypress := rebuildTestFileImport("cypress", 3)
	settled := []*graph.Edge{unknownMocha, unknownCypress, mocha}
	require.True(t, slices.IsSortedFunc(settled, compareRebuildTestFileEdgeKeys))
	// Removing every unknown row leaves the same winner. Removing just the
	// first unknown row changes the winner; this ambiguity is material.
	nativeTestFileStamp(t, file, settled, "mocha")
	nativeTestFileStamp(t, file, settled[1:], "cypress")
	nativeTestFileStamp(t, file, settled[2:], "mocha")

	build, _ := openRebuildTestFileStore(t)
	facts := seedRebuildTestFileImports(t, build, file, []*graph.Edge{mocha})
	build.AddBatch(nil, []*graph.Edge{unknownCypress, unknownMocha})
	// A proved file sorts first and must not be stamped on refusal.
	earlier := &graph.Node{ID: "a.test.ts", Kind: graph.KindFile, Name: "a.test.ts", FilePath: "a.test.ts", Language: "typescript"}
	earlierFacts := seedRebuildTestFileImports(t, build, earlier, nil)
	nodes := []*graph.Node{earlier, file}
	requireRebuildTestFileRows(t, build, nodes, settled)
	receipts := func(yield func(store_pebble.ParseFacts, error) bool) {
		if yield(earlierFacts, nil) {
			yield(facts, nil)
		}
	}
	err := runRebuildTestFiles(t.Context(), build, receipts, nil)
	var unknown *store_pebble.RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &unknown)
	require.EqualValues(t, 1, unknown.Count, "count the affected file, not candidate imports")
	requireRebuildTestFileRows(t, build, nodes, settled)
}

func rebuildTestFileImport(target string, line int) *graph.Edge {
	return &graph.Edge{From: rebuildTestTSPath, Kind: graph.EdgeImports, To: target,
		FilePath: rebuildTestTSPath, Line: line, Origin: graph.OriginASTResolved}
}

func compareRebuildTestFileEdgeKeys(a, b *graph.Edge) int {
	return cmp.Or(cmp.Compare(a.From, b.From), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.To, b.To),
		cmp.Compare(a.FilePath, b.FilePath), cmp.Compare(a.Line, b.Line))
}

func nativeTestFileStamp(t *testing.T, file *graph.Node, edges []*graph.Edge, runner string) *graph.Node {
	t.Helper()
	n := *file
	n.Meta = maps.Clone(file.Meta)
	var ownEdges []*graph.Edge
	for _, e := range edges {
		copy := *e
		copy.Meta = maps.Clone(e.Meta)
		ownEdges = append(ownEdges, &copy)
	}
	g := graph.New()
	g.AddBatch([]*graph.Node{&n}, ownEdges)
	require.Equal(t, append([]*graph.Edge{}, edges...), g.GetOutEdgesByNodeIDs([]string{n.ID})[n.ID], "native adjacency")
	marked, emitted := markTestSymbolsAndEmitEdges(g)
	require.Zero(t, marked)
	require.Zero(t, emitted)
	require.Equal(t, 1, g.NodeCount())
	require.ElementsMatch(t, edges, g.AllEdges())
	want := *file
	want.Meta = maps.Clone(file.Meta)
	if want.Meta == nil {
		want.Meta = make(map[string]any)
	}
	want.Meta["is_test_file"] = true
	if runner != "" {
		want.Meta["test_runner"] = runner
	}
	got := g.GetNode(file.ID)
	require.Equal(t, &want, got)
	return got
}

func seedRebuildTestFileImports(t *testing.T, build *store_pebble.BuildStore, file *graph.Node, settled []*graph.Edge) store_pebble.ParseFacts {
	t.Helper()
	n := *file
	n.Meta = maps.Clone(file.Meta)
	facts := store_pebble.ParseFacts{
		File:              graph.FileMetaRow{FilePath: n.FilePath, NodeCount: 1},
		EffectiveLanguage: "typescript", Nodes: []*graph.Node{&n},
	}
	for _, e := range settled {
		raw := *e
		raw.To = "unresolved::import::./" + e.To
		raw.Meta = maps.Clone(e.Meta)
		facts.Edges = append(facts.Edges, &raw)
	}
	require.NoError(t, build.AddParsedFile("", facts))
	// Linking moves only To. The checked receipt still has the exact
	// From, kind, recording path and line of the supported import.
	moves := make([]graph.EdgeReindex, 0, len(settled))
	for i, e := range settled {
		moves = append(moves, graph.EdgeReindex{Edge: e, OldTo: facts.Edges[i].To})
	}
	slices.SortFunc(moves, func(a, b graph.EdgeReindex) int { return cmp.Compare(a.OldTo, b.OldTo) })
	require.NoError(t, build.ApplyLinkReindexes(t.Context(), moves))
	return facts
}

func openRebuildTestFileStore(t *testing.T) (*store_pebble.BuildStore, string) {
	t.Helper()
	workDir := t.TempDir()
	build, err := store_pebble.OpenBuild(filepath.Join(workDir, "working"), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, build.Close()) })
	return build, workDir
}

func requireRebuildTestFileRows(t *testing.T, build *store_pebble.BuildStore, wantNodes []*graph.Node, wantEdges []*graph.Edge) {
	t.Helper()
	view := build.RebuildView()
	var nodes []*graph.Node
	require.NoError(t, view.VisitNodes(t.Context(), func(n *graph.Node) error {
		nodes = append(nodes, n)
		return nil
	}))
	require.ElementsMatch(t, wantNodes, nodes)
	var edges []*graph.Edge
	require.NoError(t, view.VisitEdges(t.Context(), func(e *graph.Edge) error {
		edges = append(edges, e)
		return nil
	}))
	require.Equal(t, wantEdges, edges, "complete pending-order rows")
}

package indexer

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/resolver"
)

const (
	returnsToFile      = "probe.go"
	returnsToOtherFile = "other.go"
	returnsToLine      = 20
	returnsToA         = returnsToFile + "::A"
	returnsToZ         = returnsToFile + "::Z"
	returnsToB         = returnsToFile + "::B.Run"
	returnsToC         = returnsToOtherFile + "::C.Run"
	returnsToLocal     = returnsToA + "#local:value@+2"
)

func TestRebuildGoBuiltinSourcesPublished(t *testing.T) {
	testRebuildGoBuiltinSourcesPublished(t, false)
}

func TestRebuildGoBuiltinSourcePublishedAfterDetour(t *testing.T) {
	testRebuildGoBuiltinSourcesPublished(t, true)
}

func testRebuildGoBuiltinSourcesPublished(t *testing.T, detour bool) {
	t.Helper()
	ctx := t.Context()
	t.Setenv("GORTEX_RESOLVE_FROM_RECONCILE", "1")
	const file = "flow.go"
	source := []byte(`package p
func foo(v int) {}
func f(v int) {}
func use(b []byte, c int, v byte) {
	foo(len(b))
	_ = append(b, byte(c))
	f(int(v))
	n := len(b)
	s := string(b)
	m := make(map[int]string)
	_, _, _ = n, s, m
}
func convert(v byte) int { return int(v) }
`)
	ext := languages.NewGoExtractor()
	extracted, err := ext.Extract(file, source)
	require.NoError(t, err)
	defer extracted.ReleaseTree()
	native := newFTSStore(t)
	require.NoError(t, native.AddBatchChecked(extracted.Nodes, extracted.Edges))
	stats, err := resolver.New(native).ResolveAllContext(ctx)
	require.NoError(t, err)
	require.False(t, stats.Incomplete)
	(&Indexer{graph: native}).materializeDataflowParams()

	idx := newTestIndexer(graph.New())
	t.Cleanup(idx.Close)
	idx.SetRootPath(t.TempDir())
	facts, err := idx.extractPreparedRebuildFile(ctx, file, ext, source)
	require.NoError(t, err)
	build, dir := openRebuildTestFileStore(t)
	require.NoError(t, build.AddParsedFile("", facts))
	w := store_pebble.Witness{Input: store_pebble.HeadDigest{1}, Recipe: store_pebble.HeadDigest{2},
		Stage: store_pebble.StageManifest, Profile: store_pebble.FullProfile, PlatformCatalogRevision: graph.PlatformCatalogRevision}
	manifest, err := store_pebble.PrepareManifest(ctx, t.TempDir(), []store_pebble.ManifestFile{{Path: file, Kind: 'f', Language: "go"}}, w, rebuildTestIncarnation)
	require.NoError(t, err)
	fileGraph, err := build.PrepareFileGraph(ctx, t.TempDir(), manifest.Witness, rebuildTestIncarnation)
	require.NoError(t, err)
	base, err := build.RetainFileGraph(ctx, fileGraph)
	require.NoError(t, err)
	if detour {
		// Return the captured ValueFlow to its extracted source before main
		// reconciles it with the selected Go conversion at the same site.
		view := build.RebuildView()
		rawID := graph.EdgeIdentity{From: "unresolved::int", To: file + "::convert",
			Kind: graph.EdgeValueFlow, FilePath: file, Line: 13}
		raw, err := view.Edge(ctx, rawID)
		require.NoError(t, err)
		require.NotNil(t, raw)
		move := func(from, to graph.Edge) {
			t.Helper()
			build.ReindexEdges([]graph.EdgeReindex{{
				Edge: &to, OldFrom: from.From, OldTo: from.To, OldKind: from.Kind,
				OldFilePath: from.FilePath, OldLine: from.Line, RefreshIdentity: true,
			}})
			prior, err := view.Edge(ctx, graph.EdgeIdentityFor(&from))
			require.NoError(t, err)
			require.Nil(t, prior)
			saved, err := view.Edge(ctx, graph.EdgeIdentityFor(&to))
			require.NoError(t, err)
			require.Equal(t, &to, saved)
		}
		middle := *raw
		middle.From = file + "::use"
		move(*raw, middle)
		move(middle, *raw)
	}

	receipts := func(yield func(store_pebble.ParseFacts, error) bool) { yield(facts, nil) }
	opts := store_pebble.RebuildLinkOptions{ScratchParent: dir, SpillBytes: 1 << 20}
	require.NoError(t, resolver.RunRebuildMain(ctx, build, opts, resolver.RebuildMainInputs{
		SelectCallers: func(page []*graph.Edge) ([]*graph.Edge, error) { return page, nil },
		Receipts:      receipts,
	}))
	require.NoError(t, runRebuildArgOf(ctx, build, receipts))
	require.NoError(t, runRebuildReturnsTo(ctx, build, receipts))
	extractor := func(path string) string {
		if path == facts.File.FilePath {
			return facts.EffectiveLanguage
		}
		return ""
	}
	require.NoError(t, build.SettleRebuildPlatformCalls(ctx, extractor))
	relations, err := build.PrepareRelations(ctx, t.TempDir(), base, extractor)
	require.NoError(t, err)
	head, err := store_pebble.OpenHead(filepath.Join(t.TempDir(), "head"), rebuildTestIncarnation, w.Profile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, head.Close()) })
	for _, stage := range []store_pebble.StageTables{manifest, fileGraph, relations} {
		_, err := head.PublishStage(ctx, stage)
		require.NoError(t, err)
	}
	staged := head.OpenSnapshot()
	t.Cleanup(func() { require.NoError(t, staged.Close()) })
	require.NoError(t, build.Publish(ctx))
	published, err := build.OpenSnapshot()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, published.Close()) })

	// Check each extracted shape against native, then compare every outgoing
	// dataflow identity for its builtin in both serving snapshots.
	const intrinsic = "builtin::go::"
	dataflow := func(edges []*graph.Edge) []graph.EdgeIdentity {
		var ids []graph.EdgeIdentity
		for _, e := range edges {
			if graph.PlaceholderSourceKind(e.Kind) || e.Kind == graph.EdgeReturnsTo {
				ids = append(ids, graph.EdgeIdentityFor(e))
			}
		}
		return ids
	}
	for _, row := range []struct {
		from, to string
		kind     graph.EdgeKind
		line     int
	}{
		{"len", file + "::foo#param:v", graph.EdgeArgOf, 5},
		{"type::byte", intrinsic + "append", graph.EdgeArgOf, 6},
		{"type::int", file + "::f#param:v", graph.EdgeArgOf, 7},
		{"len", file + "::use#local:n@+5", graph.EdgeReturnsTo, 8},
		{"type::string", file + "::use#local:s@+6", graph.EdgeReturnsTo, 9},
		{"make", file + "::use#local:m@+7", graph.EdgeReturnsTo, 10},
		{"type::int", file + "::convert", graph.EdgeValueFlow, 13},
	} {
		id := intrinsic + row.from
		want := dataflow(native.GetOutEdges(id))
		require.Contains(t, want, graph.EdgeIdentity{From: id, To: row.to, Kind: row.kind, FilePath: file, Line: row.line})
		for _, snap := range []*store_pebble.Snapshot{staged, published} {
			require.Nil(t, snap.GetNode(id))
			require.ElementsMatch(t, want, dataflow(snap.GetOutEdges(id)))
		}
	}
}

func TestRebuildReturnsToNativeOrder(t *testing.T) {
	calls := []*graph.Edge{
		rebuildReturnsToCall(returnsToA, returnsToB, returnsToFile, returnsToLine),
		rebuildReturnsToCall(returnsToZ, returnsToB, returnsToFile, returnsToLine),
	}
	first := rebuildReturnsToFixtureRow(returnsToA, "first old key", 0.75)
	second := rebuildReturnsToFixtureRow(returnsToZ, "second old key", 0.5)
	firstWinner, secondWinner := *first, *second
	firstWinner.From, secondWinner.From = returnsToB, returnsToB

	// Same complete destination key, different full payloads. The first
	// native arrival is the winner, but A sorts before Z by complete old key.
	locked := rebuildReturnsToNativeRows(t, calls, []*graph.Edge{first, second}, &firstWinner)
	rebuildReturnsToNativeRows(t, calls, []*graph.Edge{second, first}, &secondWinner)
	for _, tc := range []struct {
		name    string
		returns []*graph.Edge
	}{
		{"first-key-first", []*graph.Edge{first, second}},
		{"first-key-last", []*graph.Edge{second, first}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rebuildReturnsToCheckPrivate(t, slices.Concat(calls, tc.returns), locked)
		})
	}
}

func TestRebuildReturnsToNativeControls(t *testing.T) {
	old := rebuildReturnsToFixtureRow(returnsToA, "original payload", 0.75)
	lifted := *old
	lifted.From = returnsToB
	stationary := rebuildReturnsToFixtureRow(returnsToB, "stationary payload", 0.5)
	call := rebuildReturnsToCall(returnsToA, returnsToB, returnsToFile, returnsToLine)
	wrongLine := rebuildReturnsToCall(returnsToA, returnsToC, returnsToFile, returnsToLine-1)
	for _, tc := range []struct {
		name           string
		calls, returns []*graph.Edge
		want           []*graph.Edge
	}{
		{"stationary-destination", []*graph.Edge{call}, []*graph.Edge{old, stationary}, []*graph.Edge{stationary}},
		{"wrong-line-keeps-original", []*graph.Edge{wrongLine}, []*graph.Edge{old}, []*graph.Edge{old}},
		// Native selection keys on caller and line, not the edge's recorded file.
		{"same-line-in-other-file", []*graph.Edge{wrongLine,
			rebuildReturnsToCall(returnsToA, returnsToB, returnsToOtherFile, returnsToLine)},
			[]*graph.Edge{old}, []*graph.Edge{&lifted}},
		// B.Run can itself select C.Run at this line, but the rewritten row
		// is not input to this invocation. Its marker and payload stay intact.
		{"one-hop-per-invocation", []*graph.Edge{call,
			rebuildReturnsToCall(returnsToB, returnsToC, returnsToFile, returnsToLine)},
			[]*graph.Edge{old}, []*graph.Edge{&lifted}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			native := rebuildReturnsToNativeRows(t, tc.calls, tc.returns, tc.want...)
			rebuildReturnsToCheckPrivate(t, slices.Concat(tc.calls, tc.returns), native)
		})
	}
}

func rebuildReturnsToNativeRows(t *testing.T, calls, returns []*graph.Edge, wantReturns ...*graph.Edge) []*graph.Edge {
	t.Helper()
	native, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, native.Close()) })
	require.NoError(t, native.AddBatchChecked(rebuildReturnsToNodes(), calls))
	// One committed insert per return forces the order of the native row-id scan.
	for _, edge := range returns {
		require.NoError(t, native.AddBatchChecked(nil, []*graph.Edge{edge}))
	}
	var arrival []*graph.Edge
	native.ScanEdgesByKindsBatched([]graph.EdgeKind{graph.EdgeReturnsTo}, dataflowRewriteBatchSize, func(rows []*graph.Edge) bool {
		arrival = append(arrival, rows...)
		return true
	})
	require.Equal(t, returns, arrival, "raw SQLite arrival order")
	require.ElementsMatch(t, slices.Concat(calls, returns), native.AllEdges(), "native input")
	(&Indexer{graph: native}).materializeDataflowParams()
	rows := native.AllEdges()
	require.ElementsMatch(t, slices.Concat(calls, wantReturns), rows, "native full persisted rows")
	return rows
}

func rebuildReturnsToCheckPrivate(t *testing.T, input, want []*graph.Edge) {
	t.Helper()
	ctx := t.Context()
	nodes := rebuildReturnsToNodes()
	build, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "working"), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, build.Close()) })
	build.AddBatch(nodes, input)
	read := func() []*graph.Edge {
		t.Helper()
		var rows []*graph.Edge
		require.NoError(t, build.RebuildView().VisitEdges(ctx, func(e *graph.Edge) error {
			rows = append(rows, e)
			return nil
		}))
		return rows
	}
	require.ElementsMatch(t, input, read(), "private input")

	// This fixture supplies post-resolution rows and matching pre-link
	// receipts. It does not prove that extraction or corpus ingest produces
	// these collisions, or that corpus SQLite arrival agrees with key order.
	receipts := func(yield func(store_pebble.ParseFacts, error) bool) {
		for _, path := range []string{returnsToOtherFile, returnsToFile} {
			facts := store_pebble.ParseFacts{File: graph.FileMetaRow{FilePath: path}, EffectiveLanguage: "go"}
			for _, n := range nodes {
				if n.FilePath == path {
					facts.Nodes = append(facts.Nodes, n)
				}
			}
			facts.File.NodeCount = len(facts.Nodes)
			for _, e := range input {
				if e.FilePath != path {
					continue
				}
				raw := *e
				if raw.Kind == graph.EdgeCalls {
					raw.To, raw.Origin = "unresolved::*.Run", graph.OriginASTInferred
				}
				facts.Edges = append(facts.Edges, &raw)
			}
			if !yield(facts, nil) {
				return
			}
		}
	}
	require.NoError(t, runRebuildReturnsTo(ctx, build, receipts))
	require.ElementsMatch(t, want, read(), "private full persisted rows, including unchanged rows")
}

func rebuildReturnsToNodes() []*graph.Node {
	return []*graph.Node{
		{ID: returnsToA, Name: "A", Kind: graph.KindFunction, FilePath: returnsToFile, Language: "go"},
		{ID: returnsToZ, Name: "Z", Kind: graph.KindFunction, FilePath: returnsToFile, Language: "go"},
		{ID: returnsToB, Name: "Run", Kind: graph.KindMethod, FilePath: returnsToFile, Language: "go"},
		{ID: returnsToC, Name: "Run", Kind: graph.KindMethod, FilePath: returnsToOtherFile, Language: "go"},
		{ID: returnsToLocal, Name: "value", Kind: graph.KindLocal, FilePath: returnsToFile, Language: "go"},
	}
}

func rebuildReturnsToCall(from, to, path string, line int) *graph.Edge {
	return &graph.Edge{From: from, To: to, Kind: graph.EdgeCalls, FilePath: path, Line: line,
		Origin: graph.OriginASTResolved, Tier: "ast", Confidence: 0.75, ConfidenceLabel: "medium",
		Meta: map[string]any{"fixture_tag": "unchanged call"}}
}

func rebuildReturnsToFixtureRow(from, payload string, confidence float64) *graph.Edge {
	return &graph.Edge{From: from, To: returnsToLocal, Kind: graph.EdgeReturnsTo,
		FilePath: returnsToFile, Line: returnsToLine, Origin: graph.OriginASTInferred, Tier: "ast",
		Confidence: confidence, ConfidenceLabel: "medium",
		Meta: map[string]any{"returns_to_call": true, "call_line": returnsToLine,
			"callee_target": "unresolved::*.Run", "return_position": 0, "fixture_tag": payload}}
}

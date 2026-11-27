package indexer

import (
	"crypto/sha256"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// This checks the private main cut only. No stage preparation, offer, or
// projection runs here; later passes and corpus parity need separate proof.
func TestRebuildCPendingProjection(t *testing.T) {
	const cPath, goPath = "src/main.c", "p/control.go"
	ctx := t.Context()
	t.Setenv("GORTEX_TEST_REBUILD_BUILD_CHILD", "1")
	lease := t.TempDir()
	build, err := store_pebble.OpenBuild(filepath.Join(lease, "working"), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, build.Close()) })
	census := rebuildExtractorCensus{admitted: 2, byPath: map[string]string{}, originByGraphPath: map[string]string{}}
	for _, file := range []struct {
		path, source string
		extractor    parser.Extractor
	}{
		{cPath, `void c_leaf(void) {}
void c_entry(void) {
    c_leaf();
}
`, languages.NewCExtractor()},
		{goPath, `package p
func goLeaf() {}
func goEntry() {
    goLeaf()
}
`, languages.NewGoExtractor()},
	} {
		raw := []byte(file.source)
		parsed, err := file.extractor.Extract(file.path, raw)
		require.NoError(t, err)
		t.Cleanup(parsed.ReleaseTree)
		require.True(t, parsed.ParseChecked, file.path)
		require.Zero(t, parsed.ParseErrors, file.path)
		content := contentHashForSource(raw)
		receipt := encodeRebuildTestParseFacts(t, store_pebble.ParseFacts{
			File:  graph.FileMetaRow{FilePath: file.path, ContentHash: content, Size: len(raw), NodeCount: len(parsed.Nodes)},
			Nodes: parsed.Nodes, Edges: parsed.Edges, EffectiveLanguage: file.extractor.Language(),
		})
		facts := decodeRebuildTestParseFacts(t, receipt)
		require.Equal(t, file.path, facts.File.FilePath)
		require.Equal(t, file.extractor.Language(), facts.EffectiveLanguage)
		census.byPath[facts.File.FilePath] = facts.EffectiveLanguage
		require.NoError(t, recordRebuildGraphPath(census.originByGraphPath, facts.File.FilePath))
		require.NoError(t, build.AddParsedFile("", facts))
		t.Logf("%s source sha256=%x; receipt sha256=%x", file.path, sha256.Sum256(raw), sha256.Sum256(receipt))
	}
	require.Equal(t, map[string]string{cPath: "c", goPath: "go"}, census.byPath)

	cRaw := graph.Edge{From: cPath + "::c_entry", To: "unresolved::c_leaf", Kind: graph.EdgeCalls, FilePath: cPath, Line: 3}
	cOld := graph.EdgeIdentityFor(&cRaw)
	cBound := cOld
	cBound.To = cPath + "::c_leaf"
	goRaw := graph.Edge{From: goPath + "::goEntry", To: "unresolved::goLeaf", Kind: graph.EdgeCalls, FilePath: goPath, Line: 4,
		Meta: map[string]any{graph.MetaReturnUsage: graph.ReturnUsageDiscarded}}
	goOld := graph.EdgeIdentityFor(&goRaw)
	goResolved := goRaw
	goResolved.To, goResolved.Origin, goResolved.Confidence = goPath+"::goLeaf", graph.OriginASTResolved, 0.9
	goBound := graph.EdgeIdentityFor(&goResolved)

	view := build.RebuildView()
	readNodes := func() map[string]*graph.Node {
		t.Helper()
		nodes := make(map[string]*graph.Node)
		require.NoError(t, view.VisitNodes(ctx, func(n *graph.Node) error {
			nodes[n.ID] = n
			return nil
		}))
		return nodes
	}
	readCalls := func() map[graph.EdgeIdentity]*graph.Edge {
		t.Helper()
		edges := make(map[graph.EdgeIdentity]*graph.Edge)
		require.NoError(t, view.VisitEdges(ctx, func(e *graph.Edge) error {
			if e.Kind == graph.EdgeCalls {
				key := graph.EdgeIdentityFor(e)
				require.NotContains(t, edges, key)
				edges[key] = e
			}
			return nil
		}))
		return edges
	}
	declarations := readNodes()
	for _, id := range []string{cRaw.From, cBound.To, goRaw.From, goBound.To} {
		require.Contains(t, declarations, id)
		require.Equal(t, graph.KindFunction, declarations[id].Kind)
	}
	require.Equal(t, map[graph.EdgeIdentity]*graph.Edge{cOld: &cRaw, goOld: &goRaw}, readCalls(), "exact pending call rows")

	require.NoError(t, runRebuildFrozenMain(ctx, lease, rebuildDigest{}, build, &census, rebuildTestLinkOptions(t), false))

	require.Equal(t, declarations, readNodes(), "all declarations survive the private cut")
	require.Equal(t, map[graph.EdgeIdentity]*graph.Edge{cOld: &cRaw, goBound: &goResolved}, readCalls(),
		"C must retain its original row, never gain the native settled key, and not affect the Go bind")
}

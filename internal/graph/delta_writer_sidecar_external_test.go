package graph_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestDeltaWriterSidecarSQLiteRefFactsCompose(t *testing.T) {
	// Given real generation sidecars with both shadowed and surviving facts.
	below, err := store_sqlite.Open(filepath.Join(t.TempDir(), "below.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, below.Close()) })
	own, err := store_sqlite.Open(filepath.Join(t.TempDir(), "own.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, own.Close()) })
	old := graph.RefFact{FromID: "old", ToID: "target", FilePath: "repo/a.go", Kind: string(graph.EdgeCalls)}
	rest := graph.RefFact{FromID: "rest", ToID: "target", FilePath: "repo/b.go", Kind: string(graph.EdgeCalls)}
	fresh := graph.RefFact{FromID: "new", ToID: "target", FilePath: "repo/a.go", Kind: string(graph.EdgeCalls)}
	require.NoError(t, below.BulkSetRefFacts("repo", []graph.RefFact{old, rest}))
	dw := graph.NewDeltaWriter(below, own)
	dw.EvictFile("repo/a.go")
	require.NoError(t, dw.BulkSetRefFacts("repo", []graph.RefFact{fresh}))
	fresh.RepoPrefix, rest.RepoPrefix = "repo", "repo"
	// When explicit files, all files or targets are requested repeatedly.
	for range 2 {
		for _, files := range [][]string{{"repo/a.go", "repo/b.go"}, nil} {
			got, err := dw.LoadRefFactsByFiles("repo", files)
			require.NoError(t, err)
			// Then the covered lower fact is masked and both requested pieces survive.
			require.ElementsMatch(t, []graph.RefFact{fresh, rest}, got)
		}
		byFile, err := dw.LoadRefFactsByTargets("repo", []string{"target"})
		require.NoError(t, err)
		require.Equal(t, map[string][]graph.RefFact{"repo/a.go": {fresh}, "repo/b.go": {rest}}, byFile)
	}
	require.NoError(t, dw.DeleteRefFactsByFiles("repo", []string{"repo/a.go"}))
	ownFacts, err := own.LoadRefFactsByFiles("repo", nil)
	require.NoError(t, err)
	require.Empty(t, ownFacts)
	require.NoError(t, dw.BulkSetRefFacts("repo", []graph.RefFact{fresh}))
	require.Empty(t, dw.Unsupported())
	// A graph-only delta with no sidecar has no own fact rows to omit.
	baseOnly := graph.NewDeltaWriter(below, nil)
	baseOnly.EvictFile("repo/a.go")
	baseFacts, err := baseOnly.LoadRefFactsByFiles("repo", nil)
	require.NoError(t, err)
	require.Equal(t, []graph.RefFact{rest}, baseFacts)
}

func TestDeltaWriterSidecarSQLiteForwarding(t *testing.T) {
	// Given a real generation with every sibling sidecar capability.
	own, err := store_sqlite.Open(filepath.Join(t.TempDir(), "sidecars.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, own.Close()) })
	dw := graph.NewDeltaWriter(graph.New(), own)
	// When native writes, replacements and removals pass through the delta.
	require.NoError(t, dw.BulkSetFileMtimes("repo", map[string]int64{"a.go": 1}))
	require.Equal(t, map[string]int64{"a.go": 1}, own.LoadFileMtimes("repo"))
	require.NoError(t, dw.ReplaceFileMtimes("repo", map[string]int64{"b.go": 2}))
	require.Equal(t, map[string]int64{"b.go": 2}, own.LoadFileMtimes("repo"))
	require.NoError(t, dw.DeleteFileMtimes("repo", []string{"b.go"}))
	require.Empty(t, own.LoadFileMtimes("repo"))
	own.AddNode(&graph.Node{ID: "a", Kind: graph.KindFunction, Name: "Symbol", FilePath: "repo/a.go", RepoPrefix: "repo"})
	require.NoError(t, dw.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{{NodeID: "a", Tokens: "uniqueztoken"}}))
	hits, err := own.SearchSymbols("uniqueztoken ", 5)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, "a", hits[0].NodeID)
	require.NoError(t, dw.BatchDeleteSymbolFTS([]string{"a"}))
	hits, err = own.SearchSymbols("uniqueztoken ", 5)
	require.NoError(t, err)
	require.Empty(t, hits)
	require.NoError(t, dw.BulkSetCloneShingles("repo", map[string][]uint64{"a": {1, 2}}))
	shingles, err := own.LoadCloneShingles("repo")
	require.NoError(t, err)
	require.Equal(t, map[string][]uint64{"a": {1, 2}}, shingles)
	require.NoError(t, dw.DeleteCloneShingles([]string{"a"}))
	shingles, err = own.LoadCloneShingles("repo")
	require.NoError(t, err)
	require.Empty(t, shingles)
	require.NoError(t, dw.SetFileMetas("repo", []graph.FileMetaRow{{FilePath: "a.go", ContentHash: "hash"}}))
	metas, err := own.FileMetasForRepo("repo")
	require.NoError(t, err)
	require.Len(t, metas, 1)
	require.Equal(t, "hash", metas[0].ContentHash)
	require.NoError(t, dw.DeleteFileMetasByFiles("repo", []string{"a.go"}))
	metas, err = own.FileMetasForRepo("repo")
	require.NoError(t, err)
	require.Empty(t, metas)
	require.NoError(t, dw.ReplaceFileIndexFailures("repo", []graph.FileIndexFailure{{Path: "a.go", Error: "parse"}}))
	failures, err := own.FileIndexFailuresForRepo("repo")
	require.NoError(t, err)
	require.Len(t, failures, 1)
	require.NoError(t, dw.ReplaceFileIndexFailures("repo", nil))
	failures, err = own.FileIndexFailuresForRepo("repo")
	require.NoError(t, err)
	require.Empty(t, failures)
	// Duplicate files retain the native last-replacement-wins contract.
	require.NoError(t, dw.ReplaceContentFiles("repo", []graph.ContentFTSFileReplacement{
		{FilePath: "a.md", Items: []graph.ContentFTSItem{{NodeID: "section-a", Body: "oldword"}}},
		{FilePath: "a.md", Items: []graph.ContentFTSItem{{NodeID: "section-a", Body: "freshword"}}},
	}))
	require.NoError(t, dw.AppendContent("repo", []graph.ContentFTSItem{{NodeID: "section-b", FilePath: "b.md", Body: "otherword"}}))
	require.NoError(t, dw.BuildContentIndex())
	present, err := dw.ContentRepoHasRows("repo")
	require.NoError(t, err)
	require.True(t, present)
	content, err := dw.SearchContent("freshword", "repo", 5)
	require.NoError(t, err)
	require.Len(t, content, 1)
	scanned := map[string]string{}
	require.NoError(t, dw.ScanContent("repo", func(id, path, body string) bool { scanned[id] = path + ":" + body; return true }))
	require.Equal(t, map[string]string{"section-a": "a.md:freshword", "section-b": "b.md:otherword"}, scanned)
	require.NoError(t, dw.WipeContentFileInRepo("repo", "a.md"))
	require.NoError(t, dw.WipeContentFile("b.md"))
	present, err = dw.ContentRepoHasRows("repo")
	require.NoError(t, err)
	require.False(t, present)
	require.NoError(t, dw.AppendContent("repo", []graph.ContentFTSItem{{NodeID: "section-b", FilePath: "b.md", Body: "again"}}))
	require.NoError(t, dw.WipeContent("repo"))
	present, err = dw.ContentRepoHasRows("repo")
	require.NoError(t, err)
	require.False(t, present)
	// Then no successful native operation is mislabeled as refused.
	require.Empty(t, dw.Unsupported())
}

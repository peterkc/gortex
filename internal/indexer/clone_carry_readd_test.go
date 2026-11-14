package indexer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestCloneCarryIncrementalReadd(t *testing.T) {
	t.Setenv("GORTEX_SHADOW_MAX_FILES", "0")
	// Given a cold clone pair and an unrelated file that survives deletion.
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	other := filepath.Join(dir, "other.go")
	writeFile(t, path, cloneRepoSource)
	writeFile(t, other, "package main\nfunc Other() {}\n")
	store := newFTSStore(t)
	_, err := newTestIndexer(store).Index(dir)
	require.NoError(t, err)
	require.Len(t, similarToEdges(store), 2)
	cold := cloneCarryCorpus(t, store)
	require.Len(t, cold, 3)
	for _, row := range cold {
		require.True(t, row.Finalized)
		require.NotEmpty(t, row.Signature)
		require.Positive(t, row.TokenCount)
	}

	// When fresh incremental indexers delete and re-add the unchanged file.
	require.NoError(t, os.Remove(path))
	result, err := newTestIndexer(store).IncrementalReindexPaths(dir, nil)
	require.NoError(t, err)
	require.Empty(t, result.FailedFiles)
	require.Equal(t, 1, result.DeletedFileCount)
	require.Nil(t, store.GetNode("main.go::processItems"))
	writeFile(t, path, cloneRepoSource)
	result, err = newTestIndexer(store).IncrementalReindexPaths(dir, nil)
	require.NoError(t, err)
	require.Empty(t, result.FailedFiles)

	// Then signatures and complete corpus rows equal a clean cold build.
	clean := newFTSStore(t)
	_, err = newTestIndexer(clean).Index(dir)
	require.NoError(t, err)
	require.Equal(t, cold, cloneCarryCorpus(t, clean))
	require.Equal(t, cold, cloneCarryCorpus(t, store))
	for _, row := range cold {
		node := store.GetNode(row.NodeID)
		require.NotNil(t, node)
		require.Equal(t, row.Signature, node.Meta[cloneSigMetaKey], row.NodeID)
	}

	// Given the restored clone corpus, when an unrelated file changes later,
	// then the next incremental batch preserves every restored row and signature.
	bumpMtime(t, other, "package main\nfunc Other() { println(1) }\n")
	result, err = newTestIndexer(store).IncrementalReindexPaths(dir, nil)
	require.NoError(t, err)
	require.Empty(t, result.FailedFiles)
	require.Equal(t, cold, cloneCarryCorpus(t, store))
	for _, row := range cold {
		node := store.GetNode(row.NodeID)
		require.NotNil(t, node)
		require.Equal(t, row.Signature, node.Meta[cloneSigMetaKey], row.NodeID)
	}
}

func TestCloneCarryUnchangedSave(t *testing.T) {
	t.Setenv("GORTEX_SHADOW_MAX_FILES", "0")
	// Given a cold clone pair.
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeFile(t, path, cloneRepoSource)
	store := newFTSStore(t)
	idx := newTestIndexer(store)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	cold := cloneCarryCorpus(t, store)
	require.Len(t, cold, 3)

	// When the ready indexer re-saves a comment without deleting the file.
	bumpMtime(t, path, cloneRepoSource+"\n// Body-unchanged save.\n")
	result, err := idx.IncrementalReindexPaths(dir, nil)
	require.NoError(t, err)
	require.Empty(t, result.FailedFiles)

	// Then existing finalized corpus rows and node signatures remain unchanged.
	require.Equal(t, cold, cloneCarryCorpus(t, store))
	for _, row := range cold {
		require.NotEmpty(t, row.Signature)
		node := store.GetNode(row.NodeID)
		require.NotNil(t, node)
		require.Equal(t, row.Signature, node.Meta[cloneSigMetaKey], row.NodeID)
	}
}

func TestCloneCarryChangedReaddRejectsOldSignature(t *testing.T) {
	t.Setenv("GORTEX_SHADOW_MAX_FILES", "0")
	// Given a cold clone pair, remember the original function's signature.
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeFile(t, path, cloneRepoSource)
	writeFile(t, filepath.Join(dir, "other.go"), "package main\nfunc Other() {}\n")
	store := newFTSStore(t)
	idx := newTestIndexer(store)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	const id = "main.go::processItems"
	oldSignature := store.GetNode(id).Meta[cloneSigMetaKey]
	require.NotEmpty(t, oldSignature)

	// When deletion is followed by a re-add with a different function body.
	require.NoError(t, os.Remove(path))
	result, err := newTestIndexer(store).IncrementalReindexPaths(dir, nil)
	require.NoError(t, err)
	require.Empty(t, result.FailedFiles)
	_, rest, found := strings.Cut(cloneRepoSource, "func processRecords")
	require.True(t, found)
	changed := "package main\nfunc processItems(xs []int) int {\n" + cloneFollowupOtherBody +
		"}\n\nfunc processRecords" + rest
	writeFile(t, path, changed)
	result, err = newTestIndexer(store).IncrementalReindexPaths(dir, nil)
	require.NoError(t, err)
	require.Empty(t, result.FailedFiles)

	// Then the re-add does not carry the old signature, even before follow-up.
	node := store.GetNode(id)
	require.NotNil(t, node)
	require.NotEqual(t, oldSignature, node.Meta[cloneSigMetaKey])

	// When the deferred clone pass runs, then it matches a clean cold build.
	detectClonesAndEmitEdgesWithBaselineCtx(context.Background(), store, "", idx.cloneThreshold())
	clean := newFTSStore(t)
	_, err = newTestIndexer(clean).Index(dir)
	require.NoError(t, err)
	require.Equal(t, cloneCarryCorpus(t, clean), cloneCarryCorpus(t, store))
	require.NotEmpty(t, store.GetNode(id).Meta[cloneSigMetaKey])
	require.Equal(t, clean.GetNode(id).Meta[cloneSigMetaKey], store.GetNode(id).Meta[cloneSigMetaKey])
	require.NotEqual(t, oldSignature, store.GetNode(id).Meta[cloneSigMetaKey])
}

func cloneCarryCorpus(t *testing.T, store graph.Store) []graph.CloneCorpusRow {
	t.Helper()
	rows, err := store.(graph.CloneCorpusPager).CloneCorpusPage("", "", 10)
	require.NoError(t, err)
	require.Less(t, len(rows), 10, "fixture must fit in one corpus page")
	return rows
}

func TestCloneCarryMemoryUnchangedSave(t *testing.T) {
	t.Setenv("GORTEX_SHADOW_MAX_FILES", "0")
	// Given finalized clone rows and node signatures from a cold in-memory build.
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeFile(t, path, cloneRepoSource)
	store := graph.New()
	_, err := newTestIndexer(store).Index(dir)
	require.NoError(t, err)
	cold := cloneCarryCorpus(t, store)
	require.Len(t, cold, 3)
	for _, row := range cold {
		require.True(t, row.Finalized)
		require.NotEmpty(t, row.Signature)
		require.Equal(t, row.Signature, store.GetNode(row.NodeID).Meta[cloneSigMetaKey])
	}

	// When a fresh indexer saves a comment without changing any function body.
	bumpMtime(t, path, cloneRepoSource+"\n// Body-unchanged save.\n")
	result, err := newTestIndexer(store).IncrementalReindexPaths(dir, nil)
	require.NoError(t, err)
	require.Empty(t, result.FailedFiles)

	// Then complete corpus rows and node signatures stay equal to the cold build.
	require.Equal(t, cold, cloneCarryCorpus(t, store))
	for _, row := range cold {
		node := store.GetNode(row.NodeID)
		require.NotNil(t, node)
		require.Equal(t, row.Signature, node.Meta[cloneSigMetaKey], row.NodeID)
	}
}

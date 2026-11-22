package indexer

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestPebbleVectorRefusalDoesNotEmbedOrPublish(t *testing.T) {
	store, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "head"), store_pebble.DefaultProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	node := &graph.Node{
		ID: "repo/a.go::Alpha", Name: "Alpha", Kind: graph.KindFunction,
		FilePath: "repo/a.go", Language: "go", RepoPrefix: "repo",
	}
	store.AddNode(node)
	require.NoError(t, store.UpsertSymbolFTS(node.ID, "alpha"))
	require.NoError(t, store.Publish(t.Context()))
	textHits, err := store.SearchSymbols("Alpha", 1)
	require.NoError(t, err)
	require.Len(t, textHits, 1)
	require.Equal(t, node.ID, textHits[0].NodeID)

	idx := vectorPlanTestIndexer(store)
	idx.SetRepoPrefix("repo")
	emb := &poolEmbedder{}
	idx.SetEmbedder(emb)
	sw := idx.swappable()
	t.Cleanup(sw.Close)
	before, release := sw.AcquireBackend()
	release()

	require.NoError(t, idx.buildSearchIndexCtx(t.Context()))
	require.ErrorIs(t, idx.LastVectorBuildError(), errors.ErrUnsupported)
	require.Zero(t, emb.calls, "unsupported storage must refuse before dispatching embedding work")
	require.NoError(t, store.ReadError(), "preparation must not attempt a graph census")

	plan := &preparedVectorPlan{
		repoPrefix: "repo", dims: 3,
		items: []graph.VectorCorpusItem{{NodeID: node.ID, Vec: []float32{1, 0, 0}}},
	}
	require.ErrorIs(t, idx.installVectorPlan(t.Context(), store, plan), errors.ErrUnsupported)
	require.ErrorIs(t, idx.LastVectorBuildError(), errors.ErrUnsupported)
	assertVectorPlanReleased(t, plan)
	after, release := sw.AcquireBackend()
	defer release()
	require.Same(t, before, after, "neither preparation nor installation may publish a vector index")

	// The existing vector-specific query boundary refuses even with searchable
	// lexical data. It must not return the text hit as a substitute.
	hits, err := graph.VectorSearcher(store).SimilarTo([]float32{1, 0, 0}, 1)
	require.ErrorIs(t, err, errors.ErrUnsupported)
	require.Nil(t, hits)
}

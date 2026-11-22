package indexer

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/search"
)

type vectorPlanTestStore struct {
	graph.Store
	mu           sync.Mutex
	replaceErr   error
	replaceStats graph.VectorCorpusStats
	replaceCalls int
	hits         []graph.VectorHit
}

func newVectorPlanTestStore() *vectorPlanTestStore {
	return &vectorPlanTestStore{Store: graph.New()}
}

func (s *vectorPlanTestStore) ReplaceVectorCorpus(
	_ context.Context,
	_ string,
	dims int,
	_ []graph.VectorCorpusItem,
) (graph.VectorCorpusStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replaceCalls++
	if s.replaceErr != nil {
		return graph.VectorCorpusStats{}, s.replaceErr
	}
	stats := s.replaceStats
	if stats.Dims <= 0 {
		stats.Dims = dims
	}
	return stats, nil
}

func (s *vectorPlanTestStore) VectorCorpusStats(context.Context, int) (graph.VectorCorpusStats, error) {
	return s.replaceStats, nil
}

func (s *vectorPlanTestStore) VectorCorpusStatsForRepo(context.Context, string, int) (graph.VectorCorpusStats, error) {
	return s.replaceStats, nil
}

func (s *vectorPlanTestStore) UpsertEmbedding(string, []float32) error { return nil }
func (s *vectorPlanTestStore) BulkUpsertEmbeddings([]graph.VectorItem) error {
	return nil
}
func (s *vectorPlanTestStore) BuildVectorIndex(int) error { return nil }
func (s *vectorPlanTestStore) SimilarTo(_ []float32, limit int) ([]graph.VectorHit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit < len(s.hits) {
		return append([]graph.VectorHit(nil), s.hits[:limit]...), nil
	}
	return append([]graph.VectorHit(nil), s.hits...), nil
}
func (s *vectorPlanTestStore) GetEmbeddings([]string) map[string][]float32 { return nil }

func vectorPlanTestIndexer(store graph.Store) *Indexer {
	idx := New(store, parser.NewRegistry(), config.Default().Index, zap.NewNop())
	idx.SetEmbedder(&poolEmbedder{})
	return idx
}

func assertVectorPlanReleased(t *testing.T, plan *preparedVectorPlan) {
	t.Helper()
	assert.Empty(t, plan.repoPrefix)
	assert.Zero(t, plan.dims)
	assert.Nil(t, plan.items)
	assert.Nil(t, plan.droppedIDs)
	assert.Zero(t, plan.dropped)
	assert.Zero(t, plan.embeddedText)
}

func TestPreparedVectorPlanReleaseClearsOwnedState(t *testing.T) {
	plan := &preparedVectorPlan{
		repoPrefix: "repo",
		dims:       3,
		items: []graph.VectorCorpusItem{
			{NodeID: "repo/a", Vec: []float32{1, 0, 0}},
		},
		dropped:      1,
		droppedIDs:   []string{"repo/b"},
		embeddedText: 2,
	}

	plan.Release()
	assertVectorPlanReleased(t, plan)
	plan.Release() // idempotent
}

func TestInstallVectorPlanPublishesDelegatedBackendAndReleasesPlan(t *testing.T) {
	store := newVectorPlanTestStore()
	store.replaceStats = graph.VectorCorpusStats{
		VectorCount: 2, ChunkCount: 1,
		RepositoryVectorCount: 2, RepositoryChunkCount: 1,
		Dims: 3,
	}
	idx := vectorPlanTestIndexer(store)
	plan := &preparedVectorPlan{
		repoPrefix: "repo",
		dims:       3,
		items: []graph.VectorCorpusItem{
			{NodeID: "repo/a", Vec: []float32{1, 0, 0}},
			{NodeID: "repo/b#chunk0", ParentID: "repo/b", Vec: []float32{0, 1, 0}},
		},
	}

	require.NoError(t, idx.installVectorPlan(context.Background(), store, plan))
	assertVectorPlanReleased(t, plan)
	require.Equal(t, 1, store.replaceCalls)

	backend, release := idx.swappable().AcquireBackend()
	defer release()
	hybrid, ok := backend.(*search.HybridBackend)
	require.True(t, ok)
	require.Equal(t, 2, hybrid.VectorIndex().Count())
	require.True(t, hybrid.VectorIndex().HasChunks())
	require.Zero(t, hybrid.VectorSizeBytes())
}

func TestInstallVectorPlanFailureKeepsPublicationAndReleasesPlan(t *testing.T) {
	cause := errors.New("native install failed")
	for _, tt := range []struct {
		name  string
		err   error
		multi bool
		pre   bool
	}{
		{name: "backend failure", err: cause},
		{name: "multi-repo backend failure", err: cause, multi: true},
		{name: "canceled by installer", err: context.Canceled},
		{name: "canceled before install", err: context.Canceled, pre: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			base := newFTSStore(t)
			base.AddBatch([]*graph.Node{
				{ID: "A/a", RepoPrefix: "A", Name: "a", Kind: graph.KindFunction},
				{ID: "A/b", RepoPrefix: "A", Name: "b", Kind: graph.KindFunction},
				{ID: "B/b", RepoPrefix: "B", Name: "b", Kind: graph.KindFunction},
			}, nil)
			store := newVectorPublicationProbeStore(base)
			idx := vectorPlanTestIndexer(store)
			t.Cleanup(idx.swappable().Close)
			initial := &preparedVectorPlan{
				repoPrefix: "A", dims: 3,
				items: []graph.VectorCorpusItem{{NodeID: "A/a", Vec: []float32{1, 0, 0}}},
			}
			require.NoError(t, idx.installVectorPlan(t.Context(), store, initial))
			before, release := idx.swappable().AcquireBackend()
			release()

			if tt.multi {
				idx.repositoryMutationOwner = &MultiIndexer{}
			}
			store.replaceErr = tt.err
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			wantCalls := int32(2)
			if tt.pre {
				cancel()
				wantCalls = 1
			}
			plan := &preparedVectorPlan{
				repoPrefix: "A", dims: 3,
				items: []graph.VectorCorpusItem{{NodeID: "A/b", Vec: []float32{0, 1, 0}}},
			}
			if tt.multi {
				plan.repoPrefix = "B"
				plan.items[0].NodeID = "B/b"
			}
			err := idx.installVectorPlan(ctx, store, plan)
			require.ErrorIs(t, err, tt.err)
			if !tt.pre {
				require.ErrorIs(t, err, ErrVectorInstall)
			}
			require.Equal(t, err, idx.LastVectorBuildError())
			require.Equal(t, wantCalls, store.replaceCalls.Load())
			assertVectorPlanReleased(t, plan)
			stats, err := base.VectorCorpusStats(t.Context(), 3)
			require.NoError(t, err)
			require.Equal(t, 1, stats.VectorCount)
			require.Equal(t, map[string][]float32{"A/a": {1, 0, 0}}, base.GetEmbeddings([]string{"A/a", "A/b", "B/b"}))

			after, release := idx.swappable().AcquireBackend()
			defer release()
			require.Same(t, before, after, "failed install must not replace the published delegate")
			published := after.(*search.HybridBackend).VectorIndex()
			require.Equal(t, 1, published.Count())
			require.Equal(t, []string{"A/a"}, published.Search([]float32{1, 0, 0}, 2))
		})
	}
}

type vectorPlanResultEmbedder struct {
	vectors [][]float32
}

func (e *vectorPlanResultEmbedder) Embed(context.Context, string) ([]float32, error) {
	if len(e.vectors) == 0 {
		return nil, nil
	}
	return e.vectors[0], nil
}
func (e *vectorPlanResultEmbedder) EmbedBatch(context.Context, []string) ([][]float32, error) {
	return e.vectors, nil
}
func (e *vectorPlanResultEmbedder) Dimensions() int { return 3 }
func (e *vectorPlanResultEmbedder) Close() error    { return nil }

func TestPrepareVectorPlanRejectsCardinalityAndNonFiniteResults(t *testing.T) {
	nodes := []*graph.Node{{
		ID: "repo/a.go::Alpha", Kind: graph.KindFunction, Name: "Alpha",
		FilePath: "repo/a.go", RepoPrefix: "repo", Language: "go",
	}}
	tests := []struct {
		name    string
		vectors [][]float32
		want    string
	}{
		{name: "missing", vectors: nil, want: "returned 0 vectors for 1 texts"},
		{name: "extra", vectors: [][]float32{{1, 0, 0}, {0, 1, 0}}, want: "returned 2 vectors for 1 texts"},
		{name: "nan", vectors: [][]float32{{float32(math.NaN()), 0, 0}}, want: "all 1 embedding vectors were invalid"},
		{name: "infinity", vectors: [][]float32{{float32(math.Inf(1)), 0, 0}}, want: "all 1 embedding vectors were invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idx := New(graph.New(), parser.NewRegistry(), config.Default().Index, zap.NewNop())
			idx.SetRepoPrefix("repo")
			idx.SetEmbedder(&vectorPlanResultEmbedder{vectors: tt.vectors})
			plan, err := idx.prepareVectorPlan(context.Background(), nodes)
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.want)
			require.Nil(t, plan)
		})
	}
}

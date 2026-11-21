package store_pebble

import (
	"context"
	"errors"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
)

var (
	_ graph.VectorSearcher              = (*BuildStore)(nil)
	_ graph.AtomicVectorCorpusInstaller = (*BuildStore)(nil)
)

func vectorUnsupported() error { return fmt.Errorf("Pebble vector family: %w", errors.ErrUnsupported) }

func (b *BuildStore) UpsertEmbedding(string, []float32) error       { return vectorUnsupported() }
func (b *BuildStore) BulkUpsertEmbeddings([]graph.VectorItem) error { return vectorUnsupported() }
func (b *BuildStore) BuildVectorIndex(int) error                    { return vectorUnsupported() }
func (b *BuildStore) SimilarTo([]float32, int) ([]graph.VectorHit, error) {
	return nil, vectorUnsupported()
}
func (b *BuildStore) GetEmbeddings([]string) map[string][]float32 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.readErr == nil {
		b.readErr = vectorUnsupported()
	}
	return nil
}
func (b *BuildStore) ReadError() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.readErr
}
func (b *BuildStore) ReplaceVectorCorpus(context.Context, string, int, []graph.VectorCorpusItem) (graph.VectorCorpusStats, error) {
	return graph.VectorCorpusStats{}, vectorUnsupported()
}
func (b *BuildStore) VectorCorpusStats(context.Context, int) (graph.VectorCorpusStats, error) {
	return graph.VectorCorpusStats{}, vectorUnsupported()
}
func (b *BuildStore) VectorCorpusStatsForRepo(context.Context, string, int) (graph.VectorCorpusStats, error) {
	return graph.VectorCorpusStats{}, vectorUnsupported()
}

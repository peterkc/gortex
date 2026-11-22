package indexer

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

// mixedEmbedder returns a 4-dim vector for every third input (starting with the
// first, so the width is detectable from index 0), a nil vector for the next,
// and a wrong-width vector for the one after — a deterministic valid/nil/short
// mix regardless of how the indexer batches the inputs.
type mixedEmbedder struct {
	mu   sync.Mutex
	seen int
	good int
}

func (m *mixedEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	v, err := m.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return v[0], nil
}

func (m *mixedEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([][]float32, len(texts))
	for i := range texts {
		switch (m.seen + i) % 3 {
		case 1:
			out[i] = nil // dropped: nil vector
		case 2:
			out[i] = []float32{1, 2} // dropped: wrong width (2 != 4)
		default:
			out[i] = []float32{float32(len(texts[i])), 0, 0, 0} // valid 4-dim
			m.good++
		}
	}
	m.seen += len(texts)
	return out, nil
}

func (m *mixedEmbedder) Dimensions() int { return 4 }
func (m *mixedEmbedder) Close() error    { return nil }

// nilEmbedder returns nil for every input — an all-invalid batch.
type nilEmbedder struct{}

func (nilEmbedder) Embed(context.Context, string) ([]float32, error) { return nil, nil }
func (nilEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	return make([][]float32, len(texts)), nil
}
func (nilEmbedder) Dimensions() int { return 4 }
func (nilEmbedder) Close() error    { return nil }

const vectorIngestFixture = `package main

func Alpha() {}
func Beta() {}
func Gamma() {}
func Delta() {}
func Epsilon() {}
func Zeta() {}
`

func indexWithEmbedder(t *testing.T, emb interface {
	Embed(context.Context, string) ([]float32, error)
	EmbedBatch(context.Context, []string) ([][]float32, error)
	Dimensions() int
	Close() error
}) *Indexer {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(vectorIngestFixture), 0o644))

	g := newFTSStore(t)
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())

	cfg := config.Default().Index
	cfg.Workers = 1
	cfg.SkipSearch = config.DefaultSkipSearch()

	idx := New(g, reg, cfg, zap.NewNop())
	idx.SetEmbedder(emb)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	return idx
}

// TestVectorPublication_DropsInvalidVectors: a mix of valid/nil/short vectors
// yields a vector index populated with only the valid ones — the bad vectors
// are dropped rather than poisoning the index or aborting a viable build.
func TestVectorPublication_DropsInvalidVectors(t *testing.T) {
	emb := &mixedEmbedder{}
	idx := indexWithEmbedder(t, emb)

	sw, ok := idx.Search().(*search.Swappable)
	require.True(t, ok)
	backend, release := sw.AcquireBackend()
	defer release()
	hybrid, ok := backend.(*search.HybridBackend)
	require.True(t, ok, "a viable subset of vectors must still produce a HybridBackend")
	require.NotNil(t, hybrid.VectorIndex())

	require.Greater(t, emb.good, 0, "test precondition: some vectors were valid")
	require.Less(t, emb.good, emb.seen, "test precondition: some vectors were dropped")
	assert.Equal(t, emb.good, hybrid.VectorIndex().Count(),
		"only the valid vectors should be in the index")
	assert.NoError(t, idx.LastVectorBuildError(),
		"a partially-valid build is a success, not a recorded failure")
}

// TestVectorPublication_AllInvalidAbortsToTextOnly: when every vector is invalid
// the build must abort to text-only search, not ship a silently empty index.
func TestVectorPublication_AllInvalidAbortsToTextOnly(t *testing.T) {
	idx := indexWithEmbedder(t, nilEmbedder{})

	sw, ok := idx.Search().(*search.Swappable)
	require.True(t, ok)
	backend, release := sw.AcquireBackend()
	defer release()
	_, isHybrid := backend.(*search.HybridBackend)
	assert.False(t, isHybrid,
		"an all-invalid embedding pass must leave a text-only backend, not an empty vector index")
	assert.Error(t, idx.LastVectorBuildError(),
		"the vector-build failure must be recorded for eval to surface")
}

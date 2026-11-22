package indexer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/embedding"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

// stubEmbedder is a deterministic minimal embedder that lets the
// indexer wire up a HybridBackend in tests without pulling in the
// static-vector asset or a real ONNX runtime. It emits a 4-dim
// vector whose first element is the text length — enough for the
// durable store to install and search it.
type stubEmbedder struct{}

func (stubEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	return []float32{float32(len(text)), 0, 0, 0}, nil
}

func (stubEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = []float32{float32(len(t)), 0, 0, 0}
	}
	return out, nil
}

func (stubEmbedder) Dimensions() int { return 4 }
func (stubEmbedder) Close() error    { return nil }

// indexedVectorBackend indexes dir with the given chunk options and returns
// the published vector backend, pinned for the remainder of the test.
func indexedVectorBackend(t *testing.T, dir string, opts embedding.ChunkOptions) (*search.VectorBackend, *store_sqlite.Store) {
	t.Helper()
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())

	cfg := config.Default().Index
	cfg.Workers = 1

	idx, store := newFTSIndexer(t, dir, reg, cfg, func(idx *Indexer) {
		idx.SetEmbedder(stubEmbedder{})
		idx.SetEmbeddingChunkOptions(opts)
	})

	sw, ok := idx.Search().(*search.Swappable)
	require.True(t, ok)
	backend, release := sw.AcquireBackend()
	t.Cleanup(release)
	hyb, ok := backend.(*search.HybridBackend)
	require.True(t, ok, "an embedder-equipped index must produce a HybridBackend")
	return hyb.VectorIndex(), store
}

// TestVectorPublication_LongSymbolYieldsMultipleChunks proves that a function
// whose source span exceeds the chunk threshold is split into several chunk
// vectors and the durable rows retain each chunk's parent.
func TestVectorPublication_LongSymbolYieldsMultipleChunks(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("package main\n\nfunc BigFunc() {\n")
	for i := 0; i < 80; i++ {
		b.WriteString("\tprintln(\"line\")\n")
	}
	b.WriteString("}\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "big.go"), []byte(b.String()), 0o644))

	// Low threshold + small window so BigFunc is forced to split.
	vec, store := indexedVectorBackend(t, dir, embedding.ChunkOptions{ThresholdLines: 20, WindowLines: 15})

	require.True(t, vec.HasChunks(),
		"a long function past the threshold must produce chunk vectors")

	hits, err := store.SimilarTo([]float32{1, 0, 0, 0}, vec.Count())
	require.NoError(t, err)
	require.Len(t, hits, vec.Count())
	var chunkCount int
	for _, hit := range hits {
		if strings.HasPrefix(hit.NodeID, "big.go::BigFunc#chunk") {
			assert.Equal(t, "big.go::BigFunc", hit.ParentID)
			chunkCount++
		}
	}
	assert.GreaterOrEqual(t, chunkCount, 2,
		"BigFunc must be split into at least two chunk vectors")
}

// TestVectorPublication_ShortSymbolStaysWhole proves the inverse: a file of
// only small functions produces no chunk vectors — every symbol is embedded
// under its own ID.
func TestVectorPublication_ShortSymbolStaysWhole(t *testing.T) {
	dir := t.TempDir()
	src := `package main

func Tiny() {
	println("a")
}

func AlsoTiny() {
	println("b")
}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "small.go"), []byte(src), 0o644))

	vec, _ := indexedVectorBackend(t, dir, embedding.ChunkOptions{ThresholdLines: 60, WindowLines: 40})
	assert.False(t, vec.HasChunks(),
		"a file of only short functions must produce no chunk vectors")
	assert.Greater(t, vec.Count(), 0, "the symbols must still be embedded")
}

// TestVectorPublication_ChunkedSymbolNotDuplicatedInSearch proves the end-to-end
// de-chunk contract through a real index: searching for a long symbol returns
// it once, and no synthetic chunk ID surfaces.
func TestVectorPublication_ChunkedSymbolNotDuplicatedInSearch(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("package main\n\nfunc ValidateRequestPayload() {\n")
	for i := 0; i < 80; i++ {
		b.WriteString("\tcheckField()\n")
	}
	b.WriteString("}\n\nfunc checkField() {}\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "v.go"), []byte(b.String()), 0o644))

	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	cfg := config.Default().Index
	cfg.Workers = 1
	idx, store := newFTSIndexer(t, dir, reg, cfg, func(idx *Indexer) {
		idx.SetEmbedder(stubEmbedder{})
		idx.SetEmbeddingChunkOptions(embedding.ChunkOptions{ThresholdLines: 20, WindowLines: 15})
	})

	const symbolID = "v.go::ValidateRequestPayload"

	// Probe the text half of the hybrid on its own first. The fused result
	// below cannot carry this claim: the stub embedder maps every text to the
	// same direction, so the vector channel returns the whole corpus for any
	// query and would answer "is the symbol retrievable" no matter what the
	// text side did. The query is multi-word on purpose — an identifier-shaped
	// query is short-circuited on an exact FindNodesByName hit (store_fts.go
	// tier 0) and never reaches the ranked FTS tier.
	ftsHits, err := store.SearchSymbols("validate request payload", 20)
	require.NoError(t, err)
	ftsIDs := make([]string, 0, len(ftsHits))
	for _, h := range ftsHits {
		ftsIDs = append(ftsIDs, h.NodeID)
	}
	require.Contains(t, ftsIDs, symbolID, "the chunked symbol must be in the symbol FTS corpus")

	results := idx.Search().Search("ValidateRequestPayload", 20)
	require.NotEmpty(t, results)

	seen := map[string]int{}
	for _, r := range results {
		assert.NotContains(t, r.ID, "#chunk",
			"a chunk ID must never appear in search output")
		seen[r.ID]++
	}
	assert.Equal(t, 1, seen[symbolID],
		"the chunked symbol must be returned exactly once")
}

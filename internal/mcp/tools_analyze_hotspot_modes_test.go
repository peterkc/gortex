package mcp

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

// buildHotspotRerankFixture seeds three function nodes with deterministic
// complexity scores AND varying blame / releases metadata so the
// novelty / directional modes can reorder them in predictable ways.
func buildHotspotRerankFixture(t *testing.T, now time.Time) (graph.Store, []analysis.HotspotEntry) {
	t.Helper()
	g := graph.New()

	g.AddNode(&graph.Node{
		ID: "fresh", Name: "Fresh", Kind: graph.KindFunction, FilePath: "fresh.go",
		Meta: map[string]any{
			"last_authored": map[string]any{"timestamp": now.Add(-2 * 24 * time.Hour).Unix()},
			"added_in":      map[string]any{"timestamp": now.Add(-2 * 24 * time.Hour).Unix()},
		},
	})
	g.AddNode(&graph.Node{
		ID: "midway", Name: "Midway", Kind: graph.KindFunction, FilePath: "midway.go",
		Meta: map[string]any{
			"last_authored": map[string]any{"timestamp": now.Add(-15 * 24 * time.Hour).Unix()},
			"added_in":      map[string]any{"timestamp": now.Add(-15 * 24 * time.Hour).Unix()},
		},
	})
	g.AddNode(&graph.Node{
		ID: "stale", Name: "Stale", Kind: graph.KindFunction, FilePath: "stale.go",
		Meta: map[string]any{
			"last_authored": map[string]any{"timestamp": now.Add(-90 * 24 * time.Hour).Unix()},
			"added_in":      map[string]any{"timestamp": now.Add(-90 * 24 * time.Hour).Unix()},
		},
	})

	// Identical complexity scores so the rerank is decisive.
	entries := []analysis.HotspotEntry{
		{ID: "fresh", Name: "Fresh", ComplexityScore: 10.0},
		{ID: "midway", Name: "Midway", ComplexityScore: 10.0},
		{ID: "stale", Name: "Stale", ComplexityScore: 10.0},
	}
	return g, entries
}

func TestRerankHotspots_ComplexityIsPassThrough(t *testing.T) {
	g, entries := buildHotspotRerankFixture(t, time.Now())
	out, available := rerankHotspots(entries, g, "complexity", "", 30)
	assert.True(t, available)
	// Same order, same scores.
	assert.Len(t, out, 3)
	for i, e := range out {
		assert.Equal(t, entries[i].ComplexityScore, e.ComplexityScore)
	}
}

func TestRerankHotspots_NoveltyPromotesRecent(t *testing.T) {
	now := time.Now()
	g, entries := buildHotspotRerankFixture(t, now)
	out, available := rerankHotspots(entries, g, "novelty", "", 30)
	assert.True(t, available)
	// 'fresh' (2 days old) ranks first; 'stale' (90 days) drops to 0.
	assert.Equal(t, "fresh", out[0].ID)
	var staleScore float64
	for _, e := range out {
		if e.ID == "stale" {
			staleScore = e.ComplexityScore
		}
	}
	assert.InDelta(t, 0.0, staleScore, 1e-6, "stale outside window gets zero novelty weight")
}

func TestRerankHotspots_DirectionalAddsRewardsRecent(t *testing.T) {
	g, entries := buildHotspotRerankFixture(t, time.Now())
	out, available := rerankHotspots(entries, g, "directional", "adds", 30)
	assert.True(t, available)
	assert.Equal(t, "fresh", out[0].ID, "adds direction promotes the newest addition")
}

func TestRerankHotspots_DirectionalStableRewardsOld(t *testing.T) {
	g, entries := buildHotspotRerankFixture(t, time.Now())
	out, available := rerankHotspots(entries, g, "directional", "stable", 30)
	assert.True(t, available)
	// stale (90 days) saturates frac=1.0 (clamped), midway and fresh
	// are < 1.0 — stale wins.
	assert.Equal(t, "stale", out[0].ID, "stable direction promotes the oldest addition")
}

func TestRerankHotspots_MissingMetaScoresZero(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "nometa", Name: "NoMeta", Kind: graph.KindFunction, FilePath: "x.go"})
	entries := []analysis.HotspotEntry{{ID: "nometa", Name: "NoMeta", ComplexityScore: 10.0}}

	out, available := rerankHotspots(entries, g, "novelty", "", 30)

	assert.True(t, available)
	for _, e := range out {
		if e.ID == "nometa" {
			assert.InDelta(t, 0.0, e.ComplexityScore, 1e-6, "missing meta -> zero weight under novelty")
		}
	}
}

func TestRerankHotspots_MissingNodeDropped(t *testing.T) {
	g := graph.New()
	entries := []analysis.HotspotEntry{{ID: "doesnotexist", Name: "X", ComplexityScore: 5.0}}
	out, available := rerankHotspots(entries, g, "novelty", "", 30)
	assert.True(t, available)
	assert.Empty(t, out, "entries whose node is missing from the graph are dropped")
}

func TestDecodeMetaTimestamp_Forms(t *testing.T) {
	now := time.Now().Truncate(time.Second).UTC()
	cases := []any{
		now.Unix(),
		int(now.Unix()),
		float64(now.Unix()),
		now.Format(time.RFC3339),
	}
	for _, c := range cases {
		got := decodeMetaTimestamp(c)
		assert.Equal(t, now.Unix(), got.Unix(), "decode %T(%v) → %v", c, c, got)
	}
	assert.True(t, decodeMetaTimestamp(nil).IsZero())
	assert.True(t, decodeMetaTimestamp("garbage").IsZero())
}

func TestNoveltyWeight_LinearDecay(t *testing.T) {
	now := time.Now()
	window := 30 * 24 * time.Hour
	// Day 0 → weight 1.0
	n := &graph.Node{Meta: map[string]any{"last_authored": map[string]any{"timestamp": now.Unix()}}}
	assert.InDelta(t, 1.0, noveltyWeight(nil, n, now, window), 1e-6)
	// Day 15 → weight 0.5
	n.Meta["last_authored"] = map[string]any{"timestamp": now.Add(-15 * 24 * time.Hour).Unix()}
	assert.InDelta(t, 0.5, noveltyWeight(nil, n, now, window), 1e-2)
	// Day 30+ → weight 0
	n.Meta["last_authored"] = map[string]any{"timestamp": now.Add(-31 * 24 * time.Hour).Unix()}
	assert.InDelta(t, 0.0, noveltyWeight(nil, n, now, window), 1e-6)
}

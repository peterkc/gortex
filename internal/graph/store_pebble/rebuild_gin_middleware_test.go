package store_pebble

import (
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildGinMiddlewareScratchAndBounds(t *testing.T) {
	const file = "gin.go"
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	d := &graph.Node{ID: "dispatcher", Kind: graph.KindMethod, Language: "go", FilePath: file,
		Meta: map[string]any{"gin_dispatcher": true, "gin_handlers": []string{"Handle"}}}
	h := &graph.Node{ID: "handler", Name: "Handle", Kind: graph.KindFunction, FilePath: file}
	b.AddBatch([]*graph.Node{d, h}, nil)
	facts := ParseFacts{EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: file}, Nodes: []*graph.Node{d, h}}
	receipts := func(yield func(ParseFacts, error) bool) { yield(facts, nil) }
	rules := RebuildGinRules{
		Dispatcher: func(n *graph.Node) bool {
			flag, _ := n.Meta["gin_dispatcher"].(bool)
			return flag
		},
		Names: func(n *graph.Node) []string {
			names, _ := n.Meta["gin_handlers"].([]string)
			return names
		},
		Target: func(d, h *graph.Node) bool { return d.ID != h.ID && d.WorkspaceID == h.WorkspaceID },
		Edge: func(d, h *graph.Node) *graph.Edge {
			return &graph.Edge{From: d.ID, To: h.ID, Kind: graph.EdgeCalls, FilePath: d.FilePath, Line: d.StartLine}
		},
		SeenKey:    func(d, h string) string { return d + "\x00" + h },
		GoLanguage: func(language string) bool { return language == "go" },
		Threshold:  1,
	}
	before := rebuildFrameworkCustodyRows(t, b)
	zero := rules
	zero.Threshold = 0
	missing := rules
	missing.Target = nil
	for _, unusable := range []RebuildGinRules{zero, missing} {
		count, err := b.RebuildGinMiddleware(ctx, receipts, unusable)
		require.Zero(t, count)
		require.ErrorIs(t, err, ErrRebuildGinMiddleware)
		require.ErrorContains(t, err, "gin middleware inputs")
		require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
	}

	stranger := []byte{'g', 'q', 'G', 'x'}
	require.NoError(t, b.pending.Set(stranger, []byte("owned"), pebble.NoSync))
	count, err := b.RebuildGinMiddleware(ctx, receipts, rules)
	require.Zero(t, count)
	require.ErrorContains(t, err, "scratch range occupied")
	before[string(stranger)] = []byte("owned")
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
	require.NoError(t, b.pending.Delete(stranger, pebble.NoSync))

	big := strings.Repeat("x", 16<<20)
	other := &graph.Node{ID: "irrelevant", Kind: graph.KindVariable, FilePath: file, Meta: map[string]any{"large": big}}
	b.AddNode(other)
	tooBig := *h
	tooBig.Meta = other.Meta
	b.AddNode(&tooBig)
	count, err = b.RebuildGinMiddleware(ctx, receipts, rules)
	require.Zero(t, count)
	require.ErrorIs(t, err, ErrRebuildLinkInputSize)
	busy, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildGinPrefix)
	require.NoError(t, err)
	require.False(t, busy, "the pass must drop its scratch on refusal")

	b.AddNode(h)
	count, err = b.RebuildGinMiddleware(ctx, receipts, rules)
	require.NoError(t, err, "the oversized non-callable row is unrelated")
	require.EqualValues(t, 1, count)
	busy, err = rebuildGuardHasPrefix(ctx, b.pending, rebuildGinPrefix)
	require.NoError(t, err)
	require.False(t, busy, "the pass must drop its scratch after applying")
	key := pendingEdgeKey(rules.Edge(d, h))
	custody, err := readRebuildEdgeCustody(b.pending, key)
	require.NoError(t, err)
	require.Equal(t, rebuildEdgeCustody{proof: rebuildProofUntrackedMove}, custody)
}

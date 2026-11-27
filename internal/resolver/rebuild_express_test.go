package resolver

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildExpressSQLiteOracle(t *testing.T) {
	const (
		routes     = "src/routes/users.js"
		middleware = "src/middleware/auth.js"
		controller = "src/controllers/users.js"
	)
	extractor := languages.NewJavaScriptExtractor()
	var receipts []store_pebble.ParseFacts
	for _, input := range []struct{ file, source string }{
		{routes, `app.get('/users', authMiddleware, UserController.list);
class UserController { static list() {} }
function list() {}
function authMiddleware() {}
`},
		{middleware, `function auth() {}`},
		{controller, `class UserController { static list() {} }`},
	} {
		parsed, err := extractor.Extract(input.file, []byte(input.source))
		require.NoError(t, err)
		t.Cleanup(parsed.ReleaseTree)
		receipts = append(receipts, roundTripRebuildRNBridgeFacts(t,
			rebuildEventFacts(input.file, extractor.Language(), parsed.Nodes, parsed.Edges...)))
	}

	anchor := languages.ExpressInlineHandlerNodeID(routes, 1)
	incumbent := &graph.Edge{
		From: anchor, To: middleware + "::auth", Kind: graph.EdgeCalls, FilePath: routes, Line: 1,
		Origin: graph.OriginASTResolved, Meta: map[string]any{"incumbent": "keep this payload"},
	}
	settled := &graph.Edge{
		From: anchor, To: routes + "::list", Kind: graph.EdgeCalls, FilePath: routes, Line: 1,
		Origin: graph.OriginASTResolved,
		Meta:   map[string]any{"express_handler_ref": true, "express_ref_class": "UserController", "express_ref_method": "list"},
	}
	// Model live rows from before this turn, not authored parse receipts.
	facts := slices.Clone(receipts)
	facts[0].Edges = append(slices.Clone(receipts[0].Edges), incumbent, settled)
	selected, err := NewFrameworkSynthesizerSelection([]string{SynthExpressResolve})
	require.NoError(t, err)

	b, native := openRebuildEventStores(t, facts...)
	before := rebuildEventSavedRows(t, b)
	require.Equal(t, rebuildEventSQLiteRows(native), before)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selected)
	require.NoError(t, err)
	require.True(t, frozen.allows(SynthExpressResolve))

	// The preferred controller beats both the route-local class and the free
	// function. Suffix-stripped auth collides at the full incumbent key.
	require.Equal(t, 2, ResolveExpressHandlers(native))
	want := rebuildEventSQLiteRows(native)
	var calls []*graph.Edge
	for _, e := range want.edges {
		if e.From == anchor && e.Kind == graph.EdgeCalls {
			calls = append(calls, e)
		}
	}
	moved := &graph.Edge{
		From:            anchor,
		To:              controller + "::UserController.list",
		Kind:            graph.EdgeCalls,
		FilePath:        routes,
		Line:            1,
		Origin:          graph.OriginASTInferred,
		Confidence:      0.85,
		ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeCalls, 0.85),
		Meta: map[string]any{
			"express_handler_ref": true,
			"express_ref_class":   "UserController",
			"express_ref_method":  "list",
			MetaSynthesizedBy:     SynthExpressResolve,
			MetaProvenance:        ProvenanceHeuristic,
		},
	}
	require.ElementsMatch(t, []*graph.Edge{incumbent, settled, moved}, calls)
	require.Equal(t, before.nodes, want.nodes)
	require.Len(t, want.edges, len(before.edges)-1)
	_, err = RunRebuildExpress(t.Context(), b, rebuildGRPCReceipts(receipts...), frozen)
	require.NoError(t, err)
	require.Equal(t, want, rebuildEventSavedRows(t, b))

	t.Run("missing controller proof", func(t *testing.T) {
		b := openRebuildGRPCBuild(t, facts...)
		before := rebuildEventSavedRows(t, b)
		frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selected)
		require.NoError(t, err)
		require.True(t, frozen.allows(SynthExpressResolve))
		stats, err := RunRebuildExpress(t.Context(), b, rebuildGRPCReceipts(receipts[0], receipts[1]), frozen)
		require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
		require.Zero(t, stats)
		require.Equal(t, before, rebuildEventSavedRows(t, b))
	})
}

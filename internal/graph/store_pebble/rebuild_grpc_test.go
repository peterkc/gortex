package store_pebble

import (
	"path/filepath"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildGRPCRejectsInvalidScratch(t *testing.T) {
	b, err := OpenBuild(filepath.Join(t.TempDir(), "working"), FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, b.Close())
	})
	ctx := t.Context()
	var ran bool
	receipts := func(func(ParseFacts, error) bool) {
		ran = true
	}
	rebind := func(*graph.Edge, string, string, string, string) bool {
		ran = true
		return false
	}
	key := rebuildGRPCKey('s', "occupied")
	require.NoError(t, b.pending.Set(key, []byte("owner"), pebble.NoSync))
	stats, err := b.RebuildGRPC(ctx, receipts, rebind)
	require.ErrorIs(t, err, ErrRebuildGRPC)
	require.ErrorContains(t, err, "gRPC scratch range occupied")
	require.Zero(t, stats)
	owner, err := pendingGet(b.pending, key, func(raw []byte) (string, error) {
		return string(raw), nil
	})
	require.NoError(t, err)
	require.Equal(t, "owner", owner)
	require.False(t, ran)
	require.NoError(t, b.RebuildView().VisitEdges(ctx, func(*graph.Edge) error {
		t.Fatal("a refusal must not add edges")
		return nil
	}))
}

func TestRebuildGRPCWinnerCustody(t *testing.T) {
	const file, caller, owner, handler = "rpc.go", "caller", "server", "server.Hello"
	b, err := OpenBuild(filepath.Join(t.TempDir(), "working"), FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	ctx := t.Context()
	stub := func(to string, line int) *graph.Edge {
		return &graph.Edge{From: caller, To: to, Kind: graph.EdgeCalls, FilePath: file, Line: line,
			Meta: map[string]any{"via": "grpc.stub", "grpc_service": "Greeter", "grpc_method": "Hello"}}
	}
	raw := stub("unresolved::original", 1)
	live := stub("unresolved::moved", 1)
	loser := stub("unresolved::loser", 1)
	direct := stub("unresolved::direct", 2)
	blocked := stub("unresolved::blocked", 3)
	keeper := &graph.Edge{From: caller, To: handler, Kind: graph.EdgeCalls, FilePath: file, Line: 3,
		Meta: map[string]any{"keeper": true}}
	facts := ParseFacts{
		EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: file, NodeCount: 3},
		Nodes: []*graph.Node{
			{ID: caller, Name: "call", Kind: graph.KindFunction, Language: "go", FilePath: file},
			{ID: owner, Name: "server", Kind: graph.KindType, Language: "go", FilePath: file},
			{ID: handler, Name: "Hello", Kind: graph.KindMethod, Language: "go", FilePath: file},
		},
		Edges: []*graph.Edge{raw, loser, direct, keeper, blocked,
			{From: caller, To: "unresolved::RegisterGreeterServer", Kind: graph.EdgeCalls, FilePath: file, Line: 4,
				Meta: map[string]any{"grpc_register_service": "Greeter", "grpc_register_impl": "server"}},
			{From: handler, To: owner, Kind: graph.EdgeMemberOf, FilePath: file, Line: 5},
		},
	}
	require.NoError(t, b.AddParsedFile("", facts))
	require.NoError(t, b.ApplyLinkReindexes(ctx, []graph.EdgeReindex{{Edge: live, OldTo: raw.To}}))
	flushed := b.pending.Metrics().Levels[0].TableBytesFlushed
	stats, err := b.RebuildGRPC(ctx, func(yield func(ParseFacts, error) bool) { yield(facts, nil) },
		func(e *graph.Edge, _, _, to, _ string) bool {
			if to == "" || e.To == to {
				return false
			}
			e.To = to
			return true
		})
	require.NoError(t, err)
	require.Equal(t, uint64(4), stats.Moves)
	require.Equal(t, flushed, b.pending.Metrics().Levels[0].TableBytesFlushed)
	for _, tc := range []struct {
		line int
		raw  *graph.Edge
	}{{1, raw}, {2, direct}, {3, nil}} {
		key := pendingEdgeKey(stub(handler, tc.line))
		row, err := b.pendingEdge(key)
		require.NoError(t, err)
		require.NotNil(t, row)
		got, err := readRebuildEdgeCustody(b.pending, key)
		require.NoError(t, err)
		require.Zero(t, got.proof)
		if tc.raw == nil {
			require.Empty(t, got.raw)
			require.Equal(t, true, row.Meta["keeper"])
		} else {
			require.Equal(t, pendingEdgeKey(tc.raw), got.raw)
		}
	}
	requireRebuildCustodyCount(t, b, 2)
}

func requireRebuildCustodyCount(t *testing.T, b *BuildStore, want int) {
	t.Helper()
	var rows int
	require.NoError(t, pendingVisit(t.Context(), b.pending, rebuildLinkRecorderPrefix, func(row) error {
		rows++
		return nil
	}))
	require.Equal(t, want, rows)
}

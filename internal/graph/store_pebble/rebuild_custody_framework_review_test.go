package store_pebble

import (
	"encoding/binary"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildGRPCOrdersAndProvesRawCustody(t *testing.T) {
	const file, caller, owner, handler = "rpc.go", "caller", "server", "server.Hello"
	stub := func(to, payload string) *graph.Edge {
		return &graph.Edge{
			From: caller, To: to, Kind: graph.EdgeCalls, FilePath: file, Line: 1,
			Meta: map[string]any{
				"via":          "grpc.stub",
				"grpc_service": "Greeter",
				"grpc_method":  "Hello",
				"payload":      payload,
			},
		}
	}
	for _, name := range []string{
		"raw receipt before competitor", "raw receipt after competitor", "missing raw receipt",
		"synthetic", "untracked", "stale", "unproved stationary",
	} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			b := newBuild(t, FullProfile)
			raw := stub("unresolved::raw", "tracked participant")
			live := stub("unresolved::live", "tracked participant")
			rival := stub("unresolved::rival", "receipt winner")
			register := &graph.Edge{
				From: caller, To: "unresolved::RegisterGreeterServer",
				Kind: graph.EdgeCalls, FilePath: file, Line: 2,
				Meta: map[string]any{
					"grpc_register_service": "Greeter", "grpc_register_impl": "server",
				},
			}
			member := &graph.Edge{
				From: handler, To: owner, Kind: graph.EdgeMemberOf, FilePath: file, Line: 3,
			}
			facts := ParseFacts{
				EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: file, NodeCount: 3},
				Nodes: []*graph.Node{
					{ID: caller, Name: "call", Kind: graph.KindFunction,
						Language: "go", FilePath: file},
					{ID: owner, Name: "server", Kind: graph.KindType,
						Language: "go", FilePath: file},
					{ID: handler, Name: "Hello", Kind: graph.KindMethod,
						Language: "go", FilePath: file},
				},
				Edges: []*graph.Edge{raw, rival, register, member},
			}
			if name == "unproved stationary" {
				live.To = handler
				facts.Edges = []*graph.Edge{live, register, member}
			}
			require.NoError(t, b.AddParsedFile("", facts))
			move := []graph.EdgeReindex{{Edge: live, OldTo: raw.To}}
			switch name {
			case "raw receipt before competitor", "raw receipt after competitor",
				"missing raw receipt", "synthetic", "stale":
				require.NoError(t, b.ApplyLinkReindexes(ctx, move))
			case "untracked":
				b.ReindexEdges(move)
				custody, err := readRebuildEdgeCustody(b.pending, pendingEdgeKey(live))
				require.NoError(t, err)
				require.Equal(t, rebuildEdgeCustody{proof: rebuildProofUntrackedMove}, custody)
			}
			if name == "synthetic" || name == "stale" {
				custody := rebuildEdgeCustody{proof: rebuildProofReceiver}
				require.NoError(t, b.withWrites(func() error {
					return b.putEdgeWithCustody(live, custody)
				}))
				if name == "stale" {
					// Change the bytes underneath the proof, outside the sanctioned writer.
					tampered := stub(live.To, "does not match saved digest")
					value, err := encodeEdge(tampered)
					require.NoError(t, err)
					require.NoError(t, b.pending.Set(pendingEdgeKey(live), value, pebble.NoSync))
				}
			}

			// Both orders start with the decoy, which repeats the live key and recorder
			// fields. Only the original raw receipt can set the move's place.
			decoy := stub(live.To, "receipt-only decoy")
			facts.Edges = []*graph.Edge{decoy, rival, register, member}
			winnerInput, winnerRaw := rival, rival
			switch name {
			case "raw receipt before competitor":
				facts.Edges = []*graph.Edge{decoy, raw, rival, register, member}
				winnerInput, winnerRaw = live, raw
			case "raw receipt after competitor":
				facts.Edges = []*graph.Edge{decoy, rival, raw, register, member}
			case "stale":
				// A valid move before stale custody exposes a partial delete in apply.
				facts.Edges = []*graph.Edge{rival, decoy, register, member}
			case "unproved stationary":
				facts.Edges = []*graph.Edge{register, member}
			}
			before := rebuildFrameworkCustodyRows(t, b)
			receipts := func(yield func(ParseFacts, error) bool) { yield(facts, nil) }
			stats, err := b.RebuildGRPC(ctx, receipts, 1,
				func(e *graph.Edge, _, _, to, _ string) bool {
					if to == "" || e.To == to {
						return false
					}
					e.To = to
					return true
				})
			winner := *winnerInput
			winner.To = handler
			switch name {
			case "raw receipt before competitor", "raw receipt after competitor":
				require.NoError(t, err)
				require.Positive(t, stats.SpillBytes)
				stats.SpillBytes = 0
				wantStats := RebuildGRPCStats{Stubs: 2, Registrations: 1, Resolved: 2, Moves: 2}
				require.Equal(t, wantStats, stats)
				var edges []*graph.Edge
				require.NoError(t, b.RebuildView().VisitEdges(ctx, func(e *graph.Edge) error {
					edges = append(edges, e)
					return nil
				}))
				require.ElementsMatch(t, []*graph.Edge{&winner, register, member}, edges)
				custody, err := readRebuildEdgeCustody(b.pending, pendingEdgeKey(&winner))
				require.NoError(t, err)
				require.Equal(t, rebuildEdgeCustody{raw: pendingEdgeKey(winnerRaw)}, custody)
			case "unproved stationary":
				require.NoError(t, err)
				require.Positive(t, stats.SpillBytes)
				stats.SpillBytes = 0
				require.Equal(t, RebuildGRPCStats{Stubs: 1, Registrations: 1, Resolved: 1}, stats)
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
			default:
				require.ErrorIs(t, err, ErrRebuildGRPC)
				var refused *RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refused)
				first := graph.EdgeIdentityFor(live)
				if name == "missing raw receipt" {
					first = graph.EdgeIdentityFor(&winner)
				}
				require.Equal(t, &RebuildUnknownProvenanceError{Count: 1, First: first}, refused)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
			}
		})
	}
}

func rebuildFrameworkCustodyRows(t *testing.T, b *BuildStore) map[string][]byte {
	t.Helper()
	rows := make(map[string][]byte)
	require.NoError(t, pendingVisit(t.Context(), b.pending, nil, func(r row) error {
		rows[string(r.key)] = append([]byte(nil), r.value...)
		return nil
	}))
	return rows
}

func TestRebuildGRPCWindowSpillsInheritedProof(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	old := graph.Edge{
		From: "caller", To: "unresolved::stub", Kind: graph.EdgeCalls, FilePath: "rpc.go", Line: 1,
	}
	next := old
	next.To = "handler"
	custody := rebuildEdgeCustody{proof: rebuildProofReceiver}
	require.NoError(t, b.withWrites(func() error { return b.putEdgeWithCustody(&old, custody) }))
	move := binary.BigEndian.AppendUint64(rebuildGRPCMoveKey(0), 0)
	value, err := encodeEdge(&next)
	require.NoError(t, err)
	value = append(keyStrings(nil, string(pendingEdgeKey(&old)), next.To), value...)
	require.NoError(t, b.pending.Set(move, value, pebble.NoSync))

	// Test the window alone: this proof must not pass public gRPC recorder election.
	pass := rebuildGRPC{b: b, threshold: 1, stats: RebuildGRPCStats{Moves: 1}}
	require.NoError(t, b.withWrites(func() error { return pass.apply(ctx) }))
	// The inherited custody copy is the only window scratch write.
	require.Positive(t, pass.stats.SpillBytes)
	stored, err := b.pendingEdge(pendingEdgeKey(&old))
	require.NoError(t, err)
	require.Nil(t, stored)
	stored, err = b.pendingEdge(pendingEdgeKey(&next))
	require.NoError(t, err)
	require.Equal(t, &next, stored)
	got, err := readRebuildEdgeCustody(b.pending, pendingEdgeKey(&next))
	require.NoError(t, err)
	require.Equal(t, custody, got)
}

func TestRebuildTemporalWindowPreservesStagedWrapperProof(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	resolved := graph.Edge{
		From: "caller", To: "activity", Kind: graph.EdgeCalls, FilePath: "wf.go", Line: 2,
	}
	pass := RebuildTemporalPass{build: b, threshold: 1}
	// A wrapper slot can exist only in the stage. Its move carries a stage proof,
	// not the absent slot's raw key. This covers the window, not the reducer.
	require.NoError(t, pass.QueueMove(ctx, "unresolved::wrapper-activity", &resolved, true))
	require.NoError(t, b.withWrites(func() error { return pass.apply(ctx) }))
	stored, err := b.pendingEdge(pendingEdgeKey(&resolved))
	require.NoError(t, err)
	require.Equal(t, &resolved, stored)
	got, err := readRebuildEdgeCustody(b.pending, pendingEdgeKey(&resolved))
	require.NoError(t, err)
	require.Equal(t, rebuildEdgeCustody{proof: rebuildProofTemporalSynthetic}, got)
}

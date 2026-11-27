package store_pebble

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildGoFrameStoreGuards(t *testing.T) {
	const file = "routes.go"
	rules := RebuildGoFrameRules{
		Route: func(e *graph.Edge) (string, string, string) {
			request, _ := e.Meta["request"].(string)
			return request, "", e.From
		},
		Handler: func(n *graph.Node) (string, string, bool) { return n.Name, "", false },
		Dir:     func(string) string { return "" },
		Rank:    func(*graph.Node, string, string, string, string, string, bool) int { return 0 },
		Rebind: func(e *graph.Edge, _ string, target *graph.Node) (graph.EdgeReindex, *graph.Edge) {
			if target == nil || e.To == target.ID {
				return graph.EdgeReindex{}, nil
			}
			next := *e
			next.To = target.ID
			return graph.EdgeReindex{Edge: &next, OldTo: e.To}, nil
		},
	}
	for _, mode := range []string{"nil receipts", "nil rank", "occupied scratch", "tagged Call cap", "deleted raw", "stale stamp"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			rules := rules
			b := newBuild(t, FullProfile)
			call := &graph.Edge{From: "route", To: "unresolved::*.Req", Kind: graph.EdgeCalls, FilePath: file,
				Meta: map[string]any{"request": "Req"}}
			handler := &graph.Node{ID: "handler", Name: "Req", Kind: graph.KindMethod, FilePath: file, Language: "go"}
			facts := ParseFacts{
				EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: file, NodeCount: 1},
				Nodes: []*graph.Node{handler}, Edges: []*graph.Edge{call},
			}
			receipts := unreadStoreFactoryReceipts(t)
			switch mode {
			case "nil receipts":
				receipts = nil
			case "nil rank":
				rules.Rank = nil
			case "occupied scratch":
				require.NoError(t, b.pending.Set([]byte("gqjz"), []byte("sentinel"), pebble.NoSync))
			case "tagged Call cap":
				wide := *call
				wide.Context = strings.Repeat("x", 16<<20)
				b.AddEdge(&wide)
				receipts = func(yield func(ParseFacts, error) bool) { yield(facts, nil) }
			case "deleted raw", "stale stamp":
				b.AddNode(handler)
				receipts = func(yield func(ParseFacts, error) bool) { yield(facts, nil) }
				if mode == "stale stamp" {
					plain := *call
					plain.Meta = nil
					require.NoError(t, b.withWrites(func() error {
						return b.putEdgeWithCustody(&plain, rebuildEdgeCustody{proof: rebuildProofReceiver})
					}))
					plain.Context = "changed under the recorder"
					data, err := encodeEdge(&plain)
					require.NoError(t, err)
					require.NoError(t, b.pending.Set(pendingEdgeKey(&plain), data, pebble.NoSync))
					_, err = readRebuildEdgeCustody(b.pending, pendingEdgeKey(&plain))
					require.ErrorIs(t, err, ErrRebuildUnknownProvenance)
				}
			}
			before := rebuildFrameworkCustodyRows(t, b)
			moves, err := b.RebuildGoFrame(ctx, receipts, rules)
			switch mode {
			case "deleted raw", "stale stamp":
				require.NoError(t, err)
			case "tagged Call cap":
				require.ErrorIs(t, err, ErrRebuildGoFrame)
				require.ErrorIs(t, err, ErrRebuildLinkInputSize)
				require.ErrorContains(t, err, "GoFrame input row")
			default:
				require.ErrorIs(t, err, ErrRebuildGoFrame)
				require.ErrorIs(t, err, errInvalidRecord)
			}
			require.Zero(t, moves)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

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
		"synthetic mover", "untracked mover", "stale", "unproved stationary",
		"synthetic stationary", "untracked stationary",
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
			stationary := name == "unproved stationary" || name == "synthetic stationary" || name == "untracked stationary"
			if stationary {
				live.To = handler
				if name == "untracked stationary" {
					facts.Edges = []*graph.Edge{raw, register, member}
				} else {
					facts.Edges = []*graph.Edge{live, register, member}
				}
			}
			require.NoError(t, b.AddParsedFile("", facts))
			move := []graph.EdgeReindex{{Edge: live, OldTo: raw.To}}
			switch name {
			case "raw receipt before competitor", "raw receipt after competitor",
				"missing raw receipt", "synthetic mover", "stale":
				require.NoError(t, b.ApplyLinkReindexes(ctx, move))
			case "untracked mover", "untracked stationary":
				b.ReindexEdges(move)
				custody, err := readRebuildEdgeCustody(b.pending, pendingEdgeKey(live))
				require.NoError(t, err)
				require.Equal(t, rebuildEdgeCustody{proof: rebuildProofUntrackedMove}, custody)
			}
			if name == "synthetic mover" || name == "synthetic stationary" || name == "stale" {
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
				// order skips the stale digest. wantProvenance refuses the move during reduce.
				facts.Edges = []*graph.Edge{rival, decoy, register, member}
			case "unproved stationary", "synthetic stationary", "untracked stationary":
				facts.Edges = []*graph.Edge{register, member}
			}
			before := rebuildFrameworkCustodyRows(t, b)
			receipts := func(yield func(ParseFacts, error) bool) { yield(facts, nil) }
			stats, err := b.RebuildGRPC(ctx, nil, receipts,
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
			case "unproved stationary", "synthetic stationary", "untracked stationary":
				require.NoError(t, err)
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

func TestRebuildNgRxEffectStoreInputs(t *testing.T) {
	const file, name = "effects.ts", "load"
	rules := RebuildNgRxEffectRules{
		Stub: func(e *graph.Edge) string {
			name, _ := e.Meta["ngrx_action"].(string)
			return name
		},
		Getter: storeFactoryTestRules().Getter,
		Rebind: storeFactoryTestRules().Rebind,
	}
	for _, mode := range []string{"non-Call marker", "stationary raw claim", "action cap", "occupied scratch"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			b := newBuild(t, FullProfile)
			action := &graph.Node{ID: file + "::" + name, Kind: graph.KindType, Name: name, FilePath: file, Language: "typescript"}
			raw := &graph.Edge{From: "effect", To: "unresolved::*.load", Kind: graph.EdgeCalls, FilePath: file, Line: 1,
				Meta: map[string]any{"ngrx_action": name}}
			switch mode {
			case "non-Call marker":
				raw.Kind = graph.EdgeMemberOf
			case "stationary raw claim":
				raw.To = action.ID
			case "action cap":
				action.Meta = map[string]any{"payload": strings.Repeat("x", rebuildNgRxEffectRowBytes)}
			}
			facts := ParseFacts{
				EffectiveLanguage: "typescript", File: graph.FileMetaRow{FilePath: file, NodeCount: 1},
				Nodes: []*graph.Node{action}, Edges: []*graph.Edge{raw},
			}
			require.NoError(t, b.AddParsedFile("", facts))
			receipts := unreadStoreFactoryReceipts(t)
			if mode == "stationary raw claim" {
				live := *raw
				live.To = "unresolved::*.load"
				require.NoError(t, b.ApplyLinkReindexes(ctx, []graph.EdgeReindex{{Edge: &live, OldTo: raw.To}}))
				b.AddEdge(raw)
				rawKey := pendingEdgeKey(raw)
				stationary, err := readRebuildEdgeCustody(b.pending, rawKey)
				require.NoError(t, err)
				require.Equal(t, rebuildEdgeCustody{}, stationary, "stationary custody is implicit")
				moved, err := readRebuildEdgeCustody(b.pending, pendingEdgeKey(&live))
				require.NoError(t, err)
				require.Equal(t, rebuildEdgeCustody{raw: rawKey}, moved)
				for _, e := range []*graph.Edge{raw, &live} {
					claim, err := rebuildReceiptKey(b.pending, pendingEdgeKey(e))
					require.NoError(t, err)
					require.Equal(t, rawKey, claim, "both Calls claim the same receipt key")
				}
				receipts = func(yield func(ParseFacts, error) bool) { yield(facts, nil) }
			}
			if mode == "occupied scratch" {
				require.NoError(t, b.pending.Set([]byte("gRz"), []byte("sentinel"), pebble.NoSync))
			}
			before := rebuildFrameworkCustodyRows(t, b)
			stats, err := b.RebuildNgRxEffect(ctx, receipts, rules)
			if mode == "non-Call marker" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrRebuildNgRxEffect)
				switch mode {
				case "stationary raw claim":
					require.ErrorIs(t, err, ErrRebuildUnknownProvenance)
				case "action cap":
					require.ErrorIs(t, err, ErrRebuildLinkInputSize)
					require.ErrorContains(t, err, "NgRx Effect node row")
				case "occupied scratch":
					require.ErrorIs(t, err, errInvalidRecord)
					require.ErrorContains(t, err, "NgRx Effect scratch range occupied")
				}
			}
			require.Zero(t, stats)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildGRPCRefusesDuplicateRawCustody(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	raw := graph.Edge{
		From: "caller", To: "unresolved::a", Kind: graph.EdgeCalls, FilePath: "rpc.go", Line: 1,
		Meta: map[string]any{"via": "grpc.stub", "grpc_service": "Greeter", "grpc_method": "Hello"},
	}
	live := raw
	live.To = "unresolved::z"
	b.AddEdge(&raw)
	require.NoError(t, b.ApplyLinkReindexes(ctx, []graph.EdgeReindex{{Edge: &live, OldTo: raw.To}}))
	// Reuse of the old key leaves a stationary stub and a moved stub claiming the same raw key.
	b.AddEdge(&raw)
	before := rebuildFrameworkCustodyRows(t, b)
	facts := ParseFacts{
		EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: raw.FilePath}, Edges: []*graph.Edge{&raw},
	}
	receipts := func(yield func(ParseFacts, error) bool) { yield(facts, nil) }
	stats, err := b.RebuildGRPC(ctx, nil, receipts, func(*graph.Edge, string, string, string, string) bool { return false })
	require.ErrorIs(t, err, ErrRebuildGRPC)
	var refused *RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(&live)}, refused)
	require.Zero(t, stats)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
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

func TestRebuildGRPCWindowPreservesInheritedRawCustody(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	raw := graph.Edge{
		From: "caller", To: "unresolved::original", Kind: graph.EdgeCalls, FilePath: "rpc.go", Line: 1,
		Meta: map[string]any{"via": "grpc.stub", "grpc_service": "Greeter", "grpc_method": "Hello"},
	}
	old, next := raw, raw
	old.To, next.To = "unresolved::stub", "handler"
	b.AddEdge(&raw)
	require.NoError(t, b.ApplyLinkReindexes(ctx, []graph.EdgeReindex{{Edge: &old, OldTo: raw.To}}))
	move := binary.BigEndian.AppendUint64(rebuildGRPCMoveKey(0), 0)
	value, err := encodeEdge(&next)
	require.NoError(t, err)
	value = append(keyStrings(nil, string(pendingEdgeKey(&old)), next.To), value...)
	require.NoError(t, b.pending.Set(move, value, pebble.NoSync))

	pass := rebuildGRPC{b: b, stats: RebuildGRPCStats{Moves: 1}}
	require.NoError(t, b.withWrites(func() error { return pass.apply(ctx) }))
	stored, err := b.pendingEdge(pendingEdgeKey(&old))
	require.NoError(t, err)
	require.Nil(t, stored)
	stored, err = b.pendingEdge(pendingEdgeKey(&next))
	require.NoError(t, err)
	require.Equal(t, &next, stored)
	got, err := readRebuildEdgeCustody(b.pending, pendingEdgeKey(&next))
	require.NoError(t, err)
	require.Equal(t, rebuildEdgeCustody{raw: pendingEdgeKey(&raw)}, got)
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
	RequireRebuildTemporalSyntheticCustody(t, b, &resolved)
}

// RequireRebuildTemporalSyntheticCustody checks the exact proof from an external test of the resolver.
func RequireRebuildTemporalSyntheticCustody(t *testing.T, b *BuildStore, e *graph.Edge) {
	t.Helper()
	requireSyntheticCustody(t, b, map[graph.EdgeIdentity]rebuildEdgeProof{graph.EdgeIdentityFor(e): rebuildProofTemporalSynthetic})
}

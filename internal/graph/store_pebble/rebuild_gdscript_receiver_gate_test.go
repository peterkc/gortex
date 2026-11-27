package store_pebble

import (
	"crypto/sha256"
	"iter"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildGDScriptReceiverGateProof(t *testing.T) {
	for _, mode := range []string{"refresh", "unrelated large node", "same key stale payload", "row too large"} {
		t.Run(mode, func(t *testing.T) {
			b, err := OpenBuild(filepath.Join(t.TempDir(), "working"), FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, b.Close()) })
			b.AddNode(&graph.Node{ID: "owner.gd::apply", Kind: graph.KindMethod, Language: "gdscript", FilePath: "owner.gd"})
			if mode == "unrelated large node" {
				// A node the gate never reads cannot change a demotion.
				b.AddNode(&graph.Node{ID: "big.go::Big", Kind: graph.KindFunction, Language: "go", FilePath: "big.go",
					Meta: map[string]any{"doc": strings.Repeat("x", rebuildFrameworkGateRowBytes)}})
				b.AddEdge(&graph.Edge{From: "caller.go::invoke", To: "big.go::Big", Kind: graph.EdgeCalls, FilePath: "caller.go", Line: 3})
			}
			first := graph.Edge{From: "caller.go::invoke", To: "owner.gd::apply", Kind: graph.EdgeCalls, FilePath: "caller.go", Line: 1,
				Origin: graph.OriginASTResolved, Meta: map[string]any{"receiver_type": "Wrong"}}
			last := first
			last.Line = 2
			if mode == "row too large" {
				last.Context = strings.Repeat("x", rebuildFrameworkGateRowBytes)
			}
			key, lastKey := pendingEdgeKey(&first), pendingEdgeKey(&last)
			proof := rebuildEdgeCustody{proof: rebuildProofUntrackedMove}
			rawReceipt := first
			rawReceipt.To = "unresolved::*.apply"
			lineage := rebuildEdgeCustody{raw: pendingEdgeKey(&rawReceipt)}
			require.NoError(t, b.putEdgeWithCustody(&first, proof))
			require.NoError(t, b.putEdgeWithCustody(&last, lineage))
			read := func(k []byte) []byte {
				t.Helper()
				raw, err := pendingGet(b.pending, k, copyValue)
				require.NoError(t, err)
				require.NotEmpty(t, raw)
				return raw
			}
			demote := func(e *graph.Edge, _ func(string) (*graph.Node, error)) (bool, error) {
				e.Origin = graph.OriginSpeculative
				e.Meta[graph.MetaSpeculative] = true
				return true, nil
			}
			var n uint64
			switch mode {
			case "same key stale payload":
				require.NoError(t, b.planRebuildGDScriptGate(t.Context(), demote))
				// Simulate a newer payload at exactly the same identity, with
				// valid custody. The payload check must catch it before any row
				// is written, including the earlier, still-matching row.
				last.Context = "changed after planning"
				require.Equal(t, lastKey, pendingEdgeKey(&last))
				require.NoError(t, b.refreshPendingEdge(&last))
				firstBefore, lastBefore := read(key), read(lastKey)
				n, err = b.applyRebuildGDScriptGate(t.Context())
				require.ErrorIs(t, err, ErrRebuildUnknownProvenance)
				require.Zero(t, n)
				require.Equal(t, firstBefore, read(key))
				require.Equal(t, lastBefore, read(lastKey))
				return
			case "row too large":
				firstBefore, lastBefore := read(key), read(lastKey)
				n, err = b.RebuildGDScriptReceiverGate(t.Context(), func(_, _ iter.Seq2[*graph.Node, error]) (RebuildGDScriptReceiverDemoter, error) {
					return demote, nil
				})
				require.ErrorIs(t, err, ErrRebuildGDScriptReceiverGate)
				require.ErrorIs(t, err, ErrRebuildLinkInputSize)
				require.Zero(t, n)
				require.Equal(t, firstBefore, read(key))
				require.Equal(t, lastBefore, read(lastKey))
			default:
				oldProof := read(rebuildLinkRecorderKey(key))
				n, err = b.RebuildGDScriptReceiverGate(t.Context(), func(_, _ iter.Seq2[*graph.Node, error]) (RebuildGDScriptReceiverDemoter, error) {
					return demote, nil
				})
				require.NoError(t, err)
				require.EqualValues(t, 2, n)
				gotProof, err := readRebuildEdgeCustody(b.pending, key)
				require.NoError(t, err)
				require.Equal(t, proof, gotProof)
				require.NotEqual(t, oldProof, read(rebuildLinkRecorderKey(key)))
				gotRaw, err := readRebuildEdgeCustody(b.pending, lastKey)
				require.NoError(t, err)
				require.Equal(t, lineage, gotRaw)
				require.Equal(t, key, read(pendingReverseKey(&first, key)))
				require.Equal(t, lastKey, read(pendingReverseKey(&last, lastKey)))
			}
			busy, err := rebuildGuardHasPrefix(t.Context(), b.pending, rebuildGDScriptGatePrefix)
			require.NoError(t, err)
			require.False(t, busy)
		})
	}
}

func TestRebuildGDScriptReceiverGateIdentityGuards(t *testing.T) {
	const owner = "owner.gd::apply"
	for _, mode := range []string{"occupied scratch", "node identity", "edge identity", "demotion identity", "refresh identity", "invalid custody", "stale custody"} {
		t.Run(mode, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			b.AddNode(&graph.Node{ID: owner, Kind: graph.KindMethod, Language: "gdscript", FilePath: "owner.gd"})
			first := &graph.Edge{From: "caller", To: owner, Kind: graph.EdgeCalls, Line: 1, FilePath: "caller.gd"}
			last := *first
			last.Line++
			b.AddEdge(first)
			require.NoError(t, b.putEdgeWithCustody(&last, rebuildEdgeCustody{proof: rebuildProofUntrackedMove}))
			key := pendingEdgeKey(&last)
			demote := func(e *graph.Edge, _ func(string) (*graph.Node, error)) (bool, error) {
				e.Origin = graph.OriginSpeculative
				if mode == "demotion identity" && e.Line == last.Line {
					e.Line++
				}
				return true, nil
			}
			prepare := func(_, _ iter.Seq2[*graph.Node, error]) (RebuildGDScriptReceiverDemoter, error) { return demote, nil }
			var want string
			switch mode {
			case "occupied scratch":
				require.NoError(t, b.pending.Set(rebuildGDScriptGatePrefix, []byte("another owner"), pebble.NoSync))
				want = "scratch occupied"
			case "node identity":
				wrong, err := encodeNode(&graph.Node{ID: "other.gd::apply", Kind: graph.KindMethod, Language: "gdscript"})
				require.NoError(t, err)
				require.NoError(t, b.pending.Set(pendingNodeKey(owner), wrong, pebble.NoSync))
				want = "node identity"
			case "edge identity":
				wrong := last
				wrong.Line++
				raw, err := encodeEdge(&wrong)
				require.NoError(t, err)
				require.NoError(t, b.pending.Set(key, raw, pebble.NoSync))
				want = "edge identity"
			case "demotion identity":
				want = "demotion changed identity"
			case "refresh identity", "invalid custody", "stale custody":
				require.NoError(t, b.planRebuildGDScriptGate(t.Context(), demote))
				switch mode {
				case "refresh identity":
					before, err := encodeEdge(&last)
					require.NoError(t, err)
					wrong := last
					wrong.Line++
					wrong.Origin = graph.OriginSpeculative
					after, err := encodeEdge(&wrong)
					require.NoError(t, err)
					proposal := append(appendBytes(nil, before), after...)
					require.NoError(t, b.pending.Set(append(append([]byte(nil), rebuildGDScriptGatePrefix...), key...), proposal, pebble.NoSync))
					want = "refresh identity"
				case "invalid custody":
					require.NoError(t, b.pending.Set(rebuildLinkRecorderKey(key), []byte{0}, pebble.NoSync))
					want = "edge custody format"
				case "stale custody":
					wrong := sha256.Sum256([]byte("not the saved Call"))
					record := append([]byte{rebuildEdgeProofFormat, byte(rebuildProofUntrackedMove)}, wrong[:]...)
					require.NoError(t, b.pending.Set(rebuildLinkRecorderKey(key), record, pebble.NoSync))
				}
				before := rebuildFrameworkCustodyRows(t, b)
				n, err := b.applyRebuildGDScriptGate(t.Context())
				require.Zero(t, n)
				if mode == "stale custody" {
					require.ErrorIs(t, err, ErrRebuildUnknownProvenance)
				} else {
					require.ErrorContains(t, err, want)
				}
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
				return
			}
			before := rebuildFrameworkCustodyRows(t, b)
			n, err := b.RebuildGDScriptReceiverGate(t.Context(), prepare)
			require.ErrorIs(t, err, ErrRebuildGDScriptReceiverGate)
			require.ErrorContains(t, err, want)
			require.Zero(t, n)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildFrameworkGateNodeStopsAtWitness(t *testing.T) {
	b := newBuild(t, FullProfile)
	b.AddNode(&graph.Node{ID: "a", Language: "gdscript", Kind: graph.KindFile})
	b.AddNode(&graph.Node{ID: "z", Language: "go", Kind: graph.KindFile})
	count := 0
	found, err := b.RebuildView().HasFrameworkGateNode(t.Context(), func(n *graph.Node) (bool, error) {
		count++
		return n.Language == "gdscript", nil
	})
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, 1, count)
}

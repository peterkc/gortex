package store_pebble

import (
	"iter"
	"path/filepath"
	"strings"
	"testing"

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
				n, err = b.RebuildGDScriptReceiverGate(t.Context(), func(iter.Seq2[*graph.Node, error]) (RebuildGDScriptReceiverDemoter, error) {
					return demote, nil
				})
				require.ErrorIs(t, err, ErrRebuildGDScriptReceiverGate)
				require.ErrorIs(t, err, ErrRebuildLinkInputSize)
				require.Zero(t, n)
				require.Equal(t, firstBefore, read(key))
				require.Equal(t, lastBefore, read(lastKey))
			default:
				oldProof := read(rebuildLinkRecorderKey(key))
				n, err = b.RebuildGDScriptReceiverGate(t.Context(), func(iter.Seq2[*graph.Node, error]) (RebuildGDScriptReceiverDemoter, error) {
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

package store_pebble

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildReactNativeBridgeGuardLineage(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		tracked, otherFirst     bool
		overwriteRaw, loseOther bool
	}{
		{name: "untracked rejects the decoy"},
		{name: "tracked uses its original receipt", tracked: true},
		{name: "tracked does not take the decoy ordinal", tracked: true, otherFirst: true},
		{name: "overwritten moved recorder", tracked: true, overwriteRaw: true},
		{name: "lost first site beside tracked survivor", tracked: true, otherFirst: true, loseOther: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			b := newBuild(t, FullProfile)
			caller := &graph.Node{ID: "app.ts::show", Name: "show", Kind: graph.KindFunction, FilePath: "app.ts", Language: "typescript"}
			native := &graph.Node{
				ID: "Calendar.java::open", Name: "open", Kind: graph.KindMethod, FilePath: "Calendar.java", Language: "java",
				Meta: map[string]any{"rn_module": "Calendar", "rn_method": "open"},
			}
			raw := graph.Edge{
				From: caller.ID, To: "unresolved::original", Kind: graph.EdgeCalls, FilePath: caller.FilePath, Line: 30,
				Meta: map[string]any{"via": "rn.native", "rn_module": "Calendar", "rn_method": "open"},
			}
			decoy, live, other := raw, raw, raw
			decoy.To = "unresolved::reverted"
			live.To, live.Origin = "resolved", graph.OriginASTInferred
			other.Line = 4
			b.AddBatch([]*graph.Node{caller, native}, []*graph.Edge{&other})
			if tc.loseOther {
				overwritten := other
				overwritten.Meta = nil
				b.AddEdge(&overwritten)
				stored, err := b.pendingEdge(pendingEdgeKey(&other))
				require.NoError(t, err)
				require.Equal(t, &overwritten, stored)
			}
			if tc.tracked {
				b.AddEdge(&raw)
				require.NoError(t, b.ApplyLinkReindexes(ctx, []graph.EdgeReindex{{Edge: &live, OldTo: raw.To}}))
			} else {
				b.AddEdge(&live)
			}
			// The decoy is in the receipts, not the live graph. The guard
			// installs the source payload at that receipt's exact identity.
			reverted := live
			reverted.To = decoy.To
			_, err := b.ApplyRebuildGuardPage(ctx, []graph.EdgeReindex{{Edge: &reverted, OldTo: live.To}}, nil)
			require.NoError(t, err)
			view := b.RebuildView()
			inputID := graph.EdgeIdentityFor(&reverted)
			stored, err := view.Edge(ctx, inputID)
			require.NoError(t, err)
			require.NotNil(t, stored)
			require.Empty(t, stored.Origin)
			removed, err := view.Edge(ctx, graph.EdgeIdentityFor(&live))
			require.NoError(t, err)
			require.Nil(t, removed)
			if tc.tracked {
				recorder, found, err := view.OriginalLinkRecorder(ctx, inputID)
				require.NoError(t, err)
				require.True(t, found)
				require.Equal(t, graph.EdgeIdentityFor(&raw), recorder)
			}

			app := ParseFacts{
				File: graph.FileMetaRow{FilePath: caller.FilePath, NodeCount: 1}, Nodes: []*graph.Node{caller},
				EffectiveLanguage: caller.Language, Edges: []*graph.Edge{&decoy, &other},
			}
			wantLine := raw.Line
			if tc.tracked {
				app.Edges = []*graph.Edge{&decoy, &raw, &other}
			}
			if tc.otherFirst {
				app.Edges[1], app.Edges[2] = app.Edges[2], app.Edges[1]
				wantLine = other.Line
			}
			if tc.overwriteRaw {
				// The last receipt at the original key is not a marker.
				// The eligible decoy at the live key cannot replace it.
				last := raw
				last.Meta = nil
				app.Edges = append(app.Edges, &last)
			}
			java := ParseFacts{
				File: graph.FileMetaRow{FilePath: native.FilePath, NodeCount: 1}, Nodes: []*graph.Node{native},
				EffectiveLanguage: native.Language,
			}
			receiptReads := 0
			receipts := func(yield func(ParseFacts, error) bool) {
				receiptReads++
				if yield(app, nil) {
					yield(java, nil)
				}
			}
			before := rebuildFrameworkCustodyRows(t, b)
			offers := 0
			stats, err := b.RebuildReactNativeBridge(ctx, receipts,
				func(language string) bool { return language == "typescript" || language == "java" },
				func(e *graph.Edge) bool {
					return e.Kind == graph.EdgeCalls && e.From != "" && e.Meta["via"] == "rn.native"
				},
				func(e *graph.Edge, n *graph.Node) *graph.Edge {
					offers++
					return &graph.Edge{From: e.From, To: n.ID, Kind: graph.EdgeCalls, FilePath: e.FilePath, Line: e.Line}
				},
			)
			if !tc.tracked || tc.overwriteRaw || tc.loseOther {
				var refusal *RebuildUnknownProvenanceError
				require.ErrorIs(t, err, ErrRebuildReactNativeBridge)
				require.ErrorAs(t, err, &refusal)
				require.Equal(t, uint64(1), refusal.Count)
				want := graph.EdgeIdentity{From: caller.ID, To: native.ID, Kind: graph.EdgeCalls, FilePath: caller.FilePath, Line: wantLine}
				require.Equal(t, want, refusal.First)
				require.Zero(t, stats)
				if tc.tracked {
					require.Equal(t, 1, receiptReads)
				} else {
					require.Zero(t, receiptReads)
				}
				require.Zero(t, offers)
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
				return
			}
			require.NoError(t, err)
			require.Equal(t, uint64(1), stats.Edges)
			require.Equal(t, 1, offers)
			want := &graph.Edge{From: caller.ID, To: native.ID, Kind: graph.EdgeCalls, FilePath: caller.FilePath, Line: wantLine}
			installed, err := view.Edge(ctx, graph.EdgeIdentityFor(want))
			require.NoError(t, err)
			require.Equal(t, want, installed)
			custody, err := readRebuildEdgeCustody(b.pending, pendingEdgeKey(want))
			require.NoError(t, err)
			require.Equal(t, rebuildEdgeCustody{proof: rebuildProofRNBridge}, custody)
		})
	}
}

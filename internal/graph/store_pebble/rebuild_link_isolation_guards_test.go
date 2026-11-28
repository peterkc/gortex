package store_pebble

import (
	"context"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildLinkIsolationCutGuards(t *testing.T) {
	for _, mode := range []string{"parse after cut", "second cut", "cut during overlay", "restore before cut", "restore during overlay", "skip after cut", "nil skip", "nonfile skip", "empty skip ID"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			b := newBuild(t, FullProfile)
			file := &graph.Node{ID: "skip.py", FilePath: "skip.py", Kind: graph.KindFile, Language: "python"}
			var err error
			switch mode {
			case "parse after cut":
				require.NoError(t, b.IsolateRebuildLinks(ctx))
				err = b.AddParsedFile("", ParseFacts{File: graph.FileMetaRow{FilePath: file.FilePath, NodeCount: 1}, EffectiveLanguage: "python", Nodes: []*graph.Node{file}})
			case "second cut":
				require.NoError(t, b.IsolateRebuildLinks(ctx))
				err = b.IsolateRebuildLinks(ctx)
			case "cut during overlay":
				require.NoError(t, b.BeginRebuildContractOverlay(ctx, map[string]string{}))
				err = b.IsolateRebuildLinks(ctx)
			case "restore before cut":
				err = b.RestoreRebuildDeclarations(ctx)
			case "restore during overlay":
				require.NoError(t, b.IsolateRebuildLinks(ctx))
				require.NoError(t, b.BeginRebuildContractOverlay(ctx, map[string]string{}))
				err = b.RestoreRebuildDeclarations(ctx)
			case "skip after cut":
				require.NoError(t, b.IsolateRebuildLinks(ctx))
				err = b.AddRebuildHeldSkip(file)
			case "nil skip":
				err = b.AddRebuildHeldSkip(nil)
			case "nonfile skip":
				file.Kind = graph.KindType
				err = b.AddRebuildHeldSkip(file)
			case "empty skip ID":
				file.ID = ""
				err = b.AddRebuildHeldSkip(file)
			}
			require.ErrorIs(t, err, ErrRebuildLinkIsolation)
		})
	}
}

func TestRebuildLinkIsolationRefusesInvalidHeldRows(t *testing.T) {
	for _, mode := range []string{"key mismatch", "nonraw custody", "canceled selection entry"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			b := newBuild(t, FullProfile)
			const file = "held.py"
			parent := &graph.Node{ID: file, FilePath: file, Kind: graph.KindFile}
			child := &graph.Node{ID: file + "::A", FilePath: file, Kind: graph.KindType}
			edge := &graph.Edge{From: parent.ID, To: child.ID, Kind: graph.EdgeDefines, FilePath: file}
			facts := ParseFacts{File: graph.FileMetaRow{FilePath: file, NodeCount: 2}, EffectiveLanguage: "python", Nodes: []*graph.Node{parent, child}, Edges: []*graph.Edge{edge}}
			require.NoError(t, b.AddRebuildDeferredFile("", facts, func(string) (bool, error) { return true, nil }))
			switch mode {
			case "key mismatch":
				raw, err := pendingGet(b.pending, rebuildHoldoutKey('n', pendingNodeKey(parent.ID)), copyValue)
				require.NoError(t, err)
				require.NoError(t, b.pending.Set(rebuildHoldoutKey('n', pendingNodeKey(child.ID)), raw, pebble.NoSync))
				require.ErrorContains(t, b.IsolateRebuildLinks(ctx), "held node key differs from capture")
			case "nonraw custody":
				require.NoError(t, b.putEdgeWithCustody(edge, rebuildEdgeCustody{proof: rebuildProofGoDataflowSource}))
				require.ErrorContains(t, b.IsolateRebuildLinks(ctx), "held containment edge has non-raw custody")
			case "canceled selection entry":
				require.NoError(t, b.IsolateRebuildLinks(ctx))
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				_, err := b.SelectRebuildLinkFacts(canceled, facts)
				require.ErrorIs(t, err, context.Canceled)
			}
		})
	}
}

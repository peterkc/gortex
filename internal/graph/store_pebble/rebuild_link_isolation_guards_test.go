package store_pebble

import (
	"context"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildLinkIsolationCutGuards(t *testing.T) {
	for _, mode := range []string{"parse after cut", "second cut", "cut during overlay", "restore before cut", "restore during overlay", "skip after cut", "nil skip", "nonfile skip", "empty skip ID", "selected skip after cut", "nil selected skip", "nonfile selected skip", "empty selected skip ID", "deferred selected skip"} {
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
			case "selected skip after cut":
				require.NoError(t, b.IsolateRebuildLinks(ctx))
				file.Language = "go"
				err = b.AddRebuildSelectedSkip(file)
			case "nil selected skip":
				err = b.AddRebuildSelectedSkip(nil)
			case "nonfile selected skip":
				file.Kind = graph.KindType
				file.Language = "go"
				err = b.AddRebuildSelectedSkip(file)
			case "empty selected skip ID":
				file.ID = ""
				file.Language = "go"
				err = b.AddRebuildSelectedSkip(file)
			case "deferred selected skip":
				err = b.AddRebuildSelectedSkip(file)
			}
			require.ErrorIs(t, err, ErrRebuildLinkIsolation)
		})
	}
}

func TestRebuildLinkIsolationRefusesInvalidHeldRows(t *testing.T) {
	for _, mode := range []string{"key mismatch", "nonraw custody", "canceled selection entry", "canceled selection lookup", "missing held receipt extractor"} {
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
			case "canceled selection lookup":
				require.NoError(t, b.IsolateRebuildLinks(ctx))
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				// Enter the same per-ID loop used after a successful prefix check.
				_, err := b.selectRebuildLinkFacts(canceled, facts)
				require.ErrorIs(t, err, context.Canceled)
				require.ErrorIs(t, err, ErrRebuildLinkIsolation)
			case "missing held receipt extractor":
				require.NoError(t, b.pending.Delete(rebuildHoldoutKey('a', keyStrings(nil, file)), pebble.NoSync))
				err := b.IsolateRebuildLinks(ctx)
				require.ErrorIs(t, err, ErrRebuildLinkIsolation)
				require.ErrorContains(t, err, "held edge lacks deferred receipt attribution")
			}
		})
	}
}

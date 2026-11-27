package resolver

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildValueRefsSQLiteOracle(t *testing.T) {
	const file, name, lang = "value.go", "LIMIT_KEY", "go"
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthValueRefName})
	require.NoError(t, err)
	for _, mode := range []string{"nearest declaration", "shadow", "bound terminal retained", "receipt entrant", "shared raw claim", "joint omissions", "unrelated large local", "large declaration"} {
		t.Run(mode, func(t *testing.T) {
			reader := rebuildEventNode(file, "read", lang, "")
			first := rebuildEventNode(file, "a-constant", lang, "")
			first.Kind, first.Name, first.StartLine = graph.KindConstant, name, 9
			last := rebuildEventNode(file, "z-constant", lang, "")
			last.Kind, last.Name, last.StartLine = graph.KindVariable, name, 9
			shadow := rebuildEventNode(file, "read#limit", lang, "")
			shadow.Kind, shadow.Name = graph.KindLocal, name
			use := &graph.Edge{
				From: reader.ID, To: "unresolved::valueref::" + name, Kind: graph.EdgeReads, FilePath: file, Line: 12,
				Meta: map[string]any{"via": valueRefCandidateVia, "name": name},
			}
			nodes := []*graph.Node{reader, last, first}
			var alternative *graph.Node
			switch mode {
			case "bound terminal retained":
				use.To = last.ID
				use.Meta[metaResolveTerminal] = true
				use.Meta[metaResolveTerminalReason] = "fixture failure"
			case "shadow", "joint omissions":
				nodes = append(nodes, shadow)
			case "receipt entrant":
				alternative = rebuildEventNode(file, "candidate", lang, "")
				alternative.Name, alternative.StartLine = name, 11
				nodes = append(nodes, alternative)
			case "unrelated large local":
				other := rebuildEventNode(file, "other#limit", lang, "")
				other.Kind, other.Name = graph.KindLocal, name
				other.Meta = map[string]any{"large": strings.Repeat("x", 16<<20)}
				nodes = append(nodes, other)
			case "large declaration":
				last.Meta = map[string]any{"large": strings.Repeat("x", 16<<20)}
			}
			facts := rebuildEventFacts(file, lang, nodes, use)
			raw := facts
			switch mode {
			case "receipt entrant":
				changed := *alternative
				changed.Kind = graph.KindConstant
				raw.Nodes = append(nodes[:3:3], &changed)
			case "joint omissions":
				raw.Nodes = []*graph.Node{reader}
			}
			raw.File.NodeCount = len(raw.Nodes)
			b, native := openRebuildEventStores(t, facts)
			if mode == "shared raw claim" {
				mover := *use
				mover.To = "unresolved::valueref::moved"
				require.NoError(t, b.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{{Edge: &mover, OldTo: use.To}}))
				stationary := *use
				stationary.Meta = nil
				b.AddEdge(&stationary)
			}
			before := rebuildEventSavedRows(t, b)
			selected := selection
			// Both oversized cases test this stage's role limit, not the census limit.
			if mode != "unrelated large local" && mode != "large declaration" {
				frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
				require.NoError(t, err)
				require.True(t, frozen.allows(SynthValueRefName))
				selected = frozen
			}
			count, err := RunRebuildValueRefs(t.Context(), b, rebuildGRPCReceipts(raw), selected)
			switch mode {
			case "nearest declaration", "unrelated large local":
				require.NoError(t, err)
				require.EqualValues(t, 1, count)
				require.Equal(t, 1, ResolveValueRefs(native))
				want := rebuildEventSQLiteRows(native)
				require.Len(t, want.edges, 1)
				require.Equal(t, last.ID, want.edges[0].To)
				require.Equal(t, true, want.edges[0].Meta["conditional_def"])
				require.Equal(t, want, rebuildEventSavedRows(t, b))
			case "shadow", "bound terminal retained":
				require.NoError(t, err)
				require.Zero(t, count)
				require.Zero(t, ResolveValueRefs(native))
				require.Equal(t, before, rebuildEventSQLiteRows(native))
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				if mode == "bound terminal retained" {
					require.Len(t, before.edges, 1)
					require.Equal(t, true, before.edges[0].Meta[metaResolveTerminal])
					require.Equal(t, "fixture failure", before.edges[0].Meta[metaResolveTerminalReason])
				}
			default:
				require.ErrorIs(t, err, store_pebble.ErrRebuildValueRefs)
				if mode == "large declaration" {
					require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
				} else {
					require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
				}
				require.Zero(t, count)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				if mode == "joint omissions" {
					require.Zero(t, ResolveValueRefs(native))
					_, omitted := openRebuildEventStores(t, raw)
					require.Zero(t, ResolveValueRefs(omitted))
					one := rebuildEventFacts(file, lang, []*graph.Node{reader, first}, use)
					_, partial := openRebuildEventStores(t, one)
					require.Equal(t, 1, ResolveValueRefs(partial))
				}
				if mode == "receipt entrant" {
					require.Equal(t, 1, ResolveValueRefs(native))
					_, receipts := openRebuildEventStores(t, raw)
					require.Equal(t, 1, ResolveValueRefs(receipts))
					require.Equal(t, last.ID, rebuildEventSQLiteRows(native).edges[0].To)
					require.Equal(t, alternative.ID, rebuildEventSQLiteRows(receipts).edges[0].To)
				}
			}
		})
	}
}

package resolver

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildFactoryChainSQLiteOracle(t *testing.T) {
	const source, decl, lang = "app/main.go", "libs/worker.go", "go"
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthFactoryChain})
	require.NoError(t, err)
	for _, mode := range []string{"conformance", "raw factory entrant", "shared raw claim", "joint omitted factories", "unrelated large row", "related large row"} {
		t.Run(mode, func(t *testing.T) {
			caller := rebuildEventNode(source, "use", lang, "")
			seed := rebuildEventNode(decl, "MakeWorker", lang, "")
			seed.Meta = map[string]any{"return_type": "Worker"}
			contract := rebuildEventNode(decl, "Worker", lang, "")
			contract.Kind = graph.KindInterface
			concrete := rebuildEventNode(decl, "RealWorker", lang, "")
			concrete.Kind = graph.KindType
			method := rebuildEventNode(decl, "RealWorker.Execute", lang, "")
			method.Kind, method.Name = graph.KindMethod, "Execute"
			method.Meta = map[string]any{"receiver": concrete.Name}
			arc := &graph.Edge{From: concrete.ID, To: contract.ID, Kind: graph.EdgeImplements, FilePath: decl, Line: 3}
			call := &graph.Edge{From: caller.ID, To: "unresolved::*.Execute", Kind: graph.EdgeCalls, FilePath: source, Line: 7,
				Meta: map[string]any{"receiver_expr": "MakeWorker()"}}
			ref := *call
			ref.Kind, ref.Line = graph.EdgeReferences, 8
			route := rebuildEventFacts(source, lang, []*graph.Node{caller}, call, &ref)
			nodes := []*graph.Node{seed, contract, concrete, method}
			extra := rebuildEventNode(decl, "AlternativeFactory", lang, "")
			extra.Meta = map[string]any{"return_type": "Alternative"}
			switch mode {
			case "raw factory entrant":
				nodes = append(nodes, extra)
			case "joint omitted factories":
				extra.Name = seed.Name
				nodes = append(nodes, extra)
			case "unrelated large row", "related large row":
				extra.Name = seed.Name
				extra.Meta["large"] = strings.Repeat("x", 16<<20)
				if mode == "unrelated large row" {
					extra.Kind = graph.KindVariable
				}
				nodes = append(nodes, extra)
			}
			live := rebuildEventFacts(decl, lang, nodes, arc)
			raw := live
			switch mode {
			case "raw factory entrant":
				changed := *extra
				changed.Name = seed.Name
				raw.Nodes = append(nodes[:4:4], &changed)
			case "joint omitted factories":
				raw.Nodes = nodes[1:4]
			}
			raw.File.NodeCount = len(raw.Nodes)
			b, native := openRebuildEventStores(t, route, live)
			if mode == "shared raw claim" {
				mover := ref
				mover.To = "unresolved::Execute"
				require.NoError(t, b.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{{Edge: &mover, OldTo: ref.To}}))
				stationary := ref
				stationary.Meta = nil
				b.AddEdge(&stationary)
			}
			before := rebuildEventSavedRows(t, b)
			selected := selection
			// The census has its own node limit. Exercise the stage's role
			// limit directly for these two deliberately oversized fixtures.
			if mode != "unrelated large row" && mode != "related large row" {
				frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
				require.NoError(t, err)
				require.True(t, frozen.allows(SynthFactoryChain))
				selected = frozen
			}
			count, err := RunRebuildFactoryChain(t.Context(), b, rebuildGRPCReceipts(route, raw), selected)
			switch mode {
			case "conformance", "unrelated large row":
				require.NoError(t, err)
				require.EqualValues(t, 2, count)
				require.Equal(t, 2, ResolveFactoryChains(native))
				want := rebuildEventSQLiteRows(native)
				var moved int
				for _, e := range want.edges {
					if e.Kind == graph.EdgeCalls || e.Kind == graph.EdgeReferences {
						moved++
						require.Equal(t, method.ID, e.To)
						require.Equal(t, true, e.Meta["conformance_walked"])
						require.Equal(t, "factory_chain", e.Meta["via"])
					}
				}
				require.Equal(t, 2, moved)
				require.Equal(t, want, rebuildEventSavedRows(t, b))
			default:
				require.ErrorIs(t, err, store_pebble.ErrRebuildFactoryChain)
				if mode == "related large row" {
					require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
				} else {
					require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
				}
				require.Zero(t, count)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				if mode == "joint omitted factories" {
					// Both endpoints agree on a miss, but one factory can
					// be present while the other is absent.
					require.Zero(t, ResolveFactoryChains(native))
					_, absent := openRebuildEventStores(t, route, raw)
					require.Zero(t, ResolveFactoryChains(absent))
					one := rebuildEventFacts(decl, lang, nodes[:4], arc)
					_, present := openRebuildEventStores(t, route, one)
					require.Equal(t, 2, ResolveFactoryChains(present))
				}
				if mode == "raw factory entrant" {
					require.Equal(t, 2, ResolveFactoryChains(native))
					_, receipt := openRebuildEventStores(t, route, raw)
					require.Zero(t, ResolveFactoryChains(receipt))
				}
			}
		})
	}
}

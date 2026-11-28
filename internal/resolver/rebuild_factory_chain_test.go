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
	for _, mode := range []string{
		"conformance", "raw factory entrant", "shared raw claim", "joint omitted factories",
		"unproved method seed", "unproved free seed", "irrelevant raw arc", "inert raw arc",
		"joint raw arc entrants", "raw arc with live-only method", "proved-only arc match", "proved seed ambiguity",
		"unrelated large row", "related large row", "lost receiver before freeze", "lost receiver after freeze",
	} {
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
			if mode == "lost receiver before freeze" || mode == "lost receiver after freeze" {
				ref.Meta = nil // The Call is the only possible frozen candidate.
			}
			route := rebuildEventFacts(source, lang, []*graph.Node{caller}, call, &ref)
			nodes := []*graph.Node{seed, contract, concrete, method}
			extra := rebuildEventNode(decl, "AlternativeFactory", lang, "")
			extra.Meta = map[string]any{"return_type": "Alternative"}
			alt := rebuildEventNode(decl, "AltWorker", lang, "")
			alt.Kind = graph.KindType
			altMethod := rebuildEventNode(decl, "AltWorker.Execute", lang, "")
			altMethod.Kind, altMethod.Name = graph.KindMethod, method.Name
			altMethod.Meta = map[string]any{"receiver": alt.Name}
			altArc := &graph.Edge{From: alt.ID, To: contract.ID, Kind: graph.EdgeImplements, FilePath: decl, Line: 4}
			switch mode {
			case "raw factory entrant":
				nodes = append(nodes, extra)
			case "joint omitted factories", "unproved free seed", "unproved method seed", "proved seed ambiguity":
				extra.Name = seed.Name
				if mode == "unproved method seed" || mode == "proved seed ambiguity" {
					extra.Kind = graph.KindMethod
					extra.Meta["receiver"] = "Unrelated"
				}
				nodes = append(nodes, extra)
				if mode == "proved seed ambiguity" {
					ambiguous := rebuildEventNode(decl, "AmbiguousFactory", lang, "")
					ambiguous.Name = seed.Name
					ambiguous.Meta = map[string]any{"return_type": "Other"}
					nodes = append(nodes, ambiguous)
				}
			case "irrelevant raw arc", "inert raw arc", "joint raw arc entrants":
				nodes = append(nodes, alt, altMethod)
			case "proved-only arc match":
				nodes = append(nodes, alt)
			case "unrelated large row", "related large row":
				extra.Name = seed.Name
				extra.Meta["large"] = strings.Repeat("x", 16<<20)
				if mode == "unrelated large row" {
					extra.Kind = graph.KindVariable
				}
				nodes = append(nodes, extra)
			}
			live := rebuildEventFacts(decl, lang, nodes, arc)
			if mode == "joint raw arc entrants" || mode == "raw arc with live-only method" {
				live.Edges = nil
			}
			raw := live
			switch mode {
			case "raw factory entrant":
				changed := *extra
				changed.Name = seed.Name
				raw.Nodes = append(nodes[:4:4], &changed)
			case "joint omitted factories":
				raw.Nodes = nodes[1:4]
			case "raw arc with live-only method":
				raw.Edges = []*graph.Edge{arc}
				raw.Nodes = nodes[:3]
			case "proved-only arc match":
				// Keep the method's receipt exact. Only the implementor names change,
				// so each walk reaches that method through a different arc.
				rawConcrete, rawAlt := *concrete, *alt
				rawConcrete.Name, rawAlt.Name = alt.Name, concrete.Name
				raw.Nodes = []*graph.Node{seed, contract, &rawConcrete, method, &rawAlt}
				raw.Edges = []*graph.Edge{arc, altArc}
			case "unproved free seed":
				raw.Nodes = nodes[:3]
			case "unproved method seed":
				raw.Nodes = nodes[:4]
			case "proved seed ambiguity":
				raw.Nodes = append(append([]*graph.Node(nil), nodes[:4]...), nodes[5])
			case "joint raw arc entrants":
				raw.Edges = []*graph.Edge{arc, altArc}
			case "irrelevant raw arc", "inert raw arc":
				if mode == "irrelevant raw arc" {
					altArc.To = concrete.ID
				} else {
					altMethod.Name = "Unrelated"
				}
				raw.Edges = append(append([]*graph.Edge(nil), live.Edges...), altArc)
			}
			raw.File.NodeCount = len(raw.Nodes)
			lost := *call
			lost.Meta = nil
			if mode == "lost receiver before freeze" {
				// SQLite keeps the first payload; pending keeps the last.
				route.Edges = append(route.Edges, &lost)
			}
			b, native := openRebuildEventStores(t, route, live)
			if mode == "shared raw claim" {
				mover := ref
				mover.To = "unresolved::Execute"
				require.NoError(t, b.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{{Edge: &mover, OldTo: ref.To}}))
				stationary := ref
				stationary.Meta = nil
				b.AddEdge(&stationary)
			}
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
			require.NoError(t, err)
			require.True(t, frozen.allows(SynthFactoryChain))
			require.NoError(t, FreezeRebuildFactoryChainInputs(t.Context(), b, frozen))
			if mode == "lost receiver after freeze" {
				b.AddEdge(&lost)
			}
			before := rebuildEventSavedRows(t, b)
			count, err := RunRebuildFactoryChain(t.Context(), b, rebuildGRPCReceipts(route, raw), frozen)
			switch mode {
			case "proved seed ambiguity":
				require.NoError(t, err)
				require.Zero(t, count)
				require.Zero(t, ResolveFactoryChains(native))
				require.Equal(t, before, rebuildEventSavedRows(t, b))
			case "conformance", "unrelated large row", "unproved method seed", "irrelevant raw arc", "inert raw arc":
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
				switch mode {
				case "joint omitted factories", "unproved free seed", "raw arc with live-only method":
					// Both complete projections miss. Keeping only the first
					// factory, its method and the arc resolves both uses.
					require.Zero(t, ResolveFactoryChains(native))
					_, absent := openRebuildEventStores(t, route, raw)
					require.Zero(t, ResolveFactoryChains(absent))
					one := rebuildEventFacts(decl, lang, nodes[:4], arc)
					_, present := openRebuildEventStores(t, route, one)
					require.Equal(t, 2, ResolveFactoryChains(present))
				case "joint raw arc entrants":
					require.Zero(t, ResolveFactoryChains(native))
					_, both := openRebuildEventStores(t, route, raw)
					require.Zero(t, ResolveFactoryChains(both))
					raw.Edges = []*graph.Edge{arc}
					_, one := openRebuildEventStores(t, route, raw)
					require.Equal(t, 2, ResolveFactoryChains(one))
				case "lost receiver before freeze", "lost receiver after freeze":
					require.Equal(t, 1, ResolveFactoryChains(native))
				case "proved-only arc match":
					_, receipt := openRebuildEventStores(t, route, raw)
					require.Equal(t, 2, ResolveFactoryChains(native))
					require.Equal(t, 2, ResolveFactoryChains(receipt))
					for _, edges := range [][]*graph.Edge{native.AllEdges(), receipt.AllEdges()} {
						for _, e := range edges {
							if e.Kind == graph.EdgeCalls || e.Kind == graph.EdgeReferences {
								require.Equal(t, method.ID, e.To)
								require.Equal(t, true, e.Meta["conformance_walked"])
							}
						}
					}
				case "raw factory entrant":
					require.Equal(t, 2, ResolveFactoryChains(native))
					_, receipt := openRebuildEventStores(t, route, raw)
					require.Zero(t, ResolveFactoryChains(receipt))
				}
			}
		})
	}
}

func TestRebuildFactoryChainIgnoresLateCandidate(t *testing.T) {
	const file = "worker.go"
	factory := rebuildEventNode(file, "New", "go", "")
	factory.Meta = map[string]any{"return_type": "Worker"}
	m := rebuildEventNode(file, "Run", "go", "")
	m.Kind = graph.KindMethod
	m.Meta = map[string]any{"receiver": "Worker"}
	caller := rebuildEventNode(file, "caller", "go", "")
	nodes := []*graph.Node{factory, m, caller}
	b := openRebuildGRPCBuild(t, rebuildEventFacts(file, "go", nodes))
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthFactoryChain})
	require.NoError(t, err)
	require.NoError(t, FreezeRebuildFactoryChainInputs(t.Context(), b, selection))
	late := &graph.Edge{From: caller.ID, To: "unresolved::*.Run", Kind: graph.EdgeCalls, FilePath: file, Line: 2,
		Meta: map[string]any{"receiver_expr": "New()"}}
	b.AddEdge(late)
	before := rebuildEventSavedRows(t, b)
	count, err := RunRebuildFactoryChain(t.Context(), b, rebuildGRPCReceipts(rebuildEventFacts(file, "go", nodes, late)), selection)
	require.NoError(t, err)
	require.Zero(t, count)
	require.Equal(t, before, rebuildEventSavedRows(t, b))
}

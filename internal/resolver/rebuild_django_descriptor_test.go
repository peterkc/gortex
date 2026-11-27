package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildDjangoDescriptorSQLiteOracle(t *testing.T) {
	const file, lang = "query.go", "go"
	for _, mode := range []string{"normal", "disabled", "missing receipt", "shared receipt"} {
		t.Run(mode, func(t *testing.T) {
			caller := rebuildEventNode(file, "call", lang, "")
			caller.Meta = map[string]any{"receiver": "QuerySet"}
			plain := rebuildEventNode(file, "plain", lang, "")
			class := func(path, hint string) *graph.Node {
				n := rebuildEventNode(path, "QuerySet", "python", "")
				n.Kind = graph.KindType
				n.Meta = map[string]any{"django_iterable_class": hint}
				return n
			}
			method := func(path, receiver string) *graph.Node {
				n := rebuildEventNode(path, "__iter__", "python", "")
				n.Kind = graph.KindMethod
				n.Meta = map[string]any{"receiver": receiver}
				return n
			}
			first := method("a.py", "Custom")
			last := method("z.py", "Custom")
			fallback := method("m.py", "ModelIterable")
			claim := func(from, to string, kind graph.EdgeKind, line int, payload string) *graph.Edge {
				return &graph.Edge{From: from, To: to, Kind: kind, FilePath: file, Line: line,
					Meta: map[string]any{"payload": payload}}
			}
			winning := claim(caller.ID, "unresolved::_iterable_class", graph.EdgeCalls, 7, "receipt first")
			losing := claim(caller.ID, "unresolved::*._iterable_class", graph.EdgeCalls, 7, "receipt second")
			ref := claim(plain.ID, "unresolved::_iterable_class", graph.EdgeReferences, 8, "reference")
			occupied := claim(caller.ID, first.ID, graph.EdgeCalls, 9, "unmoved incumbent")
			collides := claim(caller.ID, "unresolved::_iterable_class", graph.EdgeCalls, 9, "collides")
			sibling := claim(caller.ID, "unresolved::_iterable_other", graph.EdgeCalls, 7, "unrelated")
			goFacts := rebuildEventFacts(file, lang, []*graph.Node{caller, plain}, winning, losing, ref, occupied, collides, sibling)
			facts := []store_pebble.ParseFacts{
				goFacts,
				rebuildEventFacts("z.py", "python", []*graph.Node{last, class("z.py", "ModelIterable")}),
				rebuildEventFacts("m.py", "python", []*graph.Node{fallback}),
				rebuildEventFacts("a.py", "python", []*graph.Node{first, class("a.py", "Custom")}),
			}
			b, sqlite := openRebuildEventStores(t, facts...)
			selection := FrameworkSynthesizerSelection{}
			switch mode {
			case "disabled":
				var err error
				selection, err = NewFrameworkSynthesizerSelection([]string{})
				require.NoError(t, err)
			case "missing receipt":
				facts[0].Edges = goFacts.Edges[1:]
			case "shared receipt":
				moved := *winning
				moved.To = "unresolved::different"
				require.NoError(t, b.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{{Edge: &moved, OldTo: winning.To}}))
				b.AddEdge(winning)
			}
			before := rebuildEventSavedRows(t, b)
			count, err := RunRebuildDjangoDescriptor(t.Context(), b, rebuildGRPCReceipts(facts...), selection)
			switch mode {
			case "normal":
				require.NoError(t, err)
				require.EqualValues(t, 4, count)
				require.Equal(t, 4, RunClaimingResolvers(sqlite)[SynthDjangoDescriptor])
				want := rebuildEventSQLiteRows(sqlite)
				require.Len(t, want.edges, 4)
				require.Equal(t, want, rebuildEventSavedRows(t, b))
				for _, e := range want.edges {
					switch {
					case e.Line == 7 && e.To != sibling.To:
						require.Equal(t, first.ID, e.To)
						require.Equal(t, "receipt first", e.Meta["payload"])
					case e.Line == 8:
						require.Equal(t, fallback.ID, e.To)
					case e.Line == 9:
						require.Equal(t, "unmoved incumbent", e.Meta["payload"])
					}
				}
				count, err = RunRebuildDjangoDescriptor(t.Context(), b, rebuildGRPCReceipts(facts...), selection)
				require.NoError(t, err)
				require.Zero(t, count)
				require.Equal(t, want, rebuildEventSavedRows(t, b))
			case "disabled":
				require.NoError(t, err)
				require.Zero(t, count)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
			default:
				require.ErrorIs(t, err, store_pebble.ErrRebuildDjangoDescriptor)
				require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
				require.Zero(t, count)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
			}
		})
	}
}

package resolver

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildReactSetStateSQLiteOracle(t *testing.T) {
	for _, spillAt := range []int{1, 1 << 20} {
		t.Run(fmt.Sprint(spillAt), func(t *testing.T) {
			facts, want := rebuildReactSetStateFixture("widget.go", spillAt == 1<<20)
			b, native, proof := openRebuildReactSetStateStores(t, facts)
			before := rebuildEventSavedRows(t, b)
			disabled, err := NewFrameworkSynthesizerSelection(nil)
			require.NoError(t, err)
			stats, err := RunRebuildReactSetState(t.Context(), b, rebuildGRPCReceipts(proof...), disabled, spillAt)
			require.NoError(t, err)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, b))

			selection := rebuildReactSetStateSelection(t)
			// Pin the actual native first-membership order. The SQL adjacency
			// query does not have ORDER BY; the fixture ties the sites' lines.
			for _, name := range []string{"refresh", "secondaryOnly"} {
				id := "widget.go::" + name
				var owners []string
				for _, e := range native.GetOutEdgesByNodeIDs([]string{id})[id] {
					if e.Kind == graph.EdgeMemberOf {
						owners = append(owners, e.To)
					}
				}
				if name == "refresh" {
					require.Equal(t, []string{"widget.go::Z-owner", "widget.go::A-other"}, owners)
				} else {
					require.Equal(t, []string{"widget.go::A-other", "widget.go::Z-owner"}, owners)
				}
			}
			for run := range 2 {
				report := RunFrameworkSynthesizersWithSelection(native, selection)
				require.NoError(t, report.Err)
				oracle := rebuildEventSQLiteRows(native)
				// One durable incumbent also has the synthesized output key.
				require.Len(t, oracle.edges, len(before.edges)+len(want))
				for _, e := range want {
					require.Contains(t, oracle.edges, e)
				}
				for _, e := range before.edges {
					require.Contains(t, oracle.edges, e)
				}
				stats, err := RunRebuildReactSetState(t.Context(), b, rebuildGRPCReceipts(proof...), selection, spillAt)
				require.NoError(t, err)
				require.Zero(t, stats.SpillBytes)
				if run == 0 {
					require.Positive(t, stats.Edges)
				}
				require.Equal(t, oracle, rebuildEventSavedRows(t, b))
			}
		})
	}
}

func TestRebuildReactSetStateGoSourceSQLiteOracle(t *testing.T) {
	const file = "widget.go"
	const class = file + "::Widget"
	facts, err := languages.NewGoExtractor().Extract(file, []byte(`package p
type Widget struct{}
func (w *Widget) render() {}
func (w *Widget) build() {}
func (w *Widget) setState() {}
func (w *Widget) refresh() {
    w.setState()
}
func (w *Widget) noop() {}
`))
	require.NoError(t, err)
	t.Cleanup(facts.ReleaseTree)
	p := rebuildEventFacts(file, "go", facts.Nodes, facts.Edges...)
	b, native, proof := openRebuildReactSetStateStores(t, p)
	before := rebuildEventSavedRows(t, b)
	selection := rebuildReactSetStateSelection(t)
	report := RunFrameworkSynthesizersWithSelection(native, selection)
	require.NoError(t, report.Err)
	require.Equal(t, 1, report.Total)
	want := rebuildReactSetStateOutput(class+".refresh", class+".render", class, file, 6)
	oracle := rebuildEventSQLiteRows(native)
	require.Len(t, oracle.edges, len(before.edges)+1)
	require.Contains(t, oracle.edges, want)
	stats, err := RunRebuildReactSetState(t.Context(), b, rebuildGRPCReceipts(proof...), selection, 1)
	require.NoError(t, err)
	require.Equal(t, uint64(1), stats.Edges)
	require.Zero(t, stats.SpillBytes)
	require.Equal(t, oracle, rebuildEventSavedRows(t, b))
}

func TestRebuildReactSetStateRefusesMissingProof(t *testing.T) {
	for _, mode := range []string{"first membership", "only matching call", "declaration"} {
		t.Run(mode, func(t *testing.T) {
			valid, _ := rebuildReactSetStateFixture("a.go", false)
			invalid, _ := rebuildReactSetStateFixture("z.go", false)
			b, native, proof := openRebuildReactSetStateStores(t, valid, invalid)
			before := rebuildEventSavedRows(t, b)
			selection := rebuildReactSetStateSelection(t)
			report := RunFrameworkSynthesizersWithSelection(native, selection)
			require.NoError(t, report.Err)
			require.Positive(t, report.Total)
			const caller = "z.go::refresh"
			switch mode {
			case "first membership":
				proof[1].Edges = slices.DeleteFunc(proof[1].Edges, func(e *graph.Edge) bool {
					return e.From == caller && e.Kind == graph.EdgeMemberOf && e.To == "z.go::Z-owner"
				})
			case "only matching call":
				proof[1].Edges = slices.DeleteFunc(proof[1].Edges, func(e *graph.Edge) bool {
					return e.From == caller && e.Kind == graph.EdgeCalls && e.To == "unresolved::setState"
				})
			case "declaration":
				for _, n := range proof[1].Nodes {
					if n.ID == caller {
						n.StartLine++
					}
				}
			}
			stats, err := RunRebuildReactSetState(t.Context(), b, rebuildGRPCReceipts(proof...), selection, 1)
			require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
		})
	}
}

func rebuildReactSetStateSelection(t *testing.T) FrameworkSynthesizerSelection {
	t.Helper()
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthReactSetState})
	require.NoError(t, err)
	return selection
}

func rebuildReactSetStateFixture(file string, large bool) (store_pebble.ParseFacts, []*graph.Edge) {
	const lang = "go"
	owner := rebuildEventNode(file, "Z-owner", lang, "")
	other := rebuildEventNode(file, "A-other", lang, "")
	owner.Kind, other.Kind = graph.KindType, graph.KindType
	nodes := []*graph.Node{owner, other}
	var edges, want []*graph.Edge
	method := func(id, name string, line int, owners ...*graph.Node) *graph.Node {
		n := rebuildEventNode(file, id, lang, "")
		n.Name, n.Kind, n.StartLine = name, graph.KindMethod, line
		nodes = append(nodes, n)
		for _, o := range owners {
			edges = append(edges, rebuildObserverEdge(n, o, graph.EdgeMemberOf, line))
		}
		return n
	}
	call := func(n *graph.Node, to string) {
		edges = append(edges, &graph.Edge{From: n.ID, To: to, Kind: graph.EdgeCalls,
			FilePath: file, Line: n.StartLine + 1, Origin: graph.OriginASTResolved})
	}
	winner := method("render-z", "render", 2, owner)
	method("render-a", "render", 3, owner)
	refresh := method("refresh", "refresh", 10, owner, other)
	call(refresh, "unresolved::setState")
	call(refresh, "unresolved::notsetState")
	want = append(want, rebuildReactSetStateOutput(refresh.ID, winner.ID, owner.ID, file, refresh.StartLine))
	secondary := method("secondaryOnly", "secondaryOnly", 20, other, owner)
	call(secondary, "unresolved::*.setState")
	call(winner, "unresolved::*.setState") // Self exclusion.
	keep := method("keep", "keep", 30, owner)
	call(keep, "external::setState")
	incumbent := rebuildObserverEdge(keep, winner, graph.EdgeCalls, keep.StartLine)
	incumbent.Meta = map[string]any{"fixture": "durable incumbent"}
	edges = append(edges, incumbent)
	noop := method("noop", "noop", 40, owner)
	call(noop, "unresolved::notsetState")
	if large {
		// Distinct method keys cross 1 MiB without a giant individual row.
		pad := strings.Repeat("x", 32<<10)
		for i := range 40 {
			n := method(fmt.Sprintf("large%02d%s", i, pad), "refresh", 50+i, owner)
			call(n, "unresolved::*.setState")
			want = append(want, rebuildReactSetStateOutput(n.ID, winner.ID, owner.ID, file, n.StartLine))
		}
	}
	return rebuildEventFacts(file, lang, nodes, edges...), want
}

func rebuildReactSetStateOutput(from, to, class, file string, line int) *graph.Edge {
	return &graph.Edge{
		From:            from,
		To:              to,
		Kind:            graph.EdgeCalls,
		FilePath:        file,
		Line:            line,
		Origin:          graph.OriginASTInferred,
		Confidence:      0.6,
		ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeCalls, 0.6),
		Meta: map[string]any{
			"via":             "react.setstate",
			"component_class": class,
			MetaSynthesizedBy: SynthReactSetState,
			MetaProvenance:    ProvenanceHeuristic,
		},
	}
}

func openRebuildReactSetStateStores(t *testing.T, facts ...store_pebble.ParseFacts) (*store_pebble.BuildStore, *store_sqlite.Store, []store_pebble.ParseFacts) {
	t.Helper()
	proof := make([]store_pebble.ParseFacts, 0, len(facts))
	live := slices.Clone(facts)
	native, err := store_sqlite.Open(filepath.Join(t.TempDir(), "setstate.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, native.Close()) })
	for i, f := range facts {
		raw, err := store_pebble.EncodeParseFacts(f)
		require.NoError(t, err)
		decoded, err := store_pebble.DecodeParseFacts(raw)
		require.NoError(t, err)
		proof = append(proof, decoded)
		require.NoError(t, native.AddBatchChecked(f.Nodes, f.Edges))
		live[i].Edges = slices.Clone(f.Edges)
		slices.Reverse(live[i].Edges)
	}
	slices.Reverse(live)
	b := openRebuildGRPCBuild(t, live...)
	require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
	return b, native, proof
}

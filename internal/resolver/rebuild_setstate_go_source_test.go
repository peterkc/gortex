package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
)

// Both lifecycle passes accept ordinary Go methods, without a React or Dart
// extractor. This is an actual retained-language writer, not a candidate to
// omit merely because its registry name refers to a deferred framework.
func TestRebuildSetStateGoSource(t *testing.T) {
	const file = "widget.go"
	const class = file + "::Widget"
	const caller = class + ".refresh"
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
	defer facts.ReleaseTree()
	g := graph.New()
	g.AddBatch(facts.Nodes, facts.Edges)
	for _, method := range []string{"render", "build", "setState", "refresh", "noop"} {
		id := class + "." + method
		n := g.GetNode(id)
		require.NotNil(t, n)
		require.Equal(t, graph.KindMethod, n.Kind)
		require.Equal(t, "go", n.Language)
		var owners []string
		for _, e := range g.GetOutEdges(id) {
			if e.Kind == graph.EdgeMemberOf {
				owners = append(owners, e.To)
			}
		}
		require.Equal(t, []string{class}, owners)
	}
	require.Equal(t, 6, g.GetNode(caller).StartLine)
	var setStateCalls []*graph.Edge
	for _, e := range g.GetOutEdges(caller) {
		if e.Kind == graph.EdgeCalls && isSetStateTarget(e.To) {
			setStateCalls = append(setStateCalls, e)
		}
	}
	require.Len(t, setStateCalls, 1)
	require.Equal(t, 7, setStateCalls[0].Line)
	read := func() map[graph.EdgeIdentity]*graph.Edge {
		rows := make(map[graph.EdgeIdentity]*graph.Edge)
		for _, e := range g.AllEdges() {
			key := graph.EdgeIdentityFor(e)
			require.NotContains(t, rows, key)
			rows[key] = cloneFrameworkEdge(e)
		}
		return rows
	}
	want := read()
	empty, err := NewFrameworkSynthesizerSelection([]string{})
	require.NoError(t, err)
	require.Zero(t, RunFrameworkSynthesizersWithSelection(g, empty).Total)
	require.Equal(t, want, read())
	selected, err := NewFrameworkSynthesizerSelection([]string{SynthReactSetState, SynthFlutterSetState})
	require.NoError(t, err)
	rep := RunFrameworkSynthesizersWithSelection(g, selected)
	require.NoError(t, rep.Err)
	require.False(t, rep.Incomplete)
	require.Equal(t, 2, rep.Total)
	require.Len(t, rep.Per, 2)
	require.Equal(t, SynthReactSetState, rep.Per[0].Name)
	require.Equal(t, SynthFlutterSetState, rep.Per[1].Name)
	for _, tc := range []struct{ target, via, name, ownerKey string }{
		{class + ".render", "react.setstate", SynthReactSetState, "component_class"},
		{class + ".build", "flutter.setstate", SynthFlutterSetState, "state_class"},
	} {
		e := &graph.Edge{From: caller, To: tc.target, Kind: graph.EdgeCalls, FilePath: file, Line: 6,
			Origin: graph.OriginASTInferred, Confidence: 0.6, ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeCalls, 0.6),
			Meta: map[string]any{"via": tc.via, tc.ownerKey: class, MetaSynthesizedBy: tc.name, MetaProvenance: ProvenanceHeuristic}}
		key := graph.EdgeIdentityFor(e)
		require.NotContains(t, want, key)
		want[key] = e
	}
	// All original rows, including noop's membership, survive unchanged.
	// Exactly two whole typed rows are added at the declaration site.
	require.Equal(t, want, read())
	require.NoError(t, RunFrameworkSynthesizersWithSelection(g, selected).Err)
	require.Equal(t, want, read(), "replay must not create a second row")
}

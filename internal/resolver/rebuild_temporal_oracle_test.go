package resolver

import (
	"cmp"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestRebuildTemporalSQLiteOracleExecutorField(t *testing.T) {
	const (
		file     = "wf/executor.go"
		caller   = file + "::ActivityExecutor.Run"
		setup    = file + "::setup"
		activity = file + "::ChargeCard"
	)
	for _, tc := range []struct {
		name, value, target string
		spillAt             int
	}{
		{name: "stationary placeholder with no handler", value: "ActivityName", target: "unresolved::temporal::activity::ActivityName", spillAt: 1},
		{name: "moving exact-signature fallback", value: "ChargeCard", target: activity, spillAt: 1 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := rebuildTemporalOracleStub(file, caller, "ActivityName")
			stub.Meta["temporal_name_field"] = "ActivityName"
			stub.Meta["temporal_recv_type"] = "ActivityExecutor"
			if tc.spillAt > 1 {
				stub.Meta["spill_payload"] = strings.Repeat("x", tc.spillAt+1024)
			}
			marker := &graph.Edge{
				From: setup, To: "unresolved::temporal-executor::ActivityExecutor::ActivityName",
				Kind: graph.EdgeCalls, FilePath: file, Line: 5,
				Meta: map[string]any{
					"via":            "temporal.executor-field",
					"executor_type":  "ActivityExecutor",
					"executor_field": "ActivityName",
					"executor_value": tc.value,
				},
			}
			nodes := []*graph.Node{
				{ID: caller, Name: "Run", Kind: graph.KindMethod, Language: "go", FilePath: file},
				{ID: setup, Name: "setup", Kind: graph.KindFunction, Language: "go", FilePath: file},
			}
			if tc.target == activity {
				nodes = append(nodes, &graph.Node{
					ID: activity, Name: "ChargeCard", Kind: graph.KindFunction, Language: "go", FilePath: file,
					Meta: map[string]any{"signature": "func ChargeCard(ctx context.Context) error"},
				})
			}
			facts := store_pebble.ParseFacts{
				EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: file, NodeCount: len(nodes)},
				Nodes: nodes, Edges: []*graph.Edge{stub, marker},
			}
			selection, err := NewFrameworkSynthesizerSelection([]string{SynthTemporalStub})
			require.NoError(t, err)
			native := openRebuildTemporalOracle(t, facts)
			before := native.GetOutEdges(caller)
			require.Len(t, before, 1)
			require.NoError(t, RunFrameworkSynthesizersWithSelection(native, selection).Err)
			calls := native.GetOutEdges(caller)
			require.Len(t, calls, 1)
			call := calls[0]
			require.Equal(t, tc.target, call.To)
			if tc.target == activity {
				require.Equal(t, tc.value, call.Meta["temporal_name"])
				require.Equal(t, true, call.Meta["temporal_via_executor"])
				require.Equal(t, "exact_sig", call.Meta["temporal_resolution_via"])
			} else {
				// The candidate was decoded from SQLite. With no retarget,
				// the executor pre-pass has no persisted metadata write.
				require.Equal(t, before[0], call)
				require.NotContains(t, call.Meta, "temporal_via_executor")
			}
			stats := assertRebuildTemporalOracle(t, native, selection, tc.spillAt, facts)
			require.Zero(t, stats.SpillBytes, "executor dispatches do not queue additions")
		})
	}
}

func TestRebuildTemporalSQLiteOracleJavaBeatsGoExactSignature(t *testing.T) {
	const (
		goFile      = "wf/charge.go"
		javaFile    = "java/Payment.java"
		caller      = goFile + "::Run"
		goHandler   = goFile + "::Charge"
		javaHandler = javaFile + "::Payment.pay"
	)
	goFacts := store_pebble.ParseFacts{
		EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: goFile, NodeCount: 2},
		Nodes: []*graph.Node{
			{ID: caller, Name: "Run", Kind: graph.KindFunction, Language: "go", FilePath: goFile},
			{ID: goHandler, Name: "Charge", Kind: graph.KindFunction, Language: "go", FilePath: goFile,
				Meta: map[string]any{"signature": "func Charge(ctx context.Context) error"}},
		},
		Edges: []*graph.Edge{rebuildTemporalOracleStub(goFile, caller, "Charge")},
	}
	annotation := &graph.Edge{
		From: javaHandler, To: javaActivityMethodID, Kind: graph.EdgeAnnotated, FilePath: javaFile, Line: 2,
		Meta: map[string]any{"args": `name = "Charge"`},
	}
	javaFacts := store_pebble.ParseFacts{
		EffectiveLanguage: "java", File: graph.FileMetaRow{FilePath: javaFile, NodeCount: 2},
		Nodes: []*graph.Node{
			{ID: javaHandler, Name: "pay", Kind: graph.KindMethod, Language: "java", FilePath: javaFile},
			{ID: javaActivityMethodID, Name: "ActivityMethod", Kind: graph.KindType, Language: "java", FilePath: javaFile,
				Meta: map[string]any{"kind": "annotation", "synthetic": true}},
		},
		Edges: []*graph.Edge{annotation},
	}
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthTemporalStub})
	require.NoError(t, err)
	native := openRebuildTemporalOracle(t, goFacts, javaFacts)
	require.NoError(t, RunFrameworkSynthesizersWithSelection(native, selection).Err)
	calls := native.GetOutEdges(caller)
	require.Len(t, calls, 1)
	require.Equal(t, javaHandler, calls[0].To)
	require.Equal(t, true, calls[0].Meta["temporal_cross_lang"])
	require.NotContains(t, calls[0].Meta, "temporal_resolution_via")
	provider := native.GetNode(javaHandler)
	require.NotNil(t, provider)
	require.Equal(t, "activity", provider.Meta["temporal_role"])
	require.Equal(t, "Charge", provider.Meta["temporal_name"])
	stats := assertRebuildTemporalOracle(t, native, selection, 1, goFacts, javaFacts)
	require.Zero(t, stats.SpillBytes, "cross-language rebind is a move, not an addition")

	// Suppress the Java candidate at this turn's exact refetch, not in parse
	// facts. The full census and every durable input row are still present.
	control := openRebuildTemporalOracle(t, goFacts, javaFacts)
	before := control.GetOutEdges(javaHandler)
	require.Len(t, before, 1)
	require.NoError(t, RunFrameworkSynthesizersWithSelection(rebuildTemporalWithoutJavaActivity{control}, selection).Err)
	require.Equal(t, before, control.GetOutEdges(javaHandler))
	unstamped := control.GetNode(javaHandler)
	require.NotNil(t, unstamped)
	require.NotContains(t, unstamped.Meta, "temporal_role")
	fallback := control.GetOutEdges(caller)
	require.Len(t, fallback, 1)
	require.Equal(t, goHandler, fallback[0].To)
	require.Equal(t, "exact_sig", fallback[0].Meta["temporal_resolution_via"])
	// D72 would omit the full arm's Go-to-Java row. Suppression instead
	// leaves a Go-to-Go row, which the endpoint rule would retain.
}

type rebuildTemporalWithoutJavaActivity struct{ *store_sqlite.Store }

func (s rebuildTemporalWithoutJavaActivity) FindEdgesByIdentities(keys []graph.EdgeIdentity) map[graph.EdgeIdentity]*graph.Edge {
	rows := s.Store.FindEdgesByIdentities(keys)
	for key := range rows {
		if key.Kind == graph.EdgeAnnotated && key.To == javaActivityMethodID {
			delete(rows, key)
		}
	}
	return rows
}

func rebuildTemporalOracleStub(file, caller, name string) *graph.Edge {
	return &graph.Edge{
		From: caller, To: temporalStubPlaceholder("activity", name), Kind: graph.EdgeCalls, FilePath: file, Line: 10,
		Meta: map[string]any{"via": "temporal.stub", "temporal_kind": "activity", "temporal_name": name},
	}
}

func openRebuildTemporalOracle(t *testing.T, receipts ...store_pebble.ParseFacts) *store_sqlite.Store {
	t.Helper()
	native, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, native.Close()) })
	for _, facts := range receipts {
		require.NoError(t, native.AddBatchChecked(facts.Nodes, facts.Edges))
	}
	return native
}

func assertRebuildTemporalOracle(t *testing.T, native *store_sqlite.Store, selection FrameworkSynthesizerSelection, spillAt int, facts ...store_pebble.ParseFacts) store_pebble.RebuildTemporalStats {
	t.Helper()
	private, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "working"), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, private.Close()) })
	for _, f := range facts {
		require.NoError(t, private.AddParsedFile("", f))
	}
	receipts := func(yield func(store_pebble.ParseFacts, error) bool) {
		for _, f := range facts {
			if !yield(f, nil) {
				return
			}
		}
	}
	stats, err := RunRebuildTemporal(t.Context(), private, receipts, selection, spillAt)
	require.NoError(t, err)

	view := private.RebuildView()
	var gotEdges []*graph.Edge
	require.NoError(t, view.VisitEdges(t.Context(), func(e *graph.Edge) error {
		gotEdges = append(gotEdges, e)
		return nil
	}))
	wantEdges := native.AllEdges()
	edgeOrder := func(a, b *graph.Edge) int {
		return cmp.Or(cmp.Compare(a.From, b.From), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.To, b.To),
			cmp.Compare(a.FilePath, b.FilePath), cmp.Compare(a.Line, b.Line))
	}
	slices.SortFunc(wantEdges, edgeOrder)
	slices.SortFunc(gotEdges, edgeOrder)
	require.Equal(t, wantEdges, gotEdges)

	var gotNodes []*graph.Node
	require.NoError(t, view.VisitNodes(t.Context(), func(n *graph.Node) error {
		gotNodes = append(gotNodes, n)
		return nil
	}))
	wantNodes := native.AllNodes()
	nodeOrder := func(a, b *graph.Node) int { return cmp.Compare(a.ID, b.ID) }
	slices.SortFunc(wantNodes, nodeOrder)
	slices.SortFunc(gotNodes, nodeOrder)
	require.Equal(t, wantNodes, gotNodes)
	return stats
}

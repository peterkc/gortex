package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestRebuildTemporalSQLiteOracleExecutorAmbiguity(t *testing.T) {
	const (
		file   = "billing/executor.go"
		caller = file + "::ActivityExecutor.Run"
	)
	stub := rebuildTemporalOracleStub(file, caller, "ActivityName")
	stub.Meta["temporal_name_field"] = "ActivityName"
	stub.Meta["temporal_recv_type"] = "ActivityExecutor"
	facts := []store_pebble.ParseFacts{{
		EffectiveLanguage: "go",
		File:              graph.FileMetaRow{FilePath: file, NodeCount: 1},
		Nodes:             []*graph.Node{{ID: caller, Name: "Run", Kind: graph.KindMethod, Language: "go", FilePath: file, RepoPrefix: "billing"}},
		Edges:             []*graph.Edge{stub},
	}}
	// Either literal can resolve. Conflict across repositories must leave
	// the dispatch unchanged, not pick either construction site.
	for _, site := range []struct{ repo, name string }{{"billing", "ChargeCard"}, {"returns", "RefundCard"}} {
		path := site.repo + "/setup.go"
		setup := path + "::setup"
		facts = append(facts, store_pebble.ParseFacts{
			EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: path, NodeCount: 2},
			Nodes: []*graph.Node{
				{ID: setup, Name: "setup", Kind: graph.KindFunction, Language: "go", FilePath: path, RepoPrefix: site.repo},
				{ID: path + "::" + site.name, Name: site.name, Kind: graph.KindFunction, Language: "go", FilePath: path, RepoPrefix: site.repo,
					Meta: map[string]any{"signature": "func " + site.name + "(ctx context.Context) error"}},
			},
			Edges: []*graph.Edge{{
				From: setup, To: "unresolved::temporal-executor::ActivityExecutor::ActivityName",
				Kind: graph.EdgeCalls, FilePath: path, Line: 5,
				Meta: map[string]any{
					"via":            "temporal.executor-field",
					"executor_type":  "ActivityExecutor",
					"executor_field": "ActivityName",
					"executor_value": site.name,
				},
			}},
		})
	}
	native := runRebuildTemporalReducerOracle(t, 1, facts...)
	calls := native.GetOutEdges(caller)
	require.Len(t, calls, 1)
	require.Equal(t, stub, calls[0])
}

func TestRebuildTemporalSQLiteOracleGoRegistrationMoves(t *testing.T) {
	const (
		file    = "wf/charge.go"
		caller  = file + "::Run"
		setup   = file + "::setup"
		handler = file + "::ChargeCard"
		alias   = "charge-v2"
	)
	for _, tc := range []struct {
		name    string
		moves   int
		spillAt int
	}{
		{name: "two_moves", moves: 2, spillAt: 1},
		{name: "past_5000_moves", moves: 5001, spillAt: 256 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			edges := make([]*graph.Edge, 0, tc.moves+1)
			edges = append(edges, &graph.Edge{
				From: setup, To: "unresolved::extern::go.temporal.io/sdk/worker::RegisterActivityWithOptions",
				Kind: graph.EdgeCalls, FilePath: file, Line: 5,
				Meta: map[string]any{
					"via":                      "temporal.register",
					"temporal_kind":            "activity",
					"temporal_name":            "ChargeCard",
					"temporal_registered_name": alias,
				},
			})
			// Distinct line numbers make both old and new keys unique. Every
			// stub moves, so omitting the continuation leaves a placeholder.
			for i := range tc.moves {
				stub := rebuildTemporalOracleStub(file, caller, alias)
				stub.Line += i
				edges = append(edges, stub)
			}
			facts := store_pebble.ParseFacts{
				EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: file, NodeCount: 3},
				Nodes: []*graph.Node{
					{ID: caller, Name: "Run", Kind: graph.KindFunction, Language: "go", FilePath: file},
					{ID: setup, Name: "setup", Kind: graph.KindFunction, Language: "go", FilePath: file},
					{ID: handler, Name: "ChargeCard", Kind: graph.KindFunction, Language: "go", FilePath: file},
				},
				Edges: edges,
			}
			native := runRebuildTemporalReducerOracle(t, tc.spillAt, facts)
			provider := native.GetNode(handler)
			require.NotNil(t, provider)
			require.Equal(t, "activity", provider.Meta["temporal_role"])
			require.Equal(t, alias, provider.Meta["temporal_name"])
			calls := native.GetOutEdges(caller)
			require.Len(t, calls, tc.moves)
			for _, call := range calls {
				require.Equal(t, handler, call.To, "line %d", call.Line)
				require.Equal(t, graph.OriginASTResolved, call.Origin, "line %d", call.Line)
			}
		})
	}
}

func TestRebuildTemporalSQLiteOracleJavaInterfaces(t *testing.T) {
	const (
		file       = "java/OrderAPI.java"
		iface      = file + "::OrderAPI"
		method     = file + "::submit"
		outside    = file + "::outside"
		implFile   = "java/OrderImpl.java"
		impl       = implFile + "::orders.OrderImpl"
		implMethod = impl + ".submit"
	)
	for _, tc := range []struct {
		name, annotation, args           string
		ifaceRole, methodRole, canonical string
	}{
		{
			name:       "ActivityInterface",
			annotation: javaActivityIfaceAnnoID,
			args:       `namePrefix = "Orders_"`,
			ifaceRole:  "activity_interface",
			methodRole: "activity",
			canonical:  "Orders_Submit",
		},
		{
			name:       "WorkflowInterface",
			annotation: javaWorkflowIfaceAnnoID,
			ifaceRole:  "workflow_interface",
			methodRole: "workflow",
			canonical:  "OrderAPI",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ifaceFacts := store_pebble.ParseFacts{
				EffectiveLanguage: "java", File: graph.FileMetaRow{FilePath: file, NodeCount: 4},
				Nodes: []*graph.Node{
					{ID: iface, Name: "OrderAPI", Kind: graph.KindInterface, Language: "java", FilePath: file, StartLine: 2, EndLine: 5},
					{ID: method, Name: "submit", Kind: graph.KindMethod, Language: "java", FilePath: file, StartLine: 3},
					{ID: outside, Name: "outside", Kind: graph.KindMethod, Language: "java", FilePath: file, StartLine: 8},
					{ID: tc.annotation, Name: tc.name, Kind: graph.KindType, Language: "java", FilePath: file,
						Meta: map[string]any{"kind": "annotation", "synthetic": true}},
				},
				Edges: []*graph.Edge{{
					From: iface, To: tc.annotation, Kind: graph.EdgeAnnotated, FilePath: file, Line: 1,
					Meta: map[string]any{"args": tc.args},
				}},
			}
			implFacts := store_pebble.ParseFacts{
				EffectiveLanguage: "java", File: graph.FileMetaRow{FilePath: implFile, NodeCount: 2},
				Nodes: []*graph.Node{
					{ID: impl, Name: "OrderImpl", Kind: graph.KindType, Language: "java", FilePath: implFile},
					{ID: implMethod, Name: "submit", Kind: graph.KindMethod, Language: "java", FilePath: implFile,
						Meta: map[string]any{"receiver": "orders.OrderImpl"}},
				},
				Edges: []*graph.Edge{{From: impl, To: iface, Kind: graph.EdgeImplements, FilePath: implFile, Line: 2}},
			}
			native := runRebuildTemporalReducerOracle(t, 1, ifaceFacts, implFacts)
			ifaceNode := native.GetNode(iface)
			require.NotNil(t, ifaceNode)
			require.Equal(t, tc.ifaceRole, ifaceNode.Meta["temporal_role"])
			require.Equal(t, "OrderAPI", ifaceNode.Meta["temporal_name"])
			for _, id := range []string{method, implMethod} {
				n := native.GetNode(id)
				require.NotNil(t, n)
				require.Equal(t, tc.methodRole, n.Meta["temporal_role"], id)
				require.Equal(t, tc.canonical, n.Meta["temporal_name"], id)
			}
			outsideNode := native.GetNode(outside)
			require.NotNil(t, outsideNode)
			require.NotContains(t, outsideNode.Meta, "temporal_role")
		})
	}
}

func TestRebuildTemporalSQLiteOracleRebindReset(t *testing.T) {
	const (
		file   = "wf/charge.go"
		caller = file + "::Run"
		old    = file + "::ChargeCard"
		alias  = "charge-v2"
	)
	stub := rebuildTemporalOracleStub(file, caller, alias)
	stub.Meta["temporal_local_activity"] = true
	// The old declaration is still live, but its registration is gone.
	stale := rebuildTemporalOracleStub(file, caller, alias)
	stale.To = old
	stale.Origin = graph.OriginSpeculative
	stale.Confidence = temporalCrossLangConfidence
	stale.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, stale.Confidence)
	stale.Meta["temporal_local_activity"] = true
	stale.Meta["temporal_resolution"] = stale.Origin
	stale.Meta[graph.MetaSpeculative] = true
	stale.Meta["temporal_cross_lang"] = true
	stale.Meta["temporal_const_deref"] = "ChargeCard"
	stale.Meta["temporal_resolution_via"] = "exact_sig"
	StampSynthesized(stale, SynthTemporalStub)
	facts := store_pebble.ParseFacts{
		EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: file, NodeCount: 2},
		Nodes: []*graph.Node{
			{ID: caller, Name: "Run", Kind: graph.KindFunction, Language: "go", FilePath: file},
			{ID: old, Name: "ChargeCard", Kind: graph.KindFunction, Language: "go", FilePath: file},
		},
		Edges: []*graph.Edge{stale},
	}
	native := runRebuildTemporalReducerOracle(t, 1, facts)
	calls := native.GetOutEdges(caller)
	require.Len(t, calls, 1)
	require.Equal(t, stub, calls[0])
}

func runRebuildTemporalReducerOracle(t *testing.T, spillAt int, facts ...store_pebble.ParseFacts) *store_sqlite.Store {
	t.Helper()
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthTemporalStub})
	require.NoError(t, err)
	native := openRebuildTemporalOracle(t, facts...)
	require.NoError(t, RunFrameworkSynthesizersWithSelection(native, selection).Err)
	assertRebuildTemporalOracle(t, native, selection, spillAt, facts...)
	return native
}

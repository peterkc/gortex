package resolver

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestRebuildTemporalRegistrationNodeInputCaps(t *testing.T) {
	const file = "wf/register.go"
	for _, role := range []string{"caller", "target type", "registered method", "unrelated"} {
		t.Run(role, func(t *testing.T) {
			caller := &graph.Node{ID: file + "::setup", Name: "setup", Kind: graph.KindFunction, FilePath: file, Language: "go"}
			target := &graph.Node{ID: file + "::Activities", Name: "Activities", Kind: graph.KindType, FilePath: file, Language: "go"}
			method := &graph.Node{ID: file + "::Activities.Charge", Name: "Charge", Kind: graph.KindMethod, FilePath: file, Language: "go",
				Meta: map[string]any{"temporal_role": "activity", "temporal_name": "Charge"}}
			fresh := &graph.Node{ID: file + "::Activities.Refund", Name: "Refund", Kind: graph.KindMethod, FilePath: file, Language: "go"}
			other := &graph.Node{ID: file + "::other", Name: "other", Kind: graph.KindFunction, FilePath: file, Language: "go"}
			large := map[string]*graph.Node{"caller": caller, "target type": target, "registered method": method, "unrelated": other}[role]
			if large.Meta == nil {
				large.Meta = map[string]any{}
			}
			large.Meta["padding"] = strings.Repeat("x", 16<<20)
			register := &graph.Edge{From: caller.ID, To: "unresolved::RegisterActivity", Kind: graph.EdgeCalls, FilePath: file, Line: 10,
				Meta: map[string]any{
					"via":                      "temporal.register",
					"temporal_kind":            "activity",
					"temporal_name":            target.Name,
					"temporal_register_plural": true,
				}}
			// The type is not itself an indexed method. The Charge method is
			// already stamped, so its appendRow check is not masked by a
			// preceding role-write witness. Refund proves the stage is active.
			facts := rebuildEventFacts(file, "go", []*graph.Node{caller, target, method, fresh, other}, register,
				&graph.Edge{From: method.ID, To: target.ID, Kind: graph.EdgeMemberOf, FilePath: file, Line: 2},
				&graph.Edge{From: fresh.ID, To: target.ID, Kind: graph.EdgeMemberOf, FilePath: file, Line: 3})
			native, _ := checkRebuildTemporalNodeCap(t, role != "unrelated", facts)
			if role == "unrelated" {
				stamped := native.GetNode(fresh.ID)
				require.NotNil(t, stamped)
				require.Equal(t, "activity", stamped.Meta["temporal_role"])
				require.Equal(t, fresh.Name, stamped.Meta["temporal_name"])
			}
		})
	}
}

func TestRebuildTemporalSingleRegistrationCallerCap(t *testing.T) {
	const file = "wf/register.go"
	caller := &graph.Node{ID: file + "::setup", Name: "setup", Kind: graph.KindFunction, FilePath: file, Language: "go",
		Meta: map[string]any{"padding": strings.Repeat("x", 16<<20)}}
	target := &graph.Node{ID: file + "::Charge", Name: "Charge", Kind: graph.KindFunction, FilePath: file, Language: "go"}
	register := &graph.Edge{From: caller.ID, To: "unresolved::RegisterActivity", Kind: graph.EdgeCalls, FilePath: file, Line: 10,
		Meta: map[string]any{"via": "temporal.register", "temporal_kind": "activity", "temporal_name": target.Name}}
	facts := rebuildEventFacts(file, "go", []*graph.Node{caller, target}, register)
	checkRebuildTemporalNodeCap(t, true, facts)
}

func TestRebuildTemporalJavaNodeInputCaps(t *testing.T) {
	const file, implFile = "java/OrderAPI.java", "java/OrderImpl.java"
	for _, role := range []string{"interface", "implementor", "unrelated"} {
		t.Run(role, func(t *testing.T) {
			iface := &graph.Node{ID: file + "::OrderAPI", Name: "OrderAPI", Kind: graph.KindInterface, Language: "java", FilePath: file,
				StartLine: 2, EndLine: 5, Meta: map[string]any{"temporal_role": "activity_interface", "temporal_name": "OrderAPI"}}
			method := &graph.Node{ID: file + "::submit", Name: "submit", Kind: graph.KindMethod, Language: "java", FilePath: file, StartLine: 3}
			impl := &graph.Node{ID: implFile + "::orders.OrderImpl", Name: "OrderImpl", Kind: graph.KindType, Language: "java", FilePath: implFile}
			implMethod := &graph.Node{ID: impl.ID + ".submit", Name: "submit", Kind: graph.KindMethod, Language: "java", FilePath: implFile,
				Meta: map[string]any{"receiver": "orders.OrderImpl"}}
			outside := &graph.Node{ID: file + "::outside", Name: "outside", Kind: graph.KindMethod, Language: "java", FilePath: file, StartLine: 8}
			large := map[string]*graph.Node{"interface": iface, "implementor": impl, "unrelated": outside}[role]
			if large.Meta == nil {
				large.Meta = map[string]any{}
			}
			large.Meta["padding"] = strings.Repeat("x", 16<<20)
			annotation := &graph.Node{ID: javaActivityIfaceAnnoID, Name: "ActivityInterface", Kind: graph.KindType, Language: "java", FilePath: file,
				Meta: map[string]any{"kind": "annotation", "synthetic": true}}
			ifaceFacts := rebuildEventFacts(file, "java", []*graph.Node{iface, method, outside, annotation},
				&graph.Edge{From: iface.ID, To: annotation.ID, Kind: graph.EdgeAnnotated, FilePath: file, Line: 1})
			implFacts := rebuildEventFacts(implFile, "java", []*graph.Node{impl, implMethod},
				&graph.Edge{From: impl.ID, To: iface.ID, Kind: graph.EdgeImplements, FilePath: implFile, Line: 2})
			native, _ := checkRebuildTemporalNodeCap(t, role != "unrelated", ifaceFacts, implFacts)
			if role == "unrelated" {
				for _, id := range []string{method.ID, implMethod.ID} {
					stamped := native.GetNode(id)
					require.NotNil(t, stamped)
					require.Equal(t, "activity", stamped.Meta["temporal_role"])
					require.Equal(t, "Submit", stamped.Meta["temporal_name"])
				}
			}
		})
	}
}

func TestRebuildTemporalWrapperNodeInputCaps(t *testing.T) {
	const file = "wf/wrapper.go"
	for _, role := range []string{"wrapper", "wrapper parameter", "forwarded parameter", "duplicate", "unrelated"} {
		t.Run(role, func(t *testing.T) {
			wrapper := &graph.Node{ID: file + "::Wrap", Name: "Wrap", Kind: graph.KindFunction, Language: "go", FilePath: file}
			caller := &graph.Node{ID: file + "::Caller", Name: "Caller", Kind: graph.KindFunction, Language: "go", FilePath: file}
			other := &graph.Node{ID: file + "::Other", Name: "Other", Kind: graph.KindFunction, Language: "go", FilePath: file}
			param := rebuildTemporalOracleWrapperParam(file, wrapper.ID, 0)
			forward := rebuildTemporalOracleWrapperParam(file, caller.ID, 0)
			large := map[string]*graph.Node{
				"wrapper":             wrapper,
				"wrapper parameter":   param,
				"forwarded parameter": forward,
				"duplicate":           forward,
				"unrelated":           other,
			}[role]
			if large.Meta == nil {
				large.Meta = map[string]any{}
			}
			large.Meta["padding"] = strings.Repeat("x", 16<<20)
			seed := rebuildTemporalOracleStub(file, wrapper.ID, "activityName")
			seed.Meta["temporal_name_param"] = "activityName"
			call := &graph.Edge{From: caller.ID, To: wrapper.ID, Kind: graph.EdgeCalls, FilePath: file, Line: 21,
				Meta: map[string]any{"arg_names": []string{"activityName"}}}
			// No one calls Caller, so its parameter is never a seed for a new stub.
			edges := []*graph.Edge{seed, call}
			if role == "duplicate" {
				edges = append(edges, rebuildTemporalOracleStub(file, caller.ID, "activityName"))
			}
			facts := rebuildEventFacts(file, "go", []*graph.Node{wrapper, caller, other, param, forward}, edges...)
			native, stats := checkRebuildTemporalNodeCap(t, role != "duplicate" && role != "unrelated", facts)
			switch role {
			case "duplicate":
				require.Zero(t, stats.Wrappers)
				require.False(t, native.EdgeExists(caller.ID, seed.To, graph.EdgeCalls, file, call.Line))
			case "unrelated":
				require.EqualValues(t, 1, stats.Wrappers)
				require.True(t, native.EdgeExists(caller.ID, seed.To, graph.EdgeCalls, file, call.Line))
			}
		})
	}
}

func checkRebuildTemporalNodeCap(t *testing.T, refuse bool, facts ...store_pebble.ParseFacts) (*store_sqlite.Store, store_pebble.RebuildTemporalStats) {
	t.Helper()
	b, native := openRebuildEventStores(t, facts...)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthTemporalStub})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
	require.NoError(t, err)
	require.True(t, frozen.allows(SynthTemporalStub))
	before := rebuildEventSavedRows(t, b)
	stats, err := RunRebuildTemporal(t.Context(), b, rebuildGRPCReceipts(facts...), frozen, 1)
	if refuse {
		require.ErrorIs(t, err, store_pebble.ErrRebuildTemporal)
		require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
		require.ErrorContains(t, err, "temporal node row")
		require.Zero(t, stats)
		require.Equal(t, before, rebuildEventSavedRows(t, b))
	} else {
		require.NoError(t, err)
		require.NoError(t, RunFrameworkSynthesizersWithSelection(native, selection).Err)
		require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
	}
	return native, stats
}

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

func TestRebuildTemporalSQLiteOracleMoveReceiptOrder(t *testing.T) {
	const (
		file    = "wf/order-moves.go"
		caller  = file + "::Run"
		setup   = file + "::setup"
		handler = file + "::ChargeCard"
	)
	register := func(alias string, line int) *graph.Edge {
		return &graph.Edge{From: setup, To: "unresolved::RegisterActivity", Kind: graph.EdgeCalls, FilePath: file, Line: line,
			Meta: map[string]any{
				"via":                      "temporal.register",
				"temporal_kind":            "activity",
				"temporal_name":            "ChargeCard",
				"temporal_registered_name": alias,
			}}
	}
	// The last receipt registration supplies the node role. Both stubs
	// move within one native window, where the first receipt mover wins.
	for _, reversed := range []bool{false, true} {
		t.Run(fmt.Sprintf("reversed=%t", reversed), func(t *testing.T) {
			facts := rebuildEventFacts(file, "go", []*graph.Node{
				{ID: caller, Name: "Run", Kind: graph.KindFunction, Language: "go", FilePath: file},
				{ID: setup, Name: "setup", Kind: graph.KindFunction, Language: "go", FilePath: file},
				{ID: handler, Name: "ChargeCard", Kind: graph.KindFunction, Language: "go", FilePath: file},
			}, register("ZCharge", 6), register("ACharge", 5),
				rebuildTemporalOracleStub(file, caller, "ZCharge"), rebuildTemporalOracleStub(file, caller, "ACharge"))
			winner, last := "ZCharge", "ACharge"
			if reversed {
				facts.Edges[0], facts.Edges[1] = facts.Edges[1], facts.Edges[0]
				facts.Edges[2], facts.Edges[3] = facts.Edges[3], facts.Edges[2]
				winner, last = last, winner
			}
			native := runRebuildTemporalReducerOracle(t, 1, facts)
			provider := native.GetNode(handler)
			require.NotNil(t, provider)
			require.Equal(t, last, provider.Meta["temporal_name"])
			calls := native.GetOutEdges(caller)
			require.Len(t, calls, 1)
			require.Equal(t, handler, calls[0].To)
			require.Equal(t, winner, calls[0].Meta["temporal_name"])
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

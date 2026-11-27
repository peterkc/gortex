package resolver

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildTemporalSQLiteOracleWrapperRounds(t *testing.T) {
	const file = "wf/chain.go"
	id := func(hop int) string { return fmt.Sprintf("%s::Wrap%02d", file, hop) }
	for _, tc := range []struct {
		name   string
		hops   int
		capped bool
	}{
		{name: "converged", hops: 15},
		{name: "capped", hops: 17, capped: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Each round sees only wrappers discovered before its caller scan.
			// The 15-hop arm has an empty round 16; the 17-hop arm still adds
			// on round 16 and does not propagate to hop 17.
			seed := rebuildTemporalOracleStub(file, id(0), "activityName")
			seed.Meta["temporal_name_param"] = "activityName"
			facts := store_pebble.ParseFacts{EffectiveLanguage: "go", Edges: []*graph.Edge{seed}}
			for hop := 0; hop <= tc.hops; hop++ {
				caller := id(hop)
				facts.Nodes = append(facts.Nodes,
					&graph.Node{ID: caller, Name: fmt.Sprintf("Wrap%02d", hop), Kind: graph.KindFunction, Language: "go", FilePath: file},
					rebuildTemporalOracleWrapperParam(file, caller, 0),
				)
				if hop > 0 {
					facts.Edges = append(facts.Edges, &graph.Edge{
						From: caller, To: id(hop - 1), Kind: graph.EdgeCalls, FilePath: file, Line: 20 + hop,
						Meta: map[string]any{"arg_names": []string{"activityName"}},
					})
				}
			}
			facts.File = graph.FileMetaRow{FilePath: file, NodeCount: len(facts.Nodes)}
			selection, err := NewFrameworkSynthesizerSelection([]string{SynthTemporalStub})
			require.NoError(t, err)
			native := openRebuildTemporalOracle(t, facts)
			require.NoError(t, RunFrameworkSynthesizersWithSelection(native, selection).Err)
			added := min(tc.hops, 16)
			for hop := 1; hop <= tc.hops; hop++ {
				require.Equal(t, hop <= added, native.EdgeExists(id(hop), seed.To, graph.EdgeCalls, file, 20+hop), "hop %d", hop)
			}
			stats := assertRebuildTemporalOracle(t, native, selection, 1, facts)
			require.Positive(t, stats.SpillBytes, "final wrapper slots spill at a 1-byte threshold")
			require.Equal(t, uint64(added), stats.Wrappers)
			require.Equal(t, 16, stats.WrapperRounds)
			require.Equal(t, tc.capped, stats.WrapperCapped)
		})
	}
}

func TestRebuildTemporalSQLiteOracleWrapperLookup(t *testing.T) {
	const (
		file           = "wf/lookup.go"
		byName         = file + "::ByName"
		exact          = file + "::Exact"
		fallbackCaller = file + "::FallbackCaller"
		exactCaller    = file + "::ExactCaller"
		activity       = file + "::ChargeCard"
	)
	byNameSeed := rebuildTemporalOracleStub(file, byName, "activityName")
	byNameSeed.Meta["temporal_name_param"] = "activityName"
	exactSeed := rebuildTemporalOracleStub(file, exact, "activityName")
	exactSeed.Meta["temporal_name_param"] = "activityName"
	nodes := []*graph.Node{
		{ID: byName, Name: "ByName", Kind: graph.KindFunction, Language: "go", FilePath: file},
		{ID: exact, Name: "Exact", Kind: graph.KindFunction, Language: "go", FilePath: file},
		{ID: fallbackCaller, Name: "FallbackCaller", Kind: graph.KindFunction, Language: "go", FilePath: file},
		{ID: exactCaller, Name: "ExactCaller", Kind: graph.KindFunction, Language: "go", FilePath: file},
		{ID: activity, Name: "ChargeCard", Kind: graph.KindFunction, Language: "go", FilePath: file,
			Meta: map[string]any{"signature": "func ChargeCard(ctx context.Context) error"}},
		rebuildTemporalOracleWrapperParam(file, byName, 0),
		rebuildTemporalOracleWrapperParam(file, exact, 1),
	}
	facts := store_pebble.ParseFacts{
		EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: file, NodeCount: len(nodes)}, Nodes: nodes,
		Edges: []*graph.Edge{
			byNameSeed, exactSeed,
			{From: fallbackCaller, To: "unresolved::ByName", Kind: graph.EdgeCalls, FilePath: file, Line: 20,
				Meta: map[string]any{"callee": "ByName", "arg_names": []string{"ChargeCard"}}},
			// Exact has no argument at position 1. The name fallback would
			// succeed at position 0 if the exact entry did not suppress it.
			{From: exactCaller, To: exact, Kind: graph.EdgeCalls, FilePath: file, Line: 21,
				Meta: map[string]any{"callee": "ByName", "arg_names": []string{"ChargeCard"}}},
			// A distinct argument keeps the existing seed from masking a
			// missing self-call guard via stub deduplication.
			{From: byName, To: byName, Kind: graph.EdgeCalls, FilePath: file, Line: 22,
				Meta: map[string]any{"callee": "ByName", "arg_names": []string{"ChargeCard"}}},
		},
	}
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthTemporalStub})
	require.NoError(t, err)
	native := openRebuildTemporalOracle(t, facts)
	beforeExact := native.GetOutEdges(exactCaller)
	beforeSelf := native.GetOutEdges(byName)
	require.Len(t, beforeExact, 1)
	require.Len(t, beforeSelf, 2)
	require.NoError(t, RunFrameworkSynthesizersWithSelection(native, selection).Err)
	require.True(t, native.EdgeExists(fallbackCaller, activity, graph.EdgeCalls, file, 20))
	require.Equal(t, beforeExact, native.GetOutEdges(exactCaller))
	require.ElementsMatch(t, beforeSelf, native.GetOutEdges(byName))
	stats := assertRebuildTemporalOracle(t, native, selection, 4<<10, facts)
	require.Zero(t, stats.SpillBytes, "the final add tail does not count as spill")
}

func TestRebuildTemporalSQLiteOracleWrapperReceiptOrder(t *testing.T) {
	const (
		file   = "wf/order.go"
		wrap   = file + "::Wrap"
		caller = file + "::Caller"
	)
	first := rebuildTemporalOracleStub(file, wrap, "ZSeed")
	first.Meta["temporal_name_param"] = "activityName"
	other := rebuildTemporalOracleStub(file, wrap, "ASeed")
	other.Meta["temporal_name_param"] = "otherName"
	param := rebuildTemporalOracleWrapperParam(file, wrap, 0)
	param.ID, param.Name = wrap+"#param:otherName", "otherName"
	nodes := []*graph.Node{
		{ID: wrap, Name: "Wrap", Kind: graph.KindFunction, Language: "go", FilePath: file},
		{ID: caller, Name: "Caller", Kind: graph.KindFunction, Language: "go", FilePath: file},
		rebuildTemporalOracleWrapperParam(file, wrap, 1),
		param,
	}
	// The first receipt seed reads argument 1. The first receipt call
	// also has the greater old key. Each choice changes the output key.
	firstCall := &graph.Edge{From: caller, To: wrap, Kind: graph.EdgeCalls, FilePath: file, Line: 90,
		Meta: map[string]any{"arg_names": []string{"Loser", "Winner"}}}
	otherCall := *firstCall
	otherCall.Line = 50
	facts := rebuildEventFacts(file, "go", nodes, first, other, firstCall, &otherCall)
	native := runRebuildTemporalReducerOracle(t, 1, facts)
	calls := native.GetOutEdges(caller)
	require.Len(t, calls, 3) // Two ordinary calls and one wrapper stub.
	require.True(t, native.EdgeExists(caller, temporalStubPlaceholder("activity", "Winner"), graph.EdgeCalls, file, 90))
}

func TestRebuildTemporalSQLiteOracleWrapperSlotOverwrite(t *testing.T) {
	const (
		file   = "wf/slot.go"
		z      = file + "::ZWrap"
		a      = file + "::AWrap"
		caller = file + "::Caller"
	)
	// Two different kind/name pairs produce the same placeholder key.
	// The second receipt for the caller overwrites the first staged slot.
	zSeed := rebuildTemporalOracleStub(file, z, "activityName")
	zSeed.Meta["temporal_name_param"] = "activityName"
	zSeed.Meta["temporal_kind"] = "activity::scope"
	zSeed.To = temporalStubPlaceholder("activity::scope", "activityName")
	aSeed := rebuildTemporalOracleStub(file, a, "activityName")
	aSeed.Meta["temporal_name_param"] = "activityName"
	nodes := []*graph.Node{
		{ID: z, Name: "ZWrap", Kind: graph.KindFunction, Language: "go", FilePath: file},
		{ID: a, Name: "AWrap", Kind: graph.KindFunction, Language: "go", FilePath: file},
		{ID: caller, Name: "Caller", Kind: graph.KindFunction, Language: "go", FilePath: file},
		rebuildTemporalOracleWrapperParam(file, z, 0),
		rebuildTemporalOracleWrapperParam(file, a, 0),
	}
	zCall := &graph.Edge{From: caller, To: z, Kind: graph.EdgeCalls, FilePath: file, Line: 20,
		Meta: map[string]any{"arg_names": []string{"Choice"}}}
	aCall := &graph.Edge{From: caller, To: a, Kind: graph.EdgeCalls, FilePath: file, Line: 20,
		Meta: map[string]any{"arg_names": []string{"scope::Choice"}}}
	facts := rebuildEventFacts(file, "go", nodes, zSeed, aSeed, zCall, aCall)
	native := runRebuildTemporalReducerOracle(t, 1, facts)
	calls := native.GetOutEdges(caller)
	require.Len(t, calls, 3)
	for _, e := range calls {
		if e.To == temporalStubPlaceholder("activity", "scope::Choice") {
			require.Equal(t, "AWrap", e.Meta["temporal_via_wrapper"])
			return
		}
	}
	t.Fatal("missing shared wrapper slot")
}

func TestRebuildTemporalSQLiteOracleWrapperShadowsBase(t *testing.T) {
	const (
		file   = "wf/shadow.go"
		wrap   = file + "::Wrap"
		caller = file + "::Caller"
	)
	shadow := temporalStubPlaceholder("activity", "Pending")
	seed := rebuildTemporalOracleStub(file, wrap, "activityName")
	seed.Meta["temporal_name_param"] = "activityName"
	nodes := []*graph.Node{
		{ID: wrap, Name: "Wrap", Kind: graph.KindFunction, Language: "go", FilePath: file},
		{ID: caller, Name: "Caller", Kind: graph.KindFunction, Language: "go", FilePath: file},
		{ID: shadow, Name: "Transit", Kind: graph.KindFunction, Language: "go", FilePath: file},
		rebuildTemporalOracleWrapperParam(file, wrap, 0),
		rebuildTemporalOracleWrapperParam(file, shadow, 0),
	}
	call := func(from, to, arg string) *graph.Edge {
		return &graph.Edge{From: from, To: to, Kind: graph.EdgeCalls, FilePath: file, Line: 20,
			Meta: map[string]any{"arg_names": []string{arg}}}
	}
	// Sweep one installs a staged stub over the call to shadow and turns
	// shadow into a wrapper. Sweep two must not read the hidden args.
	baseCall := call(caller, shadow, "Unexpected")
	facts := rebuildEventFacts(file, "go", nodes, seed, baseCall,
		call(caller, wrap, "Pending"), call(shadow, wrap, "activityName"))
	native := runRebuildTemporalReducerOracle(t, 1, facts)
	require.True(t, native.EdgeExists(shadow, seed.To, graph.EdgeCalls, file, 20))
	require.False(t, native.EdgeExists(caller, temporalStubPlaceholder("activity", "Unexpected"), graph.EdgeCalls, file, 20))
}

func rebuildTemporalOracleWrapperParam(file, caller string, position int) *graph.Node {
	return &graph.Node{
		ID:       caller + "#param:activityName",
		Name:     "activityName",
		Kind:     graph.KindParam,
		Language: "go",
		FilePath: file,
		Meta:     map[string]any{"position": position},
	}
}

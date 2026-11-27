package store_pebble

import (
	"slices"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildReactSetStateRejectsWrongRecordingPathBeforeOwner(t *testing.T) {
	for _, tc := range []struct {
		name  string
		decoy bool
	}{
		{name: "first valid membership wins"},
		{name: "same key at wrong path before the competitor", decoy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caller := setStateProofMethod("refresh", "refresh", 5)
			first := setStateProofMethod("render-z", "render", 10)
			second := setStateProofMethod("render-a", "render", 15)
			firstOwner := setStateProofMember(caller, "ZWidget")
			secondOwner := setStateProofMember(caller, "AWidget")
			facts := setStateProofFacts([]*graph.Node{caller, first, second},
				firstOwner, secondOwner,
				setStateProofMember(first, "ZWidget"), setStateProofMember(second, "AWidget"),
				setStateProofCall(caller, "unresolved::setState"),
			)
			b, replay := seedSyntheticCustody(t, facts)
			wrong := ParseFacts{EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: "wrong.go"}}
			if tc.decoy {
				// The first sighting repeats the exact edge key, not the recording path.
				// Ignoring it would put ZWidget ahead of AWidget on replay.
				wrong.Edges = []*graph.Edge{secondOwner}
			}
			receipts := func(yield func(ParseFacts, error) bool) {
				if yield(wrong, nil) {
					replay(yield)
				}
			}
			before := rebuildFrameworkCustodyRows(t, b)
			stats, err := b.RebuildReactSetState(t.Context(), receipts, 1<<20, setStateProofTarget, setStateProofOutput)
			if tc.decoy {
				require.ErrorIs(t, err, ErrRebuildReactSetState)
				require.ErrorIs(t, err, ErrRebuildUnknownProvenance)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 1, stats.Edges)
			checkSetStateProofOutput(t, b, setStateProofOutput(caller, first, "ZWidget"), rebuildProofReactSetState)
		})
	}
}

func TestRebuildReactSetStateSkipsReceiptsWithoutLiveGate(t *testing.T) {
	caller := setStateProofMethod("refresh", "refresh", 5)
	render := setStateProofMethod("render", "render", 10)
	b, _ := seedSyntheticCustody(t, setStateProofFacts([]*graph.Node{caller, render},
		setStateProofMember(caller, "Widget"), setStateProofMember(render, "Widget"), setStateProofCall(caller, "helper")))
	before := rebuildFrameworkCustodyRows(t, b)
	receipts := func(func(ParseFacts, error) bool) { t.Fatal("replayed receipts without a live setState call") }
	stats, err := b.RebuildReactSetState(t.Context(), receipts, 1<<20, setStateProofTarget, setStateProofOutput)
	require.NoError(t, err)
	require.Zero(t, stats.Edges)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
}

func TestRebuildReactSetStateProvesOnlyGatedMemberships(t *testing.T) {
	const chosen = "ZWidget"
	caller := setStateProofMethod("refresh", "refresh", 5)
	render := setStateProofMethod("render", "render", 10)
	unrelated := setStateProofMethod("unrelated-render", "render", 15)
	callerFirst, callerOther := setStateProofMember(caller, chosen), setStateProofMember(caller, "ACallerOnly")
	targetFirst, targetOther := setStateProofMember(render, chosen), setStateProofMember(render, "ATargetOnly")
	unrelatedMember := setStateProofMember(unrelated, "OtherWidget")
	// The receipt ordinal elects ZWidget for both methods despite the alternatives sorting first.
	facts := setStateProofFacts([]*graph.Node{caller, render, unrelated},
		callerFirst, callerOther, targetFirst, targetOther,
		unrelatedMember, setStateProofCall(caller, "unresolved::setState"),
	)
	for _, tc := range []struct {
		name    string
		missing *graph.Edge
	}{
		{name: "unrelated lifecycle membership only"},
		{name: "caller first owner", missing: callerFirst},
		{name: "caller alternate owner", missing: callerOther},
		{name: "target first owner", missing: targetFirst},
		{name: "target alternate owner", missing: targetOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := seedSyntheticCustody(t, facts)
			recorded := facts
			recorded.Edges = slices.DeleteFunc(slices.Clone(facts.Edges), func(e *graph.Edge) bool {
				return e == unrelatedMember || e == tc.missing
			})
			receipts := func(yield func(ParseFacts, error) bool) { yield(recorded, nil) }
			before := rebuildFrameworkCustodyRows(t, b)
			stats, err := b.RebuildReactSetState(t.Context(), receipts, 1<<20, setStateProofTarget, setStateProofOutput)
			if tc.missing != nil {
				require.ErrorIs(t, err, ErrRebuildReactSetState)
				require.ErrorIs(t, err, ErrRebuildUnknownProvenance)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 1, stats.Edges)
			checkSetStateProofOutput(t, b, setStateProofOutput(caller, render, chosen), rebuildProofReactSetState)
		})
	}
}

func TestRebuildReactSetStateUsesAnotherGateWhenProducerDigestIsStale(t *testing.T) {
	const owner = "Widget"
	for _, tc := range []struct {
		name     string
		stale    bool
		fallback bool
	}{
		{name: "checked earlier producer alone"},
		{name: "stale earlier producer and proved raw gate", stale: true, fallback: true},
		{name: "stale earlier producer alone", stale: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caller := setStateProofMethod("refresh", "refresh", 5)
			render := setStateProofMethod("render", "render", 10)
			facts := setStateProofFacts([]*graph.Node{caller, render}, setStateProofMember(caller, owner), setStateProofMember(render, owner))
			if tc.fallback {
				facts.Edges = append(facts.Edges, setStateProofCall(caller, "unresolved::setState"))
			}
			b, receipts := seedSyntheticCustody(t, facts)
			gate := setStateProofCall(caller, "earlier.setState")
			custody := rebuildEdgeCustody{proof: rebuildProofClosureCollection}
			require.NoError(t, b.withWrites(func() error { return b.putEdgeWithCustody(gate, custody) }))
			key := pendingEdgeKey(gate)
			valid, err := readRebuildEdgeCustody(b.pending, key)
			require.NoError(t, err)
			require.Equal(t, custody, valid)
			if tc.stale {
				// Keep the key, reverse row and producer stamp. Change only the live bytes.
				changed := *gate
				changed.Via = "changed after proof"
				raw, err := encodeEdge(&changed)
				require.NoError(t, err)
				require.NoError(t, b.pending.Set(key, raw, pebble.NoSync))
			}
			before := rebuildFrameworkCustodyRows(t, b)
			stats, err := b.RebuildReactSetState(t.Context(), receipts, 1<<20, setStateProofTarget, setStateProofOutput)
			if tc.stale && !tc.fallback {
				require.ErrorIs(t, err, ErrRebuildReactSetState)
				require.ErrorIs(t, err, ErrRebuildUnknownProvenance)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 1, stats.Edges)
			checkSetStateProofOutput(t, b, setStateProofOutput(caller, render, owner), rebuildProofReactSetState)
			after := rebuildFrameworkCustodyRows(t, b)
			require.Equal(t, before[string(key)], after[string(key)])
			recorder := string(rebuildLinkRecorderKey(key))
			require.Equal(t, before[recorder], after[recorder])
		})
	}
}

func TestRebuildSetStateRejectsOwnCustodyAsGate(t *testing.T) {
	for _, tc := range []struct {
		name, lifecycle string
		proof           rebuildEdgeProof
		stage           error
	}{
		{"React", "render", rebuildProofReactSetState, ErrRebuildReactSetState},
		{"Flutter", "build", rebuildProofFlutterSetState, ErrRebuildFlutterSetState},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const owner = "Widget"
			caller := setStateProofMethod("refresh", "refresh", 5)
			target := setStateProofMethod(tc.lifecycle, tc.lifecycle, 10)
			b, receipts := seedSyntheticCustody(t, setStateProofFacts([]*graph.Node{caller, target},
				setStateProofMember(caller, owner), setStateProofMember(target, owner)))
			gate := setStateProofCall(caller, "widget::setState")
			custody := rebuildEdgeCustody{proof: tc.proof}
			require.NoError(t, b.withWrites(func() error { return b.putEdgeWithCustody(gate, custody) }))
			valid, err := readRebuildEdgeCustody(b.pending, pendingEdgeKey(gate))
			require.NoError(t, err)
			require.Equal(t, custody, valid)
			before := rebuildFrameworkCustodyRows(t, b)
			run := b.RebuildReactSetState
			if tc.proof == rebuildProofFlutterSetState {
				run = b.RebuildFlutterSetState
			}
			stats, err := run(t.Context(), receipts, 1, setStateProofTarget, setStateProofOutput)
			require.ErrorIs(t, err, tc.stage)
			require.ErrorIs(t, err, ErrRebuildUnknownProvenance)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

const setStateProofFile = "widget.go"

func setStateProofMethod(id, name string, line int) *graph.Node {
	return &graph.Node{ID: id, Name: name, Kind: graph.KindMethod, FilePath: setStateProofFile, Language: "go", StartLine: line}
}

func setStateProofFacts(nodes []*graph.Node, edges ...*graph.Edge) ParseFacts {
	return ParseFacts{
		EffectiveLanguage: "go",
		File:              graph.FileMetaRow{FilePath: setStateProofFile},
		Nodes:             nodes,
		Edges:             edges,
	}
}

func setStateProofMember(from *graph.Node, owner string) *graph.Edge {
	return &graph.Edge{From: from.ID, To: owner, Kind: graph.EdgeMemberOf, FilePath: from.FilePath, Line: from.StartLine}
}

func setStateProofCall(from *graph.Node, to string) *graph.Edge {
	return &graph.Edge{From: from.ID, To: to, Kind: graph.EdgeCalls, FilePath: from.FilePath, Line: from.StartLine + 1}
}

func setStateProofTarget(to string) bool {
	return strings.HasSuffix(to, ".setState") || strings.HasSuffix(to, "::setState")
}

func setStateProofOutput(from, target *graph.Node, owner string) *graph.Edge {
	return &graph.Edge{
		From:     from.ID,
		To:       target.ID,
		Kind:     graph.EdgeCalls,
		FilePath: from.FilePath,
		Line:     from.StartLine,
		Origin:   graph.OriginASTInferred,
		Meta:     map[string]any{"component_class": owner},
	}
}

func checkSetStateProofOutput(t *testing.T, b *BuildStore, want *graph.Edge, proof rebuildEdgeProof) {
	t.Helper()
	key := pendingEdgeKey(want)
	live, err := b.pendingEdge(key)
	require.NoError(t, err)
	require.Equal(t, want, live)
	custody, err := readRebuildEdgeCustody(b.pending, key)
	require.NoError(t, err)
	require.Equal(t, rebuildEdgeCustody{proof: proof}, custody)
}

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
				var refusal *RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refusal)
				require.Equal(t, &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(secondOwner)}, refusal)
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
			recorder := rebuildLinkRecorderKey(key)
			require.Equal(t, before[string(recorder)], after[string(recorder)])
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

func TestRebuildSetStateMovesAndOutputRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, lifecycle string
		proof           rebuildEdgeProof
	}{
		{"React", "render", rebuildProofReactSetState},
		{"Flutter", "build", rebuildProofFlutterSetState},
	} {
		for _, mode := range []string{"checked moves", "untracked reused destination", "target receipt changed", "wrong gate recording", "non-method caller", "non-method lifecycle"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				ctx := t.Context()
				const owner = "Widget"
				first := setStateProofMethod("a-caller", "refresh", 5)
				second := setStateProofMethod("z-caller", "refresh", 10)
				target := setStateProofMethod("lifecycle", tc.lifecycle, 15)
				if mode == "non-method caller" {
					first.Kind, second.Kind = graph.KindFunction, graph.KindFunction
				}
				if mode == "non-method lifecycle" {
					target.Kind = graph.KindFunction
				}
				membership := setStateProofMember(target, "unresolved::Widget")
				rawFirst := setStateProofCall(first, "unresolved::resolveFirst")
				rawSecond := setStateProofCall(second, "unresolved::resolveSecond")
				facts := setStateProofFacts([]*graph.Node{first, second, target},
					setStateProofMember(first, owner), setStateProofMember(second, owner), membership, rawFirst, rawSecond)
				decoy := setStateProofCall(first, "Widget.setState")
				if mode == "untracked reused destination" {
					facts.Edges = append(facts.Edges, decoy)
				}
				b, _ := seedSyntheticCustody(t, facts)
				moveTarget := func(raw *graph.Edge, dest string, checked bool) {
					t.Helper()
					live := *raw
					live.To = dest
					moves := []graph.EdgeReindex{{Edge: &live, OldTo: raw.To}}
					if checked {
						require.NoError(t, b.ApplyLinkReindexes(ctx, moves))
					} else {
						b.ReindexEdges(moves)
					}
					custody, err := readRebuildEdgeCustody(b.pending, pendingEdgeKey(&live))
					require.NoError(t, err)
					if checked {
						require.Equal(t, pendingEdgeKey(raw), custody.raw)
					} else {
						require.Equal(t, rebuildProofUntrackedMove, custody.proof)
					}
				}
				moveTarget(membership, owner, true)
				if mode == "untracked reused destination" {
					moveTarget(decoy, "Widget.helper", true)
				}
				moveTarget(rawFirst, decoy.To, mode != "untracked reused destination")
				moveTarget(rawSecond, decoy.To, true)
				if mode == "target receipt changed" {
					changed := *target
					changed.RepoPrefix = "other"
					facts.Nodes[2] = &changed
				}
				receipts := func(yield func(ParseFacts, error) bool) {
					require.NotContains(t, mode, "non-method")
					if mode == "wrong gate recording" {
						wrong := facts
						wrong.File.FilePath = "wrong.go"
						wrong.Nodes = nil
						wrong.Edges = []*graph.Edge{rawFirst, rawSecond}
						if !yield(wrong, nil) {
							return
						}
						facts.Edges = facts.Edges[:3]
					}
					yield(facts, nil)
				}
				before := rebuildFrameworkCustodyRows(t, b)
				run := b.RebuildReactSetState
				if tc.proof == rebuildProofFlutterSetState {
					run = b.RebuildFlutterSetState
				}
				stats, err := run(ctx, receipts, 1, setStateProofTarget, setStateProofOutput)
				firstOut := setStateProofOutput(first, target, owner)
				switch mode {
				case "checked moves":
					require.NoError(t, err)
					require.EqualValues(t, 2, stats.Edges)
					checkSetStateProofOutput(t, b, firstOut, tc.proof)
					checkSetStateProofOutput(t, b, setStateProofOutput(second, target, owner), tc.proof)
					return
				case "non-method caller", "non-method lifecycle":
					// The census can write scratch rows without emitting output.
					require.NoError(t, err)
					require.Zero(t, stats.Edges)
					require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
					return
				default:
					var refusal *RebuildUnknownProvenanceError
					require.ErrorAs(t, err, &refusal)
					count := uint64(2)
					if mode == "untracked reused destination" {
						count = 1
					}
					require.Equal(t, &RebuildUnknownProvenanceError{Count: count, First: graph.EdgeIdentityFor(firstOut)}, refusal)
				}
				require.Zero(t, stats)
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
			})
		}
	}
}

func TestRebuildSetStateRefusesReceiptKeyCollisions(t *testing.T) {
	for _, mode := range []string{"membership with stage custody", "membership key reused", "call key reused"} {
		t.Run(mode, func(t *testing.T) {
			caller := setStateProofMethod("a", "refresh", 5)
			other := setStateProofMethod("z", "refresh", 10)
			target := setStateProofMethod("render", "render", 15)
			member := setStateProofMember(caller, "Widget")
			otherMember := setStateProofMember(other, "Widget")
			call := setStateProofCall(caller, "Widget.setState")
			otherCall := setStateProofCall(other, "Widget.setState")
			facts := setStateProofFacts([]*graph.Node{caller, other, target}, member, otherMember,
				setStateProofMember(target, "Widget"), call, otherCall)
			b, _ := seedSyntheticCustody(t, facts)
			bad := otherMember
			custody := rebuildEdgeCustody{raw: pendingEdgeKey(member)}
			switch mode {
			case "membership with stage custody":
				custody = rebuildEdgeCustody{proof: rebuildProofClosureCollection}
			case "call key reused":
				bad = otherCall
				custody.raw = pendingEdgeKey(call)
			}
			require.NoError(t, b.withWrites(func() error { return b.putEdgeWithCustody(bad, custody) }))
			before := rebuildFrameworkCustodyRows(t, b)
			stats, err := b.RebuildReactSetState(t.Context(), func(func(ParseFacts, error) bool) {
				t.Fatal("replayed receipts after a conflicting input key")
			}, 1, setStateProofTarget, setStateProofOutput)
			var refusal *RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refusal)
			require.Equal(t, &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(bad)}, refusal)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildReactSetStateInvalidInputWording(t *testing.T) {
	b := newBuild(t, FullProfile)
	stats, err := b.RebuildReactSetState(t.Context(), nil, 1, setStateProofTarget, setStateProofOutput)
	require.ErrorIs(t, err, ErrRebuildReactSetState)
	require.ErrorIs(t, err, ErrRebuildLinkInputSize)
	require.ErrorContains(t, err, "invalid React setState inputs")
	require.Zero(t, stats)
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

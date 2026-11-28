package store_pebble

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildLanguageMarkerSizeOrder(t *testing.T) {
	for _, tc := range []struct {
		name    string
		largeID string
		bounded bool
	}{
		{name: "bounded first", largeID: "z", bounded: true},
		{name: "oversized first", largeID: "a", bounded: true},
		{name: "only oversized", largeID: "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			prefix := []byte("gx")
			small := &graph.Node{ID: "m", Language: "kotlin"}
			raw, err := encodeNode(small)
			require.NoError(t, err)
			limit := len(pendingNodeKey(small.ID)) + len(raw)
			large := &graph.Node{ID: tc.largeID, Language: "kotlin", Meta: map[string]any{"padding": "wide"}}
			b.AddNode(large)
			require.NoError(t, b.putRebuildScratch(keyStrings(prefix, large.ID), []byte{rebuildScratchExact}, limit, "example"))
			if tc.bounded {
				b.AddNode(small)
				require.NoError(t, b.putRebuildScratch(keyStrings(prefix, small.ID), []byte{rebuildScratchExact}, limit, "example"))
			}
			proved, err := b.proveRebuildLanguageMarker(t.Context(), prefix, limit, "example")
			if !tc.bounded {
				require.ErrorIs(t, err, ErrRebuildLinkInputSize)
				require.ErrorContains(t, err, "example marker node")
				require.False(t, proved)
				return
			}
			require.NoError(t, err)
			require.True(t, proved)
		})
	}
}

func TestRebuildScratchRowLimit(t *testing.T) {
	b := newBuild(t, FullProfile)
	const limit = 8
	key := []byte("gxa")
	value := []byte("12345")
	before := b.pending.Metrics().Levels[0].TableBytesFlushed
	require.NoError(t, b.putRebuildScratch(key, value, limit, "example"))

	// An oversized replacement must not change the exact-limit row.
	err := b.putRebuildScratch(key, []byte("123456"), limit, "example")
	require.ErrorIs(t, err, ErrRebuildLinkInputSize)
	require.ErrorContains(t, err, "example scratch row")
	got, err := pendingGet(b.pending, key, copyValue)
	require.NoError(t, err)
	require.Equal(t, value, got)

	// A later prefix reader sees immediate writes without an explicit flush.
	replacement := []byte("new")
	require.NoError(t, b.putRebuildScratch(key, replacement, limit, "example"))
	var rows int
	require.NoError(t, pendingVisit(t.Context(), b.pending, []byte("gx"), func(r row) error {
		rows++
		require.Equal(t, key, r.key)
		require.Equal(t, replacement, r.value)
		return nil
	}))
	require.Equal(t, 1, rows)
	require.Equal(t, before, b.pending.Metrics().Levels[0].TableBytesFlushed)
}

func TestRebuildScratchRefusalsKeepLowestCompleteKey(t *testing.T) {
	var refused rebuildScratchRefusals
	require.NoError(t, refused.err())
	for _, line := range []int{20, 30, 10} {
		e := &graph.Edge{From: "caller", To: "target", Kind: graph.EdgeCalls, FilePath: "call.go", Line: line}
		refused.add(graph.EdgeIdentityFor(e), pendingEdgeKey(e))
	}
	var got *RebuildUnknownProvenanceError
	require.ErrorAs(t, refused.err(), &got)
	require.Equal(t, &RebuildUnknownProvenanceError{
		Count: 3,
		First: graph.EdgeIdentity{From: "caller", To: "target", Kind: graph.EdgeCalls, FilePath: "call.go", Line: 10},
	}, got)
}

func TestRebuildFactoryChainGuards(t *testing.T) {
	const file = "worker.go"
	for _, mode := range []string{"nil inputs", "occupied freeze", "missing freeze", "no frozen candidates", "frozen candidate removed", "candidate edge cap"} {
		t.Run(mode, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			ctx := t.Context()
			rules := RebuildFactoryChainRules{
				Call: func(*graph.Edge) (string, string) { return "New()", "Run" },
				Target: func(RebuildFactoryChainLookup, string, string, string) (string, bool) {
					t.Error("guard allowed target election")
					return "", false
				},
				Rebind: func(*graph.Edge, string, bool) graph.EdgeReindex { return graph.EdgeReindex{} },
			}
			if mode == "nil inputs" {
				count, err := b.RebuildFactoryChain(ctx, nil, rules)
				require.ErrorIs(t, err, ErrRebuildFactoryChain)
				require.ErrorContains(t, err, "factory chain inputs")
				require.Zero(t, count)
				return
			}
			match := func(graph.FrameworkCensusEdge) bool { return true }
			if mode == "occupied freeze" {
				require.NoError(t, b.FreezeFactoryChainInputs(ctx, match))
				before := rebuildFrameworkCustodyRows(t, b)
				require.ErrorIs(t, b.FreezeFactoryChainInputs(ctx, match), ErrRebuildFactoryChain)
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
				return
			}
			call := &graph.Edge{From: "caller", To: "unresolved::*.Run", Kind: graph.EdgeCalls, FilePath: file, Line: 1}
			if mode == "candidate edge cap" {
				call.Context = strings.Repeat("x", 16<<20)
				b.AddEdge(call)
			}
			before := rebuildFrameworkCustodyRows(t, b)
			if mode == "frozen candidate removed" {
				b.AddEdge(call)
			}
			if mode != "missing freeze" {
				require.NoError(t, b.FreezeFactoryChainInputs(ctx, match))
			}
			if mode == "frozen candidate removed" {
				require.True(t, b.RemoveEdge(call.From, call.To, call.Kind))
			}
			receipts := func(func(ParseFacts, error) bool) { t.Error("guard opened receipts") }
			count, err := b.RebuildFactoryChain(ctx, receipts, rules)
			switch mode {
			case "candidate edge cap":
				require.ErrorIs(t, err, ErrRebuildLinkInputSize)
				require.ErrorContains(t, err, "factory chain input row")
			case "missing freeze":
				require.ErrorIs(t, err, ErrRebuildFactoryChain)
				require.ErrorContains(t, err, "factory chain census missing")
			default:
				require.NoError(t, err)
			}
			require.Zero(t, count)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildFactoryChainPlanReadError(t *testing.T) {
	b := newBuild(t, FullProfile)
	call := &graph.Edge{From: "caller", To: "unresolved::*.Run", Kind: graph.EdgeCalls, FilePath: "worker.go", Line: 1}
	b.AddEdge(call)
	p := rebuildFactoryChain{rebuildCallMoves: rebuildFactoryChainColumns(b)}
	key := pendingEdgeKey(call)
	require.NoError(t, p.put(append(p.column('h'), key...), nil))
	p.rowBytes = len(key) // The identity fits, but the stored payload does not.
	count, err := p.plan(t.Context())
	require.Zero(t, count)
	require.ErrorIs(t, err, ErrRebuildLinkInputSize)
	require.NotContains(t, err.Error(), "live edge missing")
}

func TestRebuildFactoryChainSwiftObjCArcProof(t *testing.T) {
	const file = "bridge.swift"
	for _, proof := range []rebuildEdgeProof{rebuildProofSwiftObjC, rebuildProofUntrackedMove} {
		t.Run(fmt.Sprint(proof), func(t *testing.T) {
			arc := &graph.Edge{From: "concrete", To: "protocol", Kind: graph.EdgeImplements, FilePath: file, Line: 1}
			call := &graph.Edge{From: "caller", To: "unresolved::*.Run", Kind: graph.EdgeCalls, FilePath: file, Line: 2}
			facts := ParseFacts{EffectiveLanguage: "swift", File: graph.FileMetaRow{FilePath: file}, Edges: []*graph.Edge{call}}
			b, receipts := seedSyntheticCustody(t, facts)
			require.NoError(t, b.withWrites(func() error {
				return b.addEdgeWithCustody(arc, rebuildEdgeCustody{proof: proof})
			}))
			before := rebuildFrameworkCustodyRows(t, b)
			require.NoError(t, b.FreezeFactoryChainInputs(t.Context(), func(graph.FrameworkCensusEdge) bool { return true }))
			count, err := b.RebuildFactoryChain(t.Context(), receipts, RebuildFactoryChainRules{
				Call: func(*graph.Edge) (string, string) { return "factory()", "Run" },
				Target: func(lookup RebuildFactoryChainLookup, _, _, _ string) (string, bool) {
					if len(lookup.Incoming("protocol")) == 1 {
						return "method", true
					}
					return "", false
				},
				Rebind: func(e *graph.Edge, target string, _ bool) graph.EdgeReindex {
					old := e.To
					e.To = target
					return graph.EdgeReindex{Edge: e, OldTo: old}
				},
			})
			if proof == rebuildProofSwiftObjC {
				require.NoError(t, err)
				require.EqualValues(t, 1, count)
				want := *call
				want.To = "method"
				requireRebuildLinkCustody(t, b, &want, rebuildEdgeCustody{raw: pendingEdgeKey(call)})
				return
			}
			var refusal *RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refusal)
			require.Equal(t, &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(call)}, refusal)
			require.Zero(t, count)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

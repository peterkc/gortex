package store_pebble

import (
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildClosureCollectionCountsAffectedOutputs(t *testing.T) {
	// The smallest output belongs to the last field. The first proposal
	// fails both endpoint proofs but contributes only one count.
	for _, mutate := range []struct {
		name string
		run  func(*graph.Node)
	}{
		{"kind", func(n *graph.Node) { n.Kind = graph.KindFunction }},
		{"name", func(n *graph.Node) { n.Name = "replacement" }},
		{"language", func(n *graph.Node) { n.Language = "typescript" }},
		{"file", func(n *graph.Node) { n.FilePath = "wrong.go" }},
		{"line", func(n *graph.Node) { n.StartLine++ }},
		{"repo", func(n *graph.Node) { n.RepoPrefix = "other" }},
	} {
		for _, conflictFirst := range []bool{true, false} {
			order := "valid then conflicting"
			if conflictFirst {
				order = "conflicting then valid"
			}
			t.Run(mutate.name+"/"+order, func(t *testing.T) {
				d1, r1 := closureProofPair("z", "alpha")
				d2, r2 := closureProofPair("a", "omega")
				facts := setStateProofFacts([]*graph.Node{d1, r1, d2, r2})
				apple := ParseFacts{EffectiveLanguage: "swift", File: graph.FileMetaRow{FilePath: "gate.swift"},
					Nodes: []*graph.Node{{ID: "apple-marker", Language: "swift", FilePath: "gate.swift", Kind: graph.KindFile}}}
				b, exact := seedSyntheticCustody(t, facts, apple)
				bad := setStateProofFacts(nil)
				for _, n := range []*graph.Node{d1, r1, r2} {
					changed := *n
					mutate.run(&changed)
					bad.Nodes = append(bad.Nodes, &changed)
				}
				receipts := func(yield func(ParseFacts, error) bool) {
					first, last := facts, bad
					if conflictFirst {
						first, last = bad, facts
					}
					if yield(apple, nil) && yield(first, nil) {
						yield(last, nil)
					}
				}
				before := rebuildFrameworkCustodyRows(t, b)
				boundary := func(_, _ *graph.Node) bool { return true }
				stats, err := b.RebuildClosureCollection(t.Context(), receipts, 8, boundary, setStateProofOutput, closureTestApple)
				var refusal *RebuildUnknownProvenanceError
				require.ErrorIs(t, err, ErrRebuildClosureCollection)
				require.ErrorAs(t, err, &refusal)
				require.Equal(t, &RebuildUnknownProvenanceError{
					Count: 2,
					First: graph.EdgeIdentityFor(setStateProofOutput(d2, r2, "omega")),
				}, refusal)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
				stats, err = b.RebuildClosureCollection(t.Context(), exact, 8, boundary, setStateProofOutput, closureTestApple)
				require.NoError(t, err)
				require.EqualValues(t, 2, stats.Edges)
			})
		}
	}
}

func TestRebuildClosureCollectionInvalidInputsAndRows(t *testing.T) {
	for _, mode := range []string{"fanout", "nil boundary", "nil builder", "nil Apple predicate", "scratch occupied", "input row", "proposal row"} {
		t.Run(mode, func(t *testing.T) {
			dispatch, register := closureProofPair("one", "listeners")
			facts := setStateProofFacts([]*graph.Node{dispatch, register})
			b, _ := seedSyntheticCustody(t, facts)
			cap := 8
			boundary := func(_, _ *graph.Node) bool { return true }
			makeCall := setStateProofOutput
			isApple := closureTestApple
			want := "invalid closure collection stage inputs"
			switch mode {
			case "fanout":
				cap = 0
			case "nil boundary":
				boundary = nil
			case "nil builder":
				makeCall = nil
			case "nil Apple predicate":
				isApple = nil
			case "scratch occupied":
				want = "closure collection scratch range occupied"
				require.NoError(t, b.pending.Set(rebuildClosureKey('z'), []byte("sentinel"), pebble.NoSync))
			case "input row":
				want = "closure collection input row"
				large := *dispatch
				large.Meta = map[string]any{"cc_dispatch_field": "listeners", "padding": strings.Repeat("x", 16<<20)}
				raw, err := encodeNode(&large)
				require.NoError(t, err)
				require.NoError(t, b.pending.Set(pendingNodeKey(large.ID), raw, pebble.NoSync))
			case "proposal row":
				want = "closure collection scratch row"
				makeCall = func(from, to *graph.Node, field string) *graph.Edge {
					e := setStateProofOutput(from, to, field)
					e.Via = strings.Repeat("x", 16<<20)
					return e
				}
			}
			before := rebuildFrameworkCustodyRows(t, b)
			stats, err := b.RebuildClosureCollection(t.Context(), func(func(ParseFacts, error) bool) {
				t.Fatal("replayed receipts before validating closure input and proposals")
			}, cap, boundary, makeCall, isApple)
			require.ErrorIs(t, err, ErrRebuildClosureCollection)
			require.ErrorContains(t, err, want)
			if mode != "scratch occupied" {
				require.ErrorIs(t, err, ErrRebuildLinkInputSize)
			}
			require.Zero(t, stats)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func closureProofPair(id, field string) (*graph.Node, *graph.Node) {
	d := setStateProofMethod(id+"-dispatch", "dispatch", 5)
	r := setStateProofMethod(id+"-register", "register", 10)
	d.Meta = map[string]any{"cc_dispatch_field": field}
	r.Meta = map[string]any{"cc_append_field": field}
	return d, r
}

func TestRebuildClosureCollectionInstalledCustody(t *testing.T) {
	const file = "bus.swift"
	dispatch := &graph.Node{ID: "dispatch", Kind: graph.KindMethod, FilePath: file, Language: "swift",
		Meta: map[string]any{"cc_dispatch_field": "listeners"}}
	registrar := &graph.Node{ID: "registrar", Kind: graph.KindMethod, FilePath: file, Language: "swift",
		Meta: map[string]any{"cc_append_field": "listeners"}}
	makeCall := func(d, r *graph.Node, field string) *graph.Edge {
		return &graph.Edge{From: d.ID, To: r.ID, Kind: graph.EdgeCalls, FilePath: d.FilePath, Line: d.StartLine,
			Origin: graph.OriginSpeculative, Meta: map[string]any{"channel_field": field}}
	}
	b, receipts := seedSyntheticCustody(t, ParseFacts{
		EffectiveLanguage: "swift",
		File:              graph.FileMetaRow{FilePath: file},
		Nodes:             []*graph.Node{dispatch, registrar},
	})
	stats, err := b.RebuildClosureCollection(t.Context(), receipts, 8,
		func(d, r *graph.Node) bool { return d.WorkspaceID == r.WorkspaceID }, makeCall, closureTestApple)
	require.NoError(t, err)
	require.EqualValues(t, 1, stats.Edges)
	want := makeCall(dispatch, registrar, "listeners")
	live, err := b.pendingEdge(pendingEdgeKey(want))
	require.NoError(t, err)
	require.Equal(t, want, live)
	requireSyntheticCustody(t, b, map[graph.EdgeIdentity]rebuildEdgeProof{graph.EdgeIdentityFor(want): rebuildProofClosureCollection})
}

func closureTestApple(language string) bool { return language == "swift" }

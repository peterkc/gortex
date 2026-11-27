package store_pebble

import (
	"path"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func rebuildRailsReviewRules() RebuildRailsRules {
	return RebuildRailsRules{
		Call: func(e *graph.Edge) (string, string) {
			if !graph.IsUnresolvedTarget(e.To) {
				return "", ""
			}
			name, _ := e.Meta["recv_const"].(string)
			return name, graph.UnresolvedName(e.To)
		},
		Source:    func(file string) bool { return strings.HasSuffix(file, ".rb") },
		Owner:     func(e *graph.Edge) string { return e.To },
		Candidate: func(n *graph.Node) bool { return n.Kind == graph.KindType },
		Preferred: func(string, string) bool { return false },
		Directory: path.Dir,
		Model:     func(string) bool { return false },
		Rebind: func(e *graph.Edge, target string) graph.EdgeReindex {
			old := e.To
			e.To = target
			return graph.EdgeReindex{Edge: e, OldTo: old}
		},
	}
}

func TestRebuildRailsNonFrozenRawClaim(t *testing.T) {
	const file = "routes.rb"
	caller := &graph.Node{ID: "caller", Kind: graph.KindFunction, FilePath: file, Language: "ruby"}
	class := &graph.Node{ID: "service", Name: "MailService", Kind: graph.KindType, FilePath: file, Language: "ruby"}
	call := &graph.Edge{From: caller.ID, To: "unresolved::send", Kind: graph.EdgeCalls, FilePath: file, Line: 1,
		Meta: map[string]any{"recv_const": class.Name}}
	b, receipts := seedSyntheticCustody(t, ParseFacts{
		EffectiveLanguage: "ruby",
		File:              graph.FileMetaRow{FilePath: file},
		Nodes:             []*graph.Node{caller, class},
		Edges:             []*graph.Edge{call},
	})
	settled := *call
	settled.To = "settled"
	require.NoError(t, b.withWrites(func() error {
		return b.addEdgeWithCustody(&settled, rebuildEdgeCustody{raw: pendingEdgeKey(call)})
	}))
	before := rebuildFrameworkCustodyRows(t, b)
	require.NoError(t, b.FreezeRailsInputs(t.Context(), func(graph.FrameworkCensusEdge) bool { return true }))
	stats, err := b.RebuildRails(t.Context(), receipts, rebuildRailsReviewRules())
	var refused *RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(call)}, refused)
	require.Zero(t, stats)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
}

func TestRebuildRailsComparesEmptyLiveElection(t *testing.T) {
	const file = "routes.rb"
	caller := &graph.Node{ID: "caller", Kind: graph.KindFunction, FilePath: file, Language: "ruby"}
	first := &graph.Node{ID: "first", Name: "MailService", Kind: graph.KindType, FilePath: file, Language: "ruby"}
	second := *first
	second.ID = "second"
	call := &graph.Edge{From: caller.ID, To: "unresolved::send", Kind: graph.EdgeCalls, FilePath: file, Line: 1,
		Meta: map[string]any{"recv_const": first.Name}}
	facts := ParseFacts{EffectiveLanguage: "ruby", File: graph.FileMetaRow{FilePath: file},
		Nodes: []*graph.Node{caller, first, &second}, Edges: []*graph.Edge{call}}
	b, _ := seedSyntheticCustody(t, facts)
	before := rebuildFrameworkCustodyRows(t, b)
	facts.Nodes = []*graph.Node{caller, first}
	receipts := func(yield func(ParseFacts, error) bool) { yield(facts, nil) }
	require.NoError(t, b.FreezeRailsInputs(t.Context(), func(graph.FrameworkCensusEdge) bool { return true }))
	stats, err := b.RebuildRails(t.Context(), receipts, rebuildRailsReviewRules())
	var refused *RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(call)}, refused)
	require.Zero(t, stats)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
}

func TestRebuildRailsReceiptCallerEntersSource(t *testing.T) {
	const file = "routes.txt"
	caller := &graph.Node{ID: "caller", Kind: graph.KindFunction, FilePath: file, Language: "ruby"}
	class := &graph.Node{ID: "first", Name: "MailService", Kind: graph.KindType, FilePath: file, Language: "ruby"}
	call := &graph.Edge{From: caller.ID, To: "unresolved::send", Kind: graph.EdgeCalls, FilePath: file, Line: 1,
		Meta: map[string]any{"recv_const": class.Name}}
	facts := ParseFacts{EffectiveLanguage: "ruby", File: graph.FileMetaRow{FilePath: file},
		Nodes: []*graph.Node{caller, class}, Edges: []*graph.Edge{call}}
	b, _ := seedSyntheticCustody(t, facts)
	before := rebuildFrameworkCustodyRows(t, b)
	facts.Nodes = []*graph.Node{class}
	rawCaller := *caller
	rawCaller.FilePath = "routes.rb"
	receipts := func(yield func(ParseFacts, error) bool) {
		if yield(facts, nil) {
			yield(producerProofFacts(&rawCaller), nil)
		}
	}
	require.NoError(t, b.FreezeRailsInputs(t.Context(), func(graph.FrameworkCensusEdge) bool { return true }))
	stats, err := b.RebuildRails(t.Context(), receipts, rebuildRailsReviewRules())
	var refused *RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(call)}, refused)
	require.Zero(t, stats)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
}

func TestRebuildRailsFirstMemberIgnoresLaterUncertainOwner(t *testing.T) {
	const file = "routes.rb"
	caller := &graph.Node{ID: "caller", Kind: graph.KindFunction, FilePath: file, Language: "ruby"}
	class := &graph.Node{ID: "service", Name: "MailService", Kind: graph.KindType, FilePath: file, Language: "ruby"}
	certain := &graph.Node{ID: "m", Name: "send", Kind: graph.KindMethod, FilePath: file, Language: "ruby"}
	call := &graph.Edge{From: caller.ID, To: "unresolved::send", Kind: graph.EdgeCalls, FilePath: file, Line: 1,
		Meta: map[string]any{"recv_const": class.Name}}
	member := func(id, owner string, line int) *graph.Edge {
		return &graph.Edge{From: id, To: owner, Kind: graph.EdgeMemberOf, FilePath: file, Line: line}
	}
	for _, id := range []string{"a", "z"} {
		rival := *certain
		rival.ID = id
		winnerOwn := member(certain.ID, class.ID, 2)
		rivalOwn := member(rival.ID, class.ID, 3)
		unknown := member(rival.ID, "other-owner", 4)
		facts := ParseFacts{EffectiveLanguage: "ruby", File: graph.FileMetaRow{FilePath: file},
			Nodes: []*graph.Node{caller, class, certain, &rival}, Edges: []*graph.Edge{call, winnerOwn, rivalOwn, unknown}}
		b, _ := seedSyntheticCustody(t, facts)
		before := rebuildFrameworkCustodyRows(t, b)
		facts.Edges = []*graph.Edge{call, winnerOwn, rivalOwn}
		receipts := func(yield func(ParseFacts, error) bool) { yield(facts, nil) }
		require.NoError(t, b.FreezeRailsInputs(t.Context(), func(graph.FrameworkCensusEdge) bool { return true }))
		stats, err := b.RebuildRails(t.Context(), receipts, rebuildRailsReviewRules())
		if id < certain.ID {
			var refused *RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(call)}, refused)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
			continue
		}
		require.NoError(t, err)
		require.Equal(t, RebuildRailsStats{Stubs: 1, Moves: 1}, stats)
		want := *call
		want.To = certain.ID
		got, err := b.pendingEdge(pendingEdgeKey(&want))
		require.NoError(t, err)
		require.NotNil(t, got)
	}
}

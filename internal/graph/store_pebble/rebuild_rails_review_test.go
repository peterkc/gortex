package store_pebble

import (
	"path"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
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
	for _, tc := range []struct {
		name      string
		oversized bool
	}{
		{"reports provenance disagreement", false},
		{"checks receipt-made caller size", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			liveFacts := facts
			if tc.oversized {
				liveCaller := *caller
				liveCaller.Meta = map[string]any{"padding": strings.Repeat("x", 16<<20)}
				liveFacts.Nodes = []*graph.Node{&liveCaller, class}
			}
			b, _ := seedSyntheticCustody(t, liveFacts)
			before := rebuildFrameworkCustodyRows(t, b)
			proof := facts
			proof.Nodes = []*graph.Node{class}
			rawCaller := *caller
			rawCaller.FilePath = "routes.rb"
			receipts := func(yield func(ParseFacts, error) bool) {
				if yield(proof, nil) {
					yield(producerProofFacts(&rawCaller), nil)
				}
			}
			require.NoError(t, b.FreezeRailsInputs(t.Context(), func(graph.FrameworkCensusEdge) bool { return true }))
			stats, err := b.RebuildRails(t.Context(), receipts, rebuildRailsReviewRules())
			if tc.oversized {
				require.ErrorIs(t, err, ErrRebuildLinkInputSize)
				require.ErrorContains(t, err, "rails caller node")
			} else {
				var refused *RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refused)
				require.Equal(t, &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(call)}, refused)
			}
			require.Zero(t, stats)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
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

func TestRebuildRailsDirectModelCustody(t *testing.T) {
	const routeFile, modelFile = "routes.rb", "Invoice.cs"
	caller := &graph.Node{ID: "caller", Kind: graph.KindFunction, FilePath: routeFile, Language: "ruby"}
	model := &graph.Node{ID: "model", Name: "Invoice", Kind: graph.KindType, FilePath: modelFile, Language: "csharp"}
	call := &graph.Edge{From: caller.ID, To: "unresolved::find", Kind: graph.EdgeCalls, FilePath: routeFile, Line: 1,
		Meta: map[string]any{"recv_const": model.Name}}
	modelEdge := &graph.Edge{From: model.ID, To: "table::invoices", Kind: graph.EdgeModelsTable, FilePath: modelFile, Line: 2}
	rules := rebuildRailsReviewRules()
	rules.Model = func(string) bool { return true }
	for _, tc := range []string{"direct C# receipt", "missing receipt", "wrong owner file", "empty language", "stale custody", "malformed custody"} {
		t.Run(tc, func(t *testing.T) {
			route := producerProofFacts(caller)
			route.Edges = []*graph.Edge{call}
			modelFacts := producerProofFacts(model)
			modelFacts.Edges = []*graph.Edge{modelEdge}
			b, _ := seedSyntheticCustody(t, route, modelFacts)
			receipt := modelFacts
			switch tc {
			case "missing receipt", "wrong owner file", "empty language":
				receipt.Edges = nil
			case "stale custody", "malformed custody":
				val, err := encodeEdge(modelEdge)
				require.NoError(t, err)
				custody, err := (rebuildEdgeCustody{proof: rebuildProofEFCoreModel}).encode(val)
				require.NoError(t, err)
				if tc == "stale custody" {
					custody[len(custody)-1] ^= 1
				} else {
					custody = custody[:1]
				}
				require.NoError(t, b.pending.Set(rebuildLinkRecorderKey(pendingEdgeKey(modelEdge)), custody, pebble.NoSync))
			}
			facts := []ParseFacts{route, receipt}
			switch tc {
			case "wrong owner file":
				facts = append(facts, ParseFacts{EffectiveLanguage: "csharp", File: graph.FileMetaRow{FilePath: "Other.cs"}, Edges: []*graph.Edge{modelEdge}})
			case "empty language":
				facts = append(facts, ParseFacts{File: graph.FileMetaRow{FilePath: modelFile}, Edges: []*graph.Edge{modelEdge}})
			}
			receipts := func(yield func(ParseFacts, error) bool) {
				for _, f := range facts {
					if !yield(f, nil) {
						return
					}
				}
			}
			before := rebuildFrameworkCustodyRows(t, b)
			match := func(e graph.FrameworkCensusEdge) bool { return e.RecvConst != "" }
			require.NoError(t, b.FreezeRailsInputs(t.Context(), match))
			stats, err := b.RebuildRails(t.Context(), receipts, rules)
			switch tc {
			case "direct C# receipt":
				require.NoError(t, err)
				require.Equal(t, RebuildRailsStats{Stubs: 1, Moves: 1}, stats)
				want := *call
				want.To = model.ID
				got, err := b.pendingEdge(pendingEdgeKey(&want))
				require.NoError(t, err)
				require.NotNil(t, got)
			case "malformed custody":
				require.ErrorIs(t, err, ErrRebuildRails)
				require.NotErrorIs(t, err, ErrRebuildUnknownProvenance)
				require.ErrorContains(t, err, "edge custody format")
				require.Zero(t, stats)
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
			default:
				var refused *RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refused)
				require.Equal(t, &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(call)}, refused)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
			}
			busy, err := rebuildGuardHasPrefix(t.Context(), b.pending, rebuildRailsPrefix)
			require.NoError(t, err)
			require.False(t, busy)
		})
	}
}

func TestRebuildRailsFreezeGuardsAndRoleCap(t *testing.T) {
	padding := strings.Repeat("x", 16<<20)
	for _, tc := range []struct {
		name, file, receiver string
		wantErr              bool
	}{
		{"Ruby role", "routes.rb", "MailService", true},
		{"not a Ruby source", "routes.txt", "MailService", false},
		{"no receiver role", "routes.rb", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caller := &graph.Node{ID: "caller", Kind: graph.KindFunction, FilePath: tc.file, Language: "ruby"}
			call := &graph.Edge{From: caller.ID, To: "unresolved::send", Kind: graph.EdgeCalls, FilePath: tc.file, Line: 1,
				Context: padding, Meta: map[string]any{"recv_const": tc.receiver}}
			facts := producerProofFacts(caller)
			facts.Edges = []*graph.Edge{call}
			b, receipts := seedSyntheticCustody(t, facts)
			rules := rebuildRailsReviewRules()
			ctx := t.Context()
			stats, err := b.RebuildRails(ctx, receipts, rules)
			require.ErrorContains(t, err, "rails census missing")
			require.Zero(t, stats)
			require.ErrorIs(t, b.FreezeRailsInputs(ctx, nil), ErrRebuildRails)
			match := func(e graph.FrameworkCensusEdge) bool { return e.RecvConst != "" }
			require.NoError(t, b.FreezeRailsInputs(ctx, match))
			require.ErrorIs(t, b.FreezeRailsInputs(ctx, match), ErrRebuildRails)
			stats, err = b.RebuildRails(ctx, receipts, rules)
			if tc.wantErr {
				require.ErrorIs(t, err, ErrRebuildLinkInputSize)
			} else {
				require.NoError(t, err)
			}
			require.Zero(t, stats)
			require.NoError(t, b.FreezeRailsInputs(ctx, match))
			stats, err = b.RebuildRails(ctx, nil, rules)
			require.ErrorContains(t, err, "rails stage inputs")
			require.Zero(t, stats)
			busy, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildRailsPrefix)
			require.NoError(t, err)
			require.False(t, busy)
		})
	}
}

func TestRebuildRailsElectionProofGuards(t *testing.T) {
	const file, lang = "routes.rb", "ruby"
	for _, scenario := range []string{"exact", "class conflict", "member conflict", "missing source", "proved-only uncertainty"} {
		t.Run(scenario, func(t *testing.T) {
			caller := &graph.Node{ID: "caller", Kind: graph.KindFunction, FilePath: file, Language: lang}
			class := &graph.Node{ID: "class", Name: "MailService", Kind: graph.KindType, FilePath: file, Language: lang}
			method := &graph.Node{ID: "method", Name: "send", Kind: graph.KindMethod, FilePath: file, Language: lang}
			member := &graph.Edge{From: method.ID, To: class.ID, Kind: graph.EdgeMemberOf, FilePath: file, Line: 2}
			call := &graph.Edge{From: caller.ID, To: "unresolved::send", Kind: graph.EdgeCalls, FilePath: file, Line: 1,
				Meta: map[string]any{"recv_const": class.Name}}
			facts := producerProofFacts(caller)
			facts.Nodes = append(facts.Nodes, class, method)
			facts.Edges = []*graph.Edge{call, member}
			proof := facts
			var extra []ParseFacts
			switch scenario {
			case "class conflict":
				conflict := *class
				conflict.FilePath = "other.rb"
				extra = append(extra, producerProofFacts(&conflict))
			case "member conflict":
				conflict := *method
				conflict.FilePath = "other.rb"
				extra = append(extra, producerProofFacts(&conflict))
			case "missing source":
				proof.Nodes = []*graph.Node{class, method}
			case "proved-only uncertainty":
				// Two live classes in the same directory leave the live
				// election empty. Replay finds one class but a possible member.
				other := *class
				other.ID = "other-class"
				field := *method
				field.Kind = graph.KindField
				facts.Nodes = []*graph.Node{caller, class, &other, &field}
			}
			b, _ := seedSyntheticCustody(t, facts)
			before := rebuildFrameworkCustodyRows(t, b)
			require.NoError(t, b.FreezeRailsInputs(t.Context(), func(graph.FrameworkCensusEdge) bool { return true }))
			stats, err := b.RebuildRails(t.Context(), sqlCallsiteTestReceipts(append(extra, proof)...), rebuildRailsReviewRules())
			if scenario == "exact" || scenario == "class conflict" || scenario == "member conflict" {
				require.NoError(t, err)
				require.Equal(t, RebuildRailsStats{Stubs: 1, Moves: 1}, stats)
				want := *call
				want.To = method.ID
				requireRebuildLinkCustody(t, b, &want, rebuildEdgeCustody{raw: pendingEdgeKey(call)})
				return
			}
			require.ErrorIs(t, err, ErrRebuildRails)
			var refused *RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(call)}, refused)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildRailsFreezeIgnoresReferences(t *testing.T) {
	const file, lang = "routes.rb", "ruby"
	caller := &graph.Node{ID: "caller", Kind: graph.KindFunction, FilePath: file, Language: lang}
	ref := &graph.Edge{From: caller.ID, To: "unresolved::send", Kind: graph.EdgeReferences, FilePath: file, Line: 1,
		Meta: map[string]any{"recv_const": "MailService"}}
	facts := producerProofFacts(caller)
	facts.Edges = []*graph.Edge{ref}
	b, receipts := seedSyntheticCustody(t, facts)
	ctx := t.Context()
	before := rebuildFrameworkCustodyRows(t, b)
	require.NoError(t, b.FreezeRailsInputs(ctx, func(e graph.FrameworkCensusEdge) bool {
		t.Errorf("Rails freeze passed a %v to its matcher", e.Kind)
		return true
	}))
	stats, err := b.RebuildRails(ctx, receipts, rebuildRailsReviewRules())
	require.NoError(t, err)
	require.Zero(t, stats)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
}

func TestRebuildRailsFirstReceiptQuery(t *testing.T) {
	const file = "routes.rb"
	caller := &graph.Node{ID: "caller", Kind: graph.KindFunction, FilePath: file, Language: "ruby"}
	class := &graph.Node{ID: "foo", Name: "Foo", Kind: graph.KindType, FilePath: file, Language: "ruby"}
	live := &graph.Edge{From: caller.ID, To: "unresolved::find", Kind: graph.EdgeCalls, FilePath: file, Line: 1,
		Meta: map[string]any{"recv_const": "Bar"}}
	for _, first := range []string{"Foo", "Bar", ""} {
		t.Run(first, func(t *testing.T) {
			facts := producerProofFacts(caller)
			facts.Nodes = append(facts.Nodes, class)
			facts.Edges = []*graph.Edge{live}
			b, _ := seedSyntheticCustody(t, facts)
			before := rebuildFrameworkCustodyRows(t, b)
			raw := *live
			raw.Meta = map[string]any{"recv_const": first}
			facts.Edges = []*graph.Edge{&raw, live}
			require.NoError(t, b.FreezeRailsInputs(t.Context(), func(graph.FrameworkCensusEdge) bool { return true }))
			stats, err := b.RebuildRails(t.Context(), sqlCallsiteTestReceipts(facts), rebuildRailsReviewRules())
			if first == "Foo" {
				var refusal *RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refusal)
				require.Equal(t, &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(live)}, refusal)
				require.Zero(t, stats)
			} else {
				require.NoError(t, err)
				require.Equal(t, RebuildRailsStats{Stubs: 1}, stats)
			}
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

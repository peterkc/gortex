package indexer

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildReturnsToRejectsUnprovedReceipt(t *testing.T) {
	const file = "caller.go"
	for _, tc := range []struct{ name, kind string }{
		{"deferred contender cannot hide behind supported winner", "call"},
		{"return recorded outside its actual receipt", "return"},
		{"unrelated name in another recording file does not enter named join", "different"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			returns := &graph.Edge{From: "caller.go::Caller", To: "caller.go::Caller#local:v", Kind: graph.EdgeReturnsTo,
				FilePath: file, Line: 2, Meta: map[string]any{"returns_to_call": true, "callee_target": "unresolved::*.Run"}}
			call := &graph.Edge{From: returns.From, To: "a.go::A.Run", Kind: graph.EdgeCalls, FilePath: file, Line: 2}
			raw := *call
			raw.To = "unresolved::*.Run"
			receipts := []store_pebble.ParseFacts{{EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: file}, Edges: []*graph.Edge{&raw}}}
			other := store_pebble.ParseFacts{EffectiveLanguage: "custom", File: graph.FileMetaRow{FilePath: "probe.custom"}}
			rows := []*graph.Edge{returns, call}
			if tc.kind == "return" {
				other.Edges = []*graph.Edge{returns}
			} else {
				receipts[0].Edges = append(receipts[0].Edges, returns)
				competitor := *call
				competitor.To = "z.go::Z.Run"
				if tc.kind == "different" {
					competitor.To, competitor.FilePath = "d.go::Different", other.File.FilePath
				}
				other.Edges = []*graph.Edge{&competitor}
				rows = append(rows, &competitor)
			}
			receipts = append(receipts, other)
			build, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "working"), store_pebble.FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, build.Close()) })
			build.AddBatch(nil, rows)
			err = runRebuildReturnsTo(t.Context(), build, func(yield func(store_pebble.ParseFacts, error) bool) {
				for _, receipt := range receipts {
					if !yield(receipt, nil) {
						return
					}
				}
			})
			if tc.kind == "different" {
				require.NoError(t, err)
				lifted := *returns
				lifted.From = call.To
				rows[0] = &lifted
			} else {
				var refusal *store_pebble.RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refusal)
				require.Equal(t, uint64(1), refusal.Count)
				require.Equal(t, graph.EdgeIdentityFor(returns), refusal.First)
			}
			require.ElementsMatch(t, rows, readRebuildReturnProofRows(t, build))
		})
	}
}

func TestRebuildReturnsToRefusesMovingSourceDestination(t *testing.T) {
	const file = "all.go"
	incoming := &graph.Edge{From: "all.go::A", To: "unresolved::*.field", Kind: graph.EdgeReturnsTo,
		FilePath: file, Line: 7, Meta: map[string]any{"returns_to_call": false, "callee_target": "unresolved::B"}}
	moving := *incoming
	moving.From, moving.Meta = "all.go::B", map[string]any{"returns_to_call": true, "callee_target": "unresolved::C"}
	rows := []*graph.Edge{
		incoming, &moving,
		{From: incoming.From, To: moving.From, Kind: graph.EdgeCalls, FilePath: file, Line: 7},
		{From: moving.From, To: "all.go::C", Kind: graph.EdgeCalls, FilePath: file, Line: 7},
	}
	build, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "working"), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, build.Close()) })
	facts := store_pebble.ParseFacts{EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: file}, Edges: rows}
	require.NoError(t, build.AddParsedFile("", facts))
	err = runRebuildReturnsTo(t.Context(), build, func(yield func(store_pebble.ParseFacts, error) bool) { yield(facts, nil) })
	var chain *store_pebble.RebuildAttributionChainError
	require.ErrorAs(t, err, &chain)
	require.Equal(t, uint64(1), chain.Count)
	require.Equal(t, graph.EdgeIdentityFor(incoming), chain.Old)
	require.Equal(t, graph.EdgeIdentityFor(&moving), chain.Destination)
	require.ElementsMatch(t, rows, readRebuildReturnProofRows(t, build))
}

func TestRebuildReturnsToNativeLineAndNameSelection(t *testing.T) {
	const caller, file, otherFile = "caller.go::Caller", "caller.go", "helper.ts"
	calls := []*graph.Edge{
		{From: caller, To: "helper.ts::Z.Run", Kind: graph.EdgeCalls, FilePath: otherFile, Line: -2},
		{From: caller, To: "caller.go::A.Other", Kind: graph.EdgeCalls, FilePath: file, Line: 3},
		{From: caller, To: "caller.go::B.Run", Kind: graph.EdgeCalls, FilePath: file, Line: 3},
	}
	build, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "working"), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, build.Close()) })
	facts := store_pebble.ParseFacts{EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: file}, Edges: slices.Clone(calls[1:])}
	other := store_pebble.ParseFacts{EffectiveLanguage: "typescript", File: graph.FileMetaRow{FilePath: otherFile}, Edges: calls[:1]}
	want := slices.Clone(calls)
	for _, tc := range []struct {
		id, name, from string
		line, callLine int
	}{
		{"earliest", "", calls[0].To, 0, 0},
		{"named", "unresolved::*.Run", calls[2].To, 7, 3},
		{"no fallback", "unresolved::Missing", caller, 3, 0},
		{"unnamed", "", calls[1].To, 3, 0},
	} {
		edge := &graph.Edge{From: caller, To: tc.id, Kind: graph.EdgeReturnsTo, FilePath: file,
			Line: tc.line, Meta: map[string]any{"returns_to_call": true, "callee_target": tc.name, "call_line": tc.callLine}}
		facts.Edges = append(facts.Edges, edge)
		lifted := *edge
		lifted.From = tc.from
		want = append(want, &lifted)
	}
	require.NoError(t, build.AddParsedFile("", facts))
	require.NoError(t, build.AddParsedFile("", other))
	require.NoError(t, runRebuildReturnsTo(t.Context(), build, func(yield func(store_pebble.ParseFacts, error) bool) {
		if yield(facts, nil) {
			yield(other, nil)
		}
	}))
	require.ElementsMatch(t, want, readRebuildReturnProofRows(t, build))
}

func readRebuildReturnProofRows(t *testing.T, build *store_pebble.BuildStore) []*graph.Edge {
	t.Helper()
	var rows []*graph.Edge
	require.NoError(t, build.RebuildView().VisitEdges(t.Context(), func(e *graph.Edge) error {
		rows = append(rows, e)
		return nil
	}))
	return rows
}

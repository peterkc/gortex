package store_pebble

import (
	"iter"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

const sharedClaimsTestBudget = 1 << 20

func TestReduceRebuildSharedClaimsWinners(t *testing.T) {
	literal := &graph.Node{ID: "str::shared", Kind: graph.KindString, FilePath: "a.go", Language: "go", StartLine: 1}
	literalLast, literalOther := *literal, *literal
	literalLast.StartLine = 9
	literalOther.FilePath = "b.go"
	annotation := &graph.Node{ID: "annotation::java::Test", Kind: graph.KindType, Name: "Test", FilePath: "a.java", Language: "java", Meta: map[string]any{"kind": "annotation", "synthetic": true}}
	annotationOther := *annotation
	annotationOther.FilePath = "z.java"
	column := &graph.Node{ID: "col::postgres::public.users.email", Kind: graph.KindColumn, Name: "email", FilePath: "schema.go", Language: "go", StartLine: 4, Meta: map[string]any{"table": "users", "column": "email", "dialect": "postgres", "schema": "public"}}
	columnFirst, columnLast, columnOtherPath := *column, *column, *column
	// The first row from a.go beats b.go by encoded bytes, but the last
	// loses. The later node path from c.go loses regardless of its bytes.
	columnFirst.StartLine = 1
	columnLast.StartLine = 7
	columnOtherPath.FilePath, columnOtherPath.StartLine = "z.go", 0
	unique := &graph.Node{ID: "c.go::unique", Kind: graph.KindFunction, FilePath: "c.go", Language: "go"}
	a := sharedClaimsTestFacts("a.go", literal, &literalLast, &annotationOther, &columnFirst, &columnLast)
	b := sharedClaimsTestFacts("b.go", &literalOther, annotation, column)
	c := sharedClaimsTestFacts("c.go", &columnOtherPath, unique)
	// Same endpoints and kind are not collisions at distinct lines or paths.
	edge := &graph.Edge{From: unique.ID, To: literal.ID, Kind: graph.EdgeReferences, FilePath: "a.go", Line: 1}
	line, path := *edge, *edge
	line.Line, path.FilePath = 2, "c.go"
	a.Edges, b.Edges, c.Edges = []*graph.Edge{edge}, []*graph.Edge{&line}, []*graph.Edge{&path}

	want := map[string][][]byte{
		literal.ID:    {sharedClaimsTestNodeBytes(t, literal), sharedClaimsTestNodeBytes(t, &literalLast)},
		annotation.ID: {sharedClaimsTestNodeBytes(t, annotation)},
		column.ID:     {sharedClaimsTestNodeBytes(t, column)},
		unique.ID:     {sharedClaimsTestNodeBytes(t, unique)},
	}
	store := newBuild(t, FullProfile)
	for _, order := range []struct {
		name  string
		facts []ParseFacts
	}{
		{"abc", []ParseFacts{a, b, c}}, {"acb", []ParseFacts{a, c, b}},
		{"bac", []ParseFacts{b, a, c}}, {"bca", []ParseFacts{b, c, a}},
		{"cab", []ParseFacts{c, a, b}}, {"cba", []ParseFacts{c, b, a}},
	} {
		t.Run(order.name, func(t *testing.T) {
			got := make(map[string][][]byte)
			stats, err := store.ReduceRebuildSharedClaims(t.Context(), sharedClaimsTestBudget, sharedClaimsTestReceipts(order.facts...), func(keep func(string, uint64) (bool, error)) error {
				for i, facts := range order.facts {
					for _, n := range facts.Nodes {
						yes, err := keep(n.ID, uint64(i+1))
						if err != nil {
							return err
						}
						if yes {
							got[n.ID] = append(got[n.ID], sharedClaimsTestNodeBytes(t, n))
						}
					}
				}
				return nil
			})
			require.NoError(t, err)
			require.Equal(t, want, got)
			require.Equal(t, uint64(3), stats.Shadowed)
			require.Zero(t, stats.ShadowedEdges)
			require.Equal(t, uint64(1), stats.AcceptedStrings)
			require.Equal(t, uint64(1), stats.AcceptedAnnotations)
			require.Equal(t, uint64(1), stats.AcceptedColumns)
			require.Zero(t, stats.UnexplainedNodes)
			requireSharedClaimsTestEmpty(t, store)
		})
	}
}

func TestReduceRebuildSharedClaimsRejects(t *testing.T) {
	literal := &graph.Node{ID: "str::shared", Kind: graph.KindString, FilePath: "a.go"}
	literalOther := *literal
	literalOther.FilePath = "b.go"
	mixed := &graph.Node{ID: "mixed", Kind: graph.KindString, FilePath: "a.go"}
	mixedOther := *mixed
	mixedOther.Kind, mixedOther.FilePath = graph.KindFunction, "b.go"
	edge := &graph.Edge{From: mixed.ID, To: literal.ID, Kind: graph.EdgeReferences, FilePath: "a.go", Line: 1}
	edgeOther := *edge
	edgeOther.Origin = "other" // Origin is not part of edge identity.
	for _, tc := range []struct {
		name                       string
		mixed, duplicateEdge       bool
		unexplained, shadowedEdges uint64
	}{
		{"mixed_node_kinds", true, false, 1, 0},
		{"duplicate_edge", false, true, 0, 1},
		{"both", true, true, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := sharedClaimsTestFacts("a.go", literal, mixed)
			b := sharedClaimsTestFacts("b.go", &literalOther)
			if tc.mixed {
				b.Nodes = append(b.Nodes, &mixedOther)
				b.File.NodeCount++
			}
			a.Edges = []*graph.Edge{edge}
			if tc.duplicateEdge {
				b.Edges = []*graph.Edge{&edgeOther}
			}
			store := newBuild(t, FullProfile)
			called := false
			stats, err := store.ReduceRebuildSharedClaims(t.Context(), sharedClaimsTestBudget, sharedClaimsTestReceipts(a, b), func(func(string, uint64) (bool, error)) error {
				called = true
				return nil
			})
			var refusal *RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refusal)
			require.Equal(t, tc.unexplained+tc.shadowedEdges, refusal.Count)
			require.False(t, called)
			require.Equal(t, 1+tc.unexplained, stats.Shadowed)
			require.Equal(t, tc.shadowedEdges, stats.ShadowedEdges)
			require.Equal(t, uint64(1), stats.AcceptedStrings)
			require.Zero(t, stats.AcceptedAnnotations)
			require.Zero(t, stats.AcceptedColumns)
			require.Equal(t, tc.unexplained, stats.UnexplainedNodes)
			require.NotZero(t, stats.ScratchBytes)
			requireSharedClaimsTestEmpty(t, store)
		})
	}
}

func TestReduceRebuildSharedClaimsRejectsRepeatedLosingPath(t *testing.T) {
	// Both z.go claims lose to a.go. Checking only the winner's path
	// would miss the repeated stamped path and accept this key.
	a := &graph.Node{ID: "string::shared", Kind: graph.KindString, FilePath: "a.go"}
	z := *a
	z.FilePath = "z.go"
	store := newBuild(t, FullProfile)
	stats, err := store.ReduceRebuildSharedClaims(t.Context(), sharedClaimsTestBudget, sharedClaimsTestReceipts(
		sharedClaimsTestFacts("first.go", &z), sharedClaimsTestFacts("second.go", a), sharedClaimsTestFacts("third.go", &z),
	), nil)
	var refusal *RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, uint64(1), stats.Shadowed)
	require.Equal(t, uint64(1), stats.UnexplainedNodes)
	require.Zero(t, stats.AcceptedStrings)
	requireSharedClaimsTestEmpty(t, store)
}

func TestReduceRebuildSharedClaimsCapacityAfterCensus(t *testing.T) {
	a := sharedClaimsTestFacts("a.go", &graph.Node{ID: "str::shared", Kind: graph.KindString, FilePath: "a.go"})
	b := sharedClaimsTestFacts("b.go", &graph.Node{ID: "str::shared", Kind: graph.KindString, FilePath: "b.go"})
	receipts := sharedClaimsTestReceipts(a, b)
	store := newBuild(t, FullProfile)
	raw, err := store.CountRebuildShadowedIDs(t.Context(), sharedClaimsTestBudget, func(node func(string, uint64) error, _ func(graph.EdgeIdentity, uint64) error) error {
		if err := node(a.Nodes[0].ID, 1); err != nil {
			return err
		}
		return node(b.Nodes[0].ID, 2)
	})
	require.NoError(t, err)
	require.Equal(t, uint64(1), raw.Shadowed)
	// The raw census fits exactly. The winner rows need additional space.
	calls := 0
	use := func(func(string, uint64) (bool, error)) error { calls++; return nil }
	stats, err := store.ReduceRebuildSharedClaims(t.Context(), raw.ScratchBytes, receipts, use)
	require.ErrorIs(t, err, ErrRebuildLinkInputSize)
	require.Zero(t, stats)
	require.Zero(t, calls)
	requireSharedClaimsTestEmpty(t, store)

	stats, err = store.ReduceRebuildSharedClaims(t.Context(), sharedClaimsTestBudget, receipts, use)
	require.NoError(t, err)
	require.Equal(t, uint64(1), stats.Shadowed)
	require.Equal(t, uint64(1), stats.AcceptedStrings)
	require.Equal(t, 1, calls)
	requireSharedClaimsTestEmpty(t, store)
}

func sharedClaimsTestFacts(path string, nodes ...*graph.Node) ParseFacts {
	return ParseFacts{File: graph.FileMetaRow{FilePath: path, NodeCount: len(nodes)}, Nodes: nodes}
}

func sharedClaimsTestReceipts(facts ...ParseFacts) iter.Seq2[ParseFacts, error] {
	return func(yield func(ParseFacts, error) bool) {
		for _, f := range facts {
			if !yield(f, nil) {
				return
			}
		}
	}
}

func sharedClaimsTestNodeBytes(t *testing.T, n *graph.Node) []byte {
	t.Helper()
	raw, err := encodeNode(n)
	require.NoError(t, err)
	return raw
}

func requireSharedClaimsTestEmpty(t *testing.T, b *BuildStore) {
	t.Helper()
	// A fresh build has no visible rows. Check both DBs to detect leaked
	// scratch, graph, and file-metadata rows without naming scratch prefixes.
	pending, err := rebuildGuardHasPrefix(t.Context(), b.pending, nil)
	require.NoError(t, err)
	require.False(t, pending, "scratch or pending graph rows leaked")
	working, err := rebuildGuardHasPrefix(t.Context(), b.db, nil)
	require.NoError(t, err)
	require.False(t, working, "working graph rows leaked")
}

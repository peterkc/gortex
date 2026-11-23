package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"math"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildCloneApplyUpsertsPrivateRows(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	a := &graph.Node{ID: "a", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: "a.go"}
	z := &graph.Node{ID: "z", Kind: graph.KindMethod, RepoPrefix: "repo", FilePath: "z.go"}
	old := &graph.Edge{From: "a", To: "z", Kind: graph.EdgeSimilarTo, FilePath: "a.go", Line: 3, Origin: "old", Confidence: 0.5}
	otherSite := *old
	otherSite.Line = 7
	seed := []*graph.Node{a, z}
	b.AddBatch(seed, []*graph.Edge{old, &otherSite})
	shingles := map[string][]uint64{"a": {7, 1, 7}}
	require.NoError(t, b.BulkSetCloneShingles("repo", shingles))
	require.NoError(t, b.Publish(ctx))

	readRawShingles := func() map[string][]byte {
		rows := make(map[string][]byte)
		err := pendingVisit(ctx, b.pending, []byte{familyFileMeta, shingleRows}, func(r row) error {
			rows[string(r.key)] = bytes.Clone(r.value)
			return nil
		})
		require.NoError(t, err)
		return rows
	}
	raw := readRawShingles()
	require.Len(t, raw, 1)

	stamped := *a
	stamped.Meta = map[string]any{"clone_sig": "new-signature"}
	replacement := *old
	replacement.Origin, replacement.Confidence = "fresh", 0.95
	reverse := &graph.Edge{From: "z", To: "a", Kind: graph.EdgeSimilarTo, Confidence: 0.95}
	semantic := &graph.Edge{From: "a", To: "z", Kind: graph.EdgeSemanticallyRelated, FilePath: "a.go", Line: 3, Confidence: 0.8}
	v := b.RebuildView()
	err := b.ApplyRebuildCloneRelations(ctx, func(node func(*graph.Node) error, edge func(*graph.Edge) error) error {
		require.NoError(t, node(&stamped))
		for _, e := range []*graph.Edge{&replacement, reverse, semantic} {
			require.NoError(t, edge(e))
		}
		// A fresh working read still sees only the rows present before staging.
		requireRebuildCloneRows(t, v, seed, old, &otherSite)
		return nil
	})
	require.NoError(t, err)
	requireRebuildCloneRows(t, v, []*graph.Node{&stamped, z}, &replacement, &otherSite, reverse, semantic)
	require.Equal(t, raw, readRawShingles())

	// Open a new snapshot so an accidental publish cannot hide behind a pinned reader.
	published := snapshot(t, b)
	require.ElementsMatch(t, seed, published.AllNodes())
	require.ElementsMatch(t, []*graph.Edge{old, &otherSite}, published.AllEdges())
	publishedShingles, err := published.LoadCloneShingles("repo")
	require.NoError(t, err)
	require.Equal(t, shingles, publishedShingles)
}

func TestRebuildCloneApplyRejectsBeforeWrites(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	a := &graph.Node{ID: "a", Kind: graph.KindFunction}
	z := &graph.Node{ID: "z", Kind: graph.KindFunction}
	seed := []*graph.Node{a, z}
	b.AddBatch(seed, nil)
	stamped := *a
	stamped.Meta = map[string]any{"clone_sig": "abandoned"}
	abandoned := &graph.Edge{From: "a", To: "z", Kind: graph.EdgeSimilarTo}
	bad := *abandoned
	bad.Confidence = math.NaN()
	rejected := errors.New("late clone producer failure")
	v := b.RebuildView()
	for _, tc := range []struct {
		name string
		want error
	}{
		{"producer", rejected},
		{"staging", errInvalidRecord},
		{"canceled", context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempt, cancel := context.WithCancel(t.Context())
			defer cancel()
			err := b.ApplyRebuildCloneRelations(attempt, func(node func(*graph.Node) error, edge func(*graph.Edge) error) error {
				require.NoError(t, node(&stamped))
				require.NoError(t, edge(abandoned))
				switch tc.name {
				case "staging":
					return edge(&bad)
				case "canceled":
					cancel()
					return nil
				default:
					return rejected
				}
			})
			require.ErrorIs(t, err, tc.want)
			requireRebuildCloneRows(t, v, seed)
			occupied, err := rebuildGuardHasPrefix(t.Context(), b.pending, []byte{'g', 'C'})
			require.NoError(t, err)
			require.False(t, occupied, "rejected clone output left scratch rows")
		})
	}

	// Neither abandoned key is restaged. A retry must not apply either row.
	retryNode := *z
	retryNode.Meta = map[string]any{"clone_sig": "retry"}
	retryEdge := &graph.Edge{From: "z", To: "a", Kind: graph.EdgeSimilarTo}
	err := b.ApplyRebuildCloneRelations(ctx, func(node func(*graph.Node) error, edge func(*graph.Edge) error) error {
		require.NoError(t, node(&retryNode))
		return edge(retryEdge)
	})
	require.NoError(t, err)
	requireRebuildCloneRows(t, v, []*graph.Node{a, &retryNode}, retryEdge)
}

func TestRebuildCloneApplyWriteFailureRefusesPublish(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	a := &graph.Node{ID: "a", Kind: graph.KindFunction}
	z := &graph.Node{ID: "z", Kind: graph.KindFunction}
	old := &graph.Edge{From: "a", To: "z", Kind: graph.EdgeSimilarTo, Origin: "old"}
	b.AddBatch([]*graph.Node{a, z}, []*graph.Edge{old})
	require.NoError(t, b.Publish(ctx))

	// Changing an edge's origin writes the edge before updating its revision count.
	require.NoError(t, b.pending.Set(revisionKey, []byte{0xff}, pebble.NoSync))
	replacement := *old
	replacement.Origin = "fresh"
	err := b.ApplyRebuildCloneRelations(ctx, func(_ func(*graph.Node) error, edge func(*graph.Edge) error) error {
		return edge(&replacement)
	})
	require.ErrorIs(t, err, errInvalidRecord)
	// Remove the fault, so only the recorded mutation failure can block publish.
	require.NoError(t, b.pending.Delete(revisionKey, pebble.NoSync))
	require.ErrorIs(t, b.Publish(ctx), errInvalidRecord)
	require.ElementsMatch(t, []*graph.Edge{old}, snapshot(t, b).AllEdges())
	err = b.RebuildView().VisitEdges(ctx, func(e *graph.Edge) error {
		t.Fatalf("failed build exposed a working edge: %+v", e)
		return nil
	})
	require.ErrorIs(t, err, errInvalidRecord)
}

func requireRebuildCloneRows(t *testing.T, v RebuildView, wantNodes []*graph.Node, wantEdges ...*graph.Edge) {
	t.Helper()
	var nodes []*graph.Node
	require.NoError(t, v.VisitNodes(t.Context(), func(n *graph.Node) error {
		nodes = append(nodes, n)
		return nil
	}))
	require.ElementsMatch(t, wantNodes, nodes)
	var edges []*graph.Edge
	require.NoError(t, v.VisitEdges(t.Context(), func(e *graph.Edge) error {
		edges = append(edges, e)
		return nil
	}))
	require.ElementsMatch(t, wantEdges, edges)
}

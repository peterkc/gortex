package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildCloneShadowedReceiptEdges(t *testing.T) {
	shingles := []uint64{1, 3, 5, 7, 11, 13, 17, 19}
	a := rebuildCloneInputFixture(rebuildCloneInput{ID: "a.go::F", FilePath: "a.go", Language: "go", Line: 3, Tokens: 80, Shingles: shingles}, "go")
	other := rebuildCloneInputFixture(rebuildCloneInput{ID: "a.go::G", FilePath: "a.go", Language: "go", Line: 9, Tokens: 80, Shingles: shingles}, "go")
	a.Nodes = append(a.Nodes, other.Nodes[0])
	a.File.NodeCount++
	a.Shingles[other.Nodes[0].ID] = other.Shingles[other.Nodes[0].ID]

	tagged := &graph.Edge{
		From: a.Nodes[0].ID, Kind: graph.EdgeCalls, To: "unresolved::grpc::Inventory::Get",
		FilePath: "a.go", Line: 4, Origin: "tagged",
		Meta: map[string]any{"via": "grpc.stub", "grpc_service": "Inventory", "grpc_method": "Get"},
	}
	untagged := *tagged
	untagged.Origin, untagged.Meta = "untagged", nil
	// The other receipts claim the recording site in a.go without claiming its nodes.
	a.Edges = []*graph.Edge{tagged}
	m := store_pebble.ParseFacts{
		EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: "m.go"}, Edges: []*graph.Edge{tagged},
	}
	z := store_pebble.ParseFacts{
		EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: "z.go"}, Edges: []*graph.Edge{&untagged},
	}
	build, _ := openRebuildTestFileStore(t)
	receipts := seedRebuildCloneInputReceipts(t, build, a, m, z)
	passes, read := 0, 0
	replay := func(yield func(store_pebble.ParseFacts, error) bool) {
		passes++
		for facts, err := range receipts {
			read++
			if !yield(facts, err) {
				return
			}
		}
	}

	_, err := applyRebuildCloneRelations(t.Context(), build, replay, rebuildClonePredecessors{}, 0.82)
	require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
	reply := buildRefusal(err)
	require.EqualValues(t, 36, reply.Refusal)
	require.EqualValues(t, 1, reply.UnknownProvenance)
	require.Contains(t, reply.Detail, "shadowed_id=0 shadowed_edge=1")
	require.Equal(t, 1, passes)
	require.Equal(t, 3, read)
	want := []*graph.Edge{tagged}
	requireCloneParseEdgesMatchSQLite(t, want, a, m, z)
	requireRebuildTestFileRows(t, build, a.Nodes, want)
	got := make(map[string][]uint64)
	require.NoError(t, build.RebuildView().VisitCloneShingles(t.Context(), "", func(id string, values []uint64) error {
		got[id] = values
		return nil
	}))
	require.Equal(t, a.Shingles, got)
}

func TestRebuildCloneReceiptEdgesKeepDistinctSitesAndFirstPayload(t *testing.T) {
	n := &graph.Node{ID: "a.go::F", Kind: graph.KindFunction, Name: "F", FilePath: "a.go", Language: "go", StartLine: 3, EndLine: 6}
	first := &graph.Edge{From: n.ID, Kind: graph.EdgeCalls, To: "unresolved::grpc::Inventory::Get", FilePath: "a.go", Line: 4}
	pathOnly, lineOnly := *first, *first
	pathOnly.FilePath = "z.go"
	lineOnly.Line = 5
	last := lineOnly
	last.Origin = "last"
	last.Meta = map[string]any{"via": "grpc.stub"}
	a := rebuildCloneClaimFixture("a.go", n)
	a.Edges = []*graph.Edge{first}
	z := store_pebble.ParseFacts{
		EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: "z.go"},
		Edges: []*graph.Edge{&pathOnly, &lineOnly, &last},
	}
	build, _ := openRebuildTestFileStore(t)
	receipts := seedRebuildCloneInputReceipts(t, build, a, z)
	passes, read := 0, 0
	replay := func(yield func(store_pebble.ParseFacts, error) bool) {
		passes++
		for facts, err := range receipts {
			read++
			if !yield(facts, err) {
				return
			}
		}
	}

	_, err := applyRebuildCloneRelations(t.Context(), build, replay, rebuildClonePredecessors{}, 0.82)
	require.NoError(t, err)
	require.Equal(t, 1, passes, "the empty clone corpus needs only the first receipt pass")
	require.Equal(t, 2, read)
	want := []*graph.Edge{first, &lineOnly, &pathOnly}
	requireCloneParseEdgesMatchSQLite(t, want, a, z)
	requireRebuildTestFileRows(t, build, []*graph.Node{n}, want)
}

func requireCloneParseEdgesMatchSQLite(t *testing.T, want []*graph.Edge, facts ...store_pebble.ParseFacts) {
	t.Helper()
	native := newFTSStore(t)
	for _, receipt := range facts {
		require.NoError(t, native.AddBatchChecked(receipt.Nodes, receipt.Edges))
	}
	require.ElementsMatch(t, want, native.AllEdges())
}

package indexer

import (
	"errors"
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/clones"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildCloneNodeStampsMatchNative(t *testing.T) {
	facts := rebuildCloneNodeStampFixtures()
	build, _ := openRebuildTestFileStore(t)
	receipts := seedRebuildCloneInputReceipts(t, build, facts...)
	view := build.RebuildView()

	// The replacement must preserve live fields that were not in the receipt.
	live := facts[1].Nodes[0]
	live.Origin = "working-origin"
	live.Meta["test_labels"] = []string{"working", "kept"}
	build.AddNode(live)
	requireRebuildCloneNodeStampInputs(t, view, facts)

	previous := cmsMinCorpus
	t.Cleanup(func() { cmsMinCorpus = previous })
	cmsMinCorpus = len(facts)
	oracle := graph.New()
	for _, f := range facts {
		n := *f.Nodes[0]
		n.Meta = maps.Clone(n.Meta)
		oracle.AddNode(&n)
	}
	items, corpus := finaliseCloneSignaturesFromNodes(oracle, "")
	require.Equal(t, len(facts), corpus)
	require.Len(t, items, 1)
	require.Equal(t, live.ID, items[0].ID)

	var got []*graph.Node
	counts, err := visitRebuildCloneNodeStamps(t.Context(), build, receipts, func(n *graph.Node) error {
		got = append(got, n)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, rebuildCloneSignatureCounts{Corpus: 3, Kept: 1, Claims: store_pebble.RebuildShadowedIDStats{ScratchBytes: 57, PeakRowBytes: 19}}, counts)
	want := []*graph.Node{oracle.GetNode("a.go::F"), oracle.GetNode("b.go::F"), oracle.GetNode("c.rs::F")}
	require.ElementsMatch(t, want, got, "full native rows, including dropped bodies")
	for _, n := range got {
		require.NotContains(t, n.Meta, cloneShinglesMetaKey)
		if n.ID == live.ID {
			require.NotEqual(t, live.Meta[cloneSigMetaKey], n.Meta[cloneSigMetaKey], "replace the stale signature")
		} else {
			require.NotContains(t, n.Meta, cloneSigMetaKey, "drop the stale signature for %s", n.ID)
		}
		n.Name = "changed by sink"
		n.Meta["test_labels"].([]string)[0] = "changed by sink"
	}
	requireRebuildCloneNodeStampInputs(t, view, facts)
}

func TestRebuildCloneNodeStampsPropagateSinkError(t *testing.T) {
	facts := rebuildCloneNodeStampFixtures()
	build, _ := openRebuildTestFileStore(t)
	receipts := seedRebuildCloneInputReceipts(t, build, facts...)
	view := build.RebuildView()
	sinkErr := errors.New("node stamp sink failed")
	calls := 0
	counts, err := visitRebuildCloneNodeStamps(t.Context(), build, receipts, func(n *graph.Node) error {
		calls++
		if calls == 2 {
			n.Meta["test_labels"].([]string)[0] = "changed by failing sink"
			return sinkErr
		}
		return nil
	})
	require.ErrorIs(t, err, sinkErr)
	require.Zero(t, counts, "an earlier successful callback does not commit counts")
	require.Equal(t, 2, calls, "stop at the sink error")
	requireRebuildCloneNodeStampInputs(t, view, facts)
}

func TestRebuildCloneNodeStampsRejectLiveMetadataDrift(t *testing.T) {
	for _, key := range []string{cloneShinglesMetaKey, cloneTokensMetaKey} {
		t.Run(key, func(t *testing.T) {
			facts := rebuildCloneNodeStampFixtures()
			build, _ := openRebuildTestFileStore(t)
			receipts := seedRebuildCloneInputReceipts(t, build, facts...)
			view := build.RebuildView()

			changed := facts[1].Nodes[0]
			if key == cloneShinglesMetaKey {
				changed.Meta[key] = []uint64{20, 22, 23, 24, 25, 26, 27, 28, 29}
			} else {
				changed.Meta[key] = 81
			}
			build.AddNode(changed)
			requireRebuildCloneNodeStampInputs(t, view, facts)
			require.NoError(t, visitRebuildCloneInputs(t.Context(), view, receipts, func(rebuildCloneInput) error { return nil }),
				"the receipt and the sidecar still agree; only the live metadata differs")

			counts, err := visitRebuildCloneNodeStamps(t.Context(), build, receipts, func(n *graph.Node) error {
				require.NotEqual(t, changed.ID, n.ID, "do not yield the inconsistent node")
				return nil
			})
			require.ErrorIs(t, err, store_pebble.ErrRebuildView)
			require.Zero(t, counts)
			requireRebuildCloneNodeStampInputs(t, view, facts)
		})
	}
}

func rebuildCloneNodeStampFixtures() []store_pebble.ParseFacts {
	// The Rust body shares one hash with each Go body. At the CMS floor,
	// only b.go retains eight hashes; a.go retains seven and Rust five.
	inputs := []rebuildCloneInput{
		{ID: "a.go::F", FilePath: "a.go", Language: "go", Line: 3, Tokens: 79, Shingles: []uint64{1, 2, 3, 4, 5, 6, 7, 8}},
		{ID: "b.go::F", FilePath: "b.go", Language: "go", Line: 7, Tokens: 80, Shingles: []uint64{21, 22, 23, 24, 25, 26, 27, 28, 29}},
		{ID: "c.rs::F", FilePath: "c.rs", Language: "rust", Line: 11, Tokens: 97, Shingles: []uint64{1, 21, 101, 102, 103, 104, 105}},
	}
	var facts []store_pebble.ParseFacts
	for _, input := range inputs {
		f := rebuildCloneInputFixture(input, input.Language)
		n := f.Nodes[0]
		n.QualName = "fixture.F"
		n.StartColumn, n.EndColumn = 2, 5
		n.Meta[cloneSigMetaKey] = clones.EncodeSignature(clones.Signature{1})
		n.Meta["test_labels"] = []string{"receipt", input.Language}
		facts = append(facts, f)
	}
	return facts
}

func requireRebuildCloneNodeStampInputs(t *testing.T, view store_pebble.RebuildView, facts []store_pebble.ParseFacts) {
	t.Helper()
	for _, f := range facts {
		want := f.Nodes[0]
		got, err := view.Node(t.Context(), want.ID)
		require.NoError(t, err)
		require.Equal(t, want, got, "saved node %s", want.ID)
		require.NoError(t, view.CheckRebuildCloneInput(t.Context(), want, f.Shingles[want.ID]), "saved sidecar %s", want.ID)
	}
}

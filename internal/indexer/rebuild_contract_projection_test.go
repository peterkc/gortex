package indexer

import (
	"iter"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestDeferredContractLanguagesRefusesIncompleteCensus(t *testing.T) {
	for _, census := range []*rebuildExtractorCensus{nil, {},
		{byPath: map[string]string{"a.go": "go"}, originByGraphPath: map[string]string{}, admitted: 2},
		{byPath: map[string]string{"a.go": ""}, originByGraphPath: map[string]string{}, admitted: 1},
	} {
		r := rebuildContractReceiptReader{census: census}
		_, err := r.deferredContractLanguages()
		require.ErrorIs(t, err, graph.ErrContractProjectionStale)
	}
}

func TestSelectRebuildContractRecordRefusesChangedOrUnprovedOwner(t *testing.T) {
	ctx := t.Context()
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	r := rebuildContractReceiptReader{census: &rebuildExtractorCensus{
		byPath: map[string]string{"a.py": "python"}, originByGraphPath: map[string]string{"a.py": "a.py"},
	}}
	c := store_pebble.RebuildContractRecord{Contract: contracts.Contract{ID: "env::KEY", Type: contracts.ContractEnv, Role: contracts.RoleConsumer, FilePath: "a.py"}}
	view := b.RebuildView()
	for _, owner := range []string{"missing", "foreign"} {
		b.AddNode(&graph.Node{ID: "foreign", RepoPrefix: "other", FilePath: "a.py", Kind: graph.KindFile})
		keep, err := r.selectRebuildContractRecord(ctx, view, c, owner)
		require.ErrorIs(t, err, graph.ErrContractProjectionStale)
		require.False(t, keep)
	}
	b.AddNode(&graph.Node{ID: "moved", FilePath: "other.py", Kind: graph.KindFunction})
	for _, owner := range []string{"", "moved"} {
		keep, err := r.selectRebuildContractRecord(ctx, view, c, owner)
		require.NoError(t, err)
		require.False(t, keep)
	}
	c.FilePath = "absent.py"
	keep, err := r.selectRebuildContractRecord(ctx, view, c, "")
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)
	require.False(t, keep)
}

func TestRebuildContractProducerRefusesDeferredLanguageMismatch(t *testing.T) {
	ctx := t.Context()
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	idx := newTestIndexer(graph.New())
	t.Cleanup(idx.Close)
	r := rebuildContractReceiptReader{idx: idx, census: &rebuildExtractorCensus{
		byPath: map[string]string{}, originByGraphPath: map[string]string{},
	}}
	err = r.withRebuildContractProducer(ctx, b, func(func(contracts.Contract) error) error { return nil }, make(rebuildEarlyContractIDs),
		func(_ iter.Seq2[store_pebble.RebuildContractBatch, error], seal func(string, store_pebble.RelationsBase) (store_pebble.StageTables, error)) error {
			stage, err := seal(t.TempDir(), store_pebble.RelationsBase{})
			require.Zero(t, stage)
			return err
		})
	require.ErrorContains(t, err, "contract selection differs from retained relation coverage")
}

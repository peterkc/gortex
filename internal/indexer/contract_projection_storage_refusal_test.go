package indexer

import (
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"testing"
)

type readOnlyProjectionFixture struct {
	graphview.GenerationLayerHandle
}

func TestContractProjectionRefusesLayerWithoutPayloadCapabilities(t *testing.T) {
	store := builderOpenStoreAt(t, t.TempDir()+"/projection.sqlite")
	t.Cleanup(func() { _ = store.Close() })
	native := store.AtGeneration(1)
	layer, err := graphview.NewGenerationLayer(readOnlyProjectionFixture{GenerationLayerHandle: native})
	require.NoError(t, err)
	for name, selected := range map[string]*graphview.GenerationLayer{"missing_payload_reads": layer, "empty_layer": {}} {
		t.Run(name, func(t *testing.T) {
			reader := graph.NewOverlaidViewWithLayer(graph.New(), selected)
			chain, ok := contractPositiveChain(reader, 0)
			require.False(t, ok)
			require.Nil(t, chain)
			require.False(t, contractInventoryCompositionCovered(reader, []generationPayloadStore{native}))
			prefix, err := positivePrefixReader(t.Context(), reader)
			require.ErrorIs(t, err, graph.ErrContractProjectionIncomplete)
			require.Nil(t, prefix)
		})
	}
}

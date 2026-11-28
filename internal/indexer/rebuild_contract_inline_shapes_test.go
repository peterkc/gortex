package indexer

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildInlineContractShapesMatchNative(t *testing.T) {
	const firstID, secondID = "types.go::First", "types.go::Second"
	const providerID, consumerID = "http::GET::/a", "http::GET::/b"
	first := &contracts.Shape{Kind: "struct", Fields: []contracts.ShapeField{{Name: "A", Type: "string"}}}
	second := &contracts.Shape{Kind: "struct", Fields: []contracts.ShapeField{{Name: "B", Type: "int"}}}
	shapeMap := map[string]*contracts.Shape{firstID: first, secondID: second}
	g := graph.New()
	for id, shape := range shapeMap {
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindType, Meta: map[string]any{"shape": shape}})
	}
	idx := newTestIndexer(g)
	t.Cleanup(idx.Close)
	provider := func(file, typeID string, line int) contracts.Contract {
		return contracts.Contract{ID: providerID, Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
			FilePath: file, SymbolID: "handler.go::Serve", Line: line, Meta: map[string]any{
				"response_type": typeID, "request_type": typeID,
				"response_envelope": []map[string]any{
					{"name": "ref", "type": typeID},
					{"name": "explicit", "type": secondID, "shape": nil},
					{"name": "absent", "type": "types.go::Missing"},
					{"name": "bare", "type": "string"},
				},
			}}
	}
	consumer := func(line int) contracts.Contract {
		c := provider("consumer.go", secondID, line)
		c.ID, c.Role = consumerID, contracts.RoleConsumer
		return c
	}
	inputs := []contracts.Contract{provider("a.go", firstID, 2), provider("a.go", secondID, 4), provider("b.go", secondID, 3), consumer(1), consumer(2)}
	native := contracts.NewRegistry()
	for _, c := range cloneContractRecords(inputs) {
		native.Add(c)
	}
	idx.inlineEnvelopeShapes(native)
	for _, c := range native.ByID(providerID) {
		require.Equal(t, first, c.Meta["response_shape"])
		require.Equal(t, first, c.Meta["request_shape"])
	}
	consumers := native.ByID(consumerID)
	require.Equal(t, second, consumers[0].Meta["response_envelope"].([]map[string]any)[0]["shape"])
	require.NotContains(t, consumers[1].Meta["response_envelope"].([]map[string]any)[0], "shape")
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	require.NoError(t, b.WithRebuildContractRecords(t.Context(), func(records *store_pebble.RebuildContractRecords) error {
		for i, c := range inputs {
			require.NoError(t, records.Add(store_pebble.RebuildContractRecord{Contract: c, BodyUnproved: i == 1}))
		}
		require.NoError(t, inlineRebuildContractShapes(records, func(id string) (*contracts.Shape, error) {
			require.NotEqual(t, "string", id)
			return shapeMap[id], nil
		}))
		for _, id := range []string{providerID, consumerID} {
			var actual []contracts.Contract
			require.NoError(t, records.VisitByID(id, func(item store_pebble.RebuildContractRecord) error {
				actual = append(actual, item.Contract)
				require.Equal(t, id == providerID && item.Line == 4, item.BodyUnproved)
				return nil
			}))
			require.Equal(t, native.ByID(id), actual)
		}
		return nil
	}))
	stop := errors.New("cannot load checked shape")
	require.ErrorIs(t, b.WithRebuildContractRecords(t.Context(), func(records *store_pebble.RebuildContractRecords) error {
		require.NoError(t, records.Add(store_pebble.RebuildContractRecord{Contract: inputs[0]}))
		return inlineRebuildContractShapes(records, func(string) (*contracts.Shape, error) { return nil, stop })
	}), stop)
}

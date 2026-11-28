package indexer

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildInlineContractShapesUnprovedRepresentative(t *testing.T) {
	const id, symbol, stale = "http::GET::/a", "handler.go::Serve", "types.go::Stale"
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	staleResponse := map[string]any{"response_type": stale}
	staleEnvelope := map[string]any{"response_envelope": []map[string]any{{"name": "stale", "type": stale}}}
	for _, tc := range []struct {
		name      string
		meta      map[string]any
		reachable bool
	}{
		{"response", staleResponse, true},
		{"request", map[string]any{"request_type": stale}, true},
		{"envelope", staleEnvelope, true},
		{"missing stale type", nil, true},
		{"no proved write target", staleEnvelope, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proved := store_pebble.RebuildContractRecord{Contract: contracts.Contract{
				ID:       id,
				Type:     contracts.ContractHTTP,
				Role:     contracts.RoleProvider,
				FilePath: "routes.go",
				SymbolID: symbol,
				Line:     2,
				Meta:     map[string]any{"response_shape": &contracts.Shape{Kind: "struct"}},
			}}
			// The first row represents the hidden sibling in All.
			unproved := proved
			unproved.Line, unproved.Meta, unproved.BodyUnproved = 1, tc.meta, true
			otherSymbol := proved
			otherSymbol.SymbolID = "handler.go::Other"
			consumer := proved
			consumer.Role = contracts.RoleConsumer
			inputs := []store_pebble.RebuildContractRecord{unproved, otherSymbol, consumer}
			want := []store_pebble.RebuildContractRecord{unproved, otherSymbol, consumer}
			if tc.reachable {
				otherFile := proved
				otherFile.FilePath = "other.go"
				inputs = append(inputs, proved, otherFile)
				proved.BodyUnproved, otherFile.BodyUnproved = true, true
				want = append(want, proved, otherFile)
			}
			require.NoError(t, b.WithRebuildContractRecords(t.Context(), func(records *store_pebble.RebuildContractRecords) error {
				for _, item := range inputs {
					require.NoError(t, records.Add(item))
				}
				lookups := 0
				require.NoError(t, inlineRebuildContractShapes(records, func(string) (*contracts.Shape, error) {
					lookups++
					return &contracts.Shape{Kind: "struct"}, nil
				}))
				require.Zero(t, lookups)
				var got []store_pebble.RebuildContractRecord
				require.NoError(t, records.VisitByID(id, func(item store_pebble.RebuildContractRecord) error {
					got = append(got, item)
					return nil
				}))
				require.Equal(t, want, got)
				return nil
			}))
		})
	}
}

func TestRebuildInlineContractShapesCompletenessGuard(t *testing.T) {
	const id, typeID = "http::GET::/a", "types.go::Response"
	const stale, missing = "types.go::Stale", "types.go::Missing"
	shape := &contracts.Shape{Kind: "struct", Fields: []contracts.ShapeField{{Name: "Value", Type: "string"}}}
	for _, tc := range []struct {
		name              string
		missingField      string
		incompleteRow     bool
		consumer          bool
		wantFirstUnproved bool
	}{
		{name: "complete"},
		{name: "missing request shape", missingField: "request_type", wantFirstUnproved: true},
		{name: "missing response shape", missingField: "response_type", wantFirstUnproved: true},
		{name: "missing envelope", missingField: "response_envelope", wantFirstUnproved: true},
		{name: "incomplete envelope", incompleteRow: true, wantFirstUnproved: true},
		{name: "unproved consumer with complete shapes", consumer: true, wantFirstUnproved: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := contracts.Contract{
				ID: id, Type: contracts.ContractHTTP, Role: contracts.RoleProvider, FilePath: "a.go", SymbolID: "handler.go::Serve",
				Meta: map[string]any{
					"response_type": typeID, "request_type": typeID,
					"response_envelope": []map[string]any{{"name": "value", "type": typeID}},
				},
			}
			if tc.missingField != "" {
				delete(first.Meta, tc.missingField)
			}
			if tc.incompleteRow {
				first.Meta["response_envelope"] = []map[string]any{
					{"name": "value", "type": typeID},
					{"name": "missing", "type": missing},
				}
			}
			later := first
			later.FilePath = "b.go"
			later.Meta = map[string]any{"response_type": stale}
			if tc.consumer {
				// A non-provider must not borrow the provider-only proof, even
				// if its input metadata already carries a full set of shapes.
				later.Role = contracts.RoleConsumer
				later.Meta["request_shape"] = shape
				later.Meta["response_shape"] = shape
				later.Meta["response_envelope"] = []map[string]any{{"name": "value", "type": typeID, "shape": shape}}
			}
			inputs := []contracts.Contract{first, later}
			native := contracts.NewRegistry()
			for _, c := range cloneContractRecords(inputs) {
				native.Add(c)
			}
			g := graph.New()
			g.AddNode(&graph.Node{ID: typeID, Kind: graph.KindType, Meta: map[string]any{"shape": shape}})
			idx := newTestIndexer(g)
			t.Cleanup(idx.Close)
			idx.inlineEnvelopeShapes(native)
			want := native.ByID(id)
			require.Len(t, want, 2)
			if !tc.wantFirstUnproved {
				for _, row := range want {
					require.Equal(t, shape, row.Meta["request_shape"])
					require.Equal(t, shape, row.Meta["response_shape"])
					require.Equal(t, shape, row.Meta["response_envelope"].([]map[string]any)[0]["shape"])
				}
			}

			b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, b.Close()) })
			require.NoError(t, b.WithRebuildContractRecords(t.Context(), func(records *store_pebble.RebuildContractRecords) error {
				for i, c := range inputs {
					require.NoError(t, records.Add(store_pebble.RebuildContractRecord{Contract: c, BodyUnproved: i == 1}))
				}
				require.NoError(t, inlineRebuildContractShapes(records, func(id string) (*contracts.Shape, error) {
					require.NotEqual(t, stale, id, "must not read the unproved body")
					if tc.incompleteRow && id == missing {
						return nil, nil
					}
					require.Equal(t, typeID, id)
					return shape, nil
				}))
				var got []contracts.Contract
				require.NoError(t, records.VisitByID(id, func(item store_pebble.RebuildContractRecord) error {
					require.Equal(t, tc.wantFirstUnproved || item.FilePath == later.FilePath, item.BodyUnproved, item.FilePath)
					got = append(got, item.Contract)
					return nil
				}))
				require.Equal(t, want, got)
				return nil
			}))
		})
	}
}

func TestRebuildInlineContractShapesEnvelopeTargets(t *testing.T) {
	const id, typeID = "http::GET::/a", "types.go::Response"
	shape := &contracts.Shape{Kind: "struct"}
	envelope := func(name string, shape *contracts.Shape) map[string]any {
		row := map[string]any{"name": name, "type": typeID}
		if shape != nil {
			row["shape"] = shape
		}
		return map[string]any{"response_envelope": []map[string]any{row}}
	}
	first := contracts.Contract{
		ID:       id,
		Type:     contracts.ContractHTTP,
		Role:     contracts.RoleConsumer,
		FilePath: "a.go",
		SymbolID: "handler.go::Serve",
		Line:     1,
	}
	provider := first
	provider.Role, provider.Line, provider.Meta = contracts.RoleProvider, 2, envelope("provider", nil)
	hidden := provider
	hidden.Line, hidden.Meta = 3, nil
	last := first
	last.FilePath, last.Line, last.Meta = "b.go", 4, envelope("consumer", nil)
	inputs := []contracts.Contract{first, provider, hidden, last}
	want := []contracts.Contract{first, provider, hidden, last}
	for i := 1; i < len(want); i++ {
		want[i].Meta = envelope("consumer", shape)
	}

	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	require.NoError(t, b.WithRebuildContractRecords(t.Context(), func(records *store_pebble.RebuildContractRecords) error {
		for _, c := range inputs {
			require.NoError(t, records.Add(store_pebble.RebuildContractRecord{Contract: c}))
		}
		require.NoError(t, inlineRebuildContractShapes(records, func(id string) (*contracts.Shape, error) {
			require.Equal(t, typeID, id)
			return shape, nil
		}))
		var got []contracts.Contract
		require.NoError(t, records.VisitByID(id, func(item store_pebble.RebuildContractRecord) error {
			got = append(got, item.Contract)
			return nil
		}))
		require.Equal(t, want, got)
		return nil
	}))
}

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

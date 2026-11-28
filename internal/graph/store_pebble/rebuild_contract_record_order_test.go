package store_pebble

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
)

func TestRebuildOrderedRepresentativesPreserveElection(t *testing.T) {
	b, err := OpenBuild(t.TempDir(), FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	record := func(id, path, symbol string, role contracts.Role, line int) RebuildContractRecord {
		return RebuildContractRecord{Contract: contracts.Contract{ID: id, FilePath: path, SymbolID: symbol, Role: role, Line: line}}
	}
	const id = "http::GET::/items"
	later := record(id, "z.go", "a", contracts.RoleProvider, 1)
	provider := record(id, "a.go", "b", contracts.RoleProvider, 44)
	provider.BodyUnproved = true
	hidden := record(id, "a.go", "b", contracts.RoleProvider, 3)
	consumer := record(id, "a.go", "b", contracts.RoleConsumer, 46)
	earlierSymbol := record(id, "a.go", "a", contracts.RoleProvider, 47)
	other := record("http::GET::/other", "a.go", "a", contracts.RoleProvider, 1)
	stop := errors.New("stop reading contract records")
	const empty = "http::GET::/empty"
	require.NoError(t, b.WithRebuildContractRecords(t.Context(), func(r *RebuildContractRecords) error {
		for _, item := range []RebuildContractRecord{other, later, provider, hidden, consumer, earlierSymbol} {
			require.NoError(t, r.Add(item))
		}
		require.NoError(t, r.ClearID(empty))
		var readAfter func(func(RebuildContractRecord) error) error
		require.ErrorIs(t, r.WithOrderedRepresentatives(func(read func(func(RebuildContractRecord) error) error) error {
			readAfter = read
			return read(func(item RebuildContractRecord) error {
				require.Equal(t, earlierSymbol, item)
				return stop
			})
		}), stop)
		require.Error(t, readAfter(func(RebuildContractRecord) error { return nil }))
		require.NoError(t, r.WithOrderedRepresentatives(func(read func(func(RebuildContractRecord) error) error) error {
			for range 2 {
				var actual []RebuildContractRecord
				require.NoError(t, read(func(item RebuildContractRecord) error {
					actual = append(actual, item)
					return nil
				}))
				require.Equal(t, []RebuildContractRecord{earlierSymbol, consumer, provider, later, other}, actual)
			}
			return nil
		}))
		var ids []string
		require.NoError(t, r.VisitIDs(func(id string) error {
			ids = append(ids, id)
			return nil
		}))
		require.Equal(t, []string{empty, id, other.ID}, ids)
		return nil
	}))
}

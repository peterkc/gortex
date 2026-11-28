package store_pebble

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildContractRecordsKeepLiveAndRawBucketViews(t *testing.T) {
	ctx := t.Context()
	b, err := OpenBuild(t.TempDir(), FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	newRecord := func(id, symbol string, line int) RebuildContractRecord {
		return RebuildContractRecord{Contract: contracts.Contract{ID: id, Type: contracts.ContractHTTP,
			Role: contracts.RoleProvider, FilePath: "routes.go", Line: line, SymbolID: symbol,
			Meta: map[string]any{"status_codes": []int{200, 202}}}}
	}
	a, duplicate, z := newRecord("a", "Register", 3), newRecord("a", "Register", 8), newRecord("z", "Register", 10)
	stop := errors.New("stop contract pass")
	var escaped *RebuildContractRecords
	err = b.WithRebuildContractRecords(ctx, func(r *RebuildContractRecords) error {
		escaped = r
		require.Error(t, b.WithRebuildContractRecords(ctx, func(*RebuildContractRecords) error {
			t.Fatal("nested borrower entered the same scratch range")
			return nil
		}))
		for _, item := range []RebuildContractRecord{z, a, duplicate} {
			require.NoError(t, r.Add(item))
		}
		var seen []RebuildContractRecord
		require.NoError(t, r.VisitRepresentatives(func(item RebuildContractRecord) error {
			seen = append(seen, item)
			if item.ID != a.ID {
				return nil
			}
			return r.UpdateByID(z.ID, func(live *RebuildContractRecord) (bool, error) {
				live.SymbolID, live.BodyUnproved = "Linked", true
				return true, nil
			})
		}))
		require.Equal(t, []RebuildContractRecord{a, z}, seen, "representatives read the original rows")
		seen = nil
		require.NoError(t, r.VisitByID(a.ID, func(item RebuildContractRecord) error {
			seen = append(seen, item)
			return nil
		}))
		require.Equal(t, []RebuildContractRecord{a, duplicate}, seen)
		seen = nil
		require.NoError(t, r.VisitByID(z.ID, func(item RebuildContractRecord) error {
			seen = append(seen, item)
			return nil
		}))
		changed := z
		changed.SymbolID, changed.BodyUnproved = "Linked", true
		require.Equal(t, []RebuildContractRecord{changed}, seen)
		require.NoError(t, r.ClearID(a.ID))
		var ids []string
		require.NoError(t, r.VisitIDs(func(id string) error {
			ids = append(ids, id)
			return nil
		}))
		require.Equal(t, []string{"a", "z"}, ids, "the cleared native key persists")
		seen = nil
		require.NoError(t, r.VisitRepresentatives(func(item RebuildContractRecord) error {
			seen = append(seen, item)
			return nil
		}))
		require.Equal(t, []RebuildContractRecord{changed}, seen, "no live record for a")
		require.NoError(t, r.Add(duplicate))
		seen = nil
		require.NoError(t, r.VisitByID(a.ID, func(item RebuildContractRecord) error {
			seen = append(seen, item)
			return nil
		}))
		require.Equal(t, []RebuildContractRecord{duplicate}, seen)
		return stop
	})
	require.ErrorIs(t, err, stop)
	require.Error(t, escaped.Add(a))
	has, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildContractRecordPrefix)
	require.NoError(t, err)
	require.False(t, has, "cleanup owns only this scratch range")
	require.NoError(t, b.WithRebuildContractRecords(ctx, func(r *RebuildContractRecords) error {
		for _, c := range []contracts.Contract{
			{ID: "a", FilePath: "b|c", SymbolID: "S", Role: contracts.RoleProvider},
			{ID: "a|b", FilePath: "c", SymbolID: "S", Role: contracts.RoleProvider},
		} {
			require.NoError(t, r.Add(RebuildContractRecord{Contract: c}))
		}
		require.ErrorIs(t, r.VisitRepresentatives(func(RebuildContractRecord) error { return nil }), graph.ErrContractProjectionStale)
		return nil
	}))
}

package store_pebble

import (
	"iter"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
)

func TestRebuildContractSelectionKeepsIndependentSets(t *testing.T) {
	b, err := OpenBuild(t.TempDir(), FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	collect := func(read iter.Seq2[string, error]) []string {
		var ids []string
		for id, err := range read {
			require.NoError(t, err)
			ids = append(ids, id)
		}
		return ids
	}
	require.NoError(t, b.WithRebuildContractRecords(t.Context(), func(r *RebuildContractRecords) error {
		require.Error(t, r.WithContractSelection(nil))
		for _, id := range []string{"z", "a", "a", "empty"} {
			require.NoError(t, r.Add(RebuildContractRecord{Contract: contracts.Contract{ID: id}}))
		}
		require.NoError(t, r.ClearID("empty"))
		var escaped iter.Seq2[string, error]
		require.NoError(t, r.WithContractSelection(func(s *RebuildContractSelection) error {
			require.Error(t, s.SelectID("z"))
			for id, err := range s.CompleteIDs() {
				require.Empty(t, id)
				require.Error(t, err)
			}
			require.NoError(t, s.FreezeCompleteIDs())
			require.Error(t, s.FreezeCompleteIDs())
			escaped = s.CompleteIDs()
			require.Equal(t, []string{"a", "z"}, collect(s.CompleteIDs()))
			require.Empty(t, collect(s.SelectedIDs()))
			require.NoError(t, s.SelectID("z"))
			require.Error(t, s.SelectID("empty"))
			selected, err := s.Selected("a")
			require.NoError(t, err)
			require.False(t, selected)
			selected, err = s.Selected("z")
			require.NoError(t, err)
			require.True(t, selected)
			for _, id := range []string{"z.go::T", "a.go::T", "a.go::T"} {
				require.NoError(t, s.RememberShapeRef(id))
			}
			require.Error(t, s.RememberShapeRef("bare"))
			require.Equal(t, []string{"a.go::T", "z.go::T"}, collect(s.ShapeRefs()))
			require.Equal(t, []string{"z"}, collect(s.SelectedIDs()))
			require.Equal(t, []string{"a", "z"}, collect(s.CompleteIDs()))
			require.Error(t, r.WithContractSelection(func(*RebuildContractSelection) error { return nil }))
			return nil
		}))
		for id, err := range escaped {
			require.Empty(t, id)
			require.Error(t, err)
		}
		require.NoError(t, r.ClearID("z"))
		require.NoError(t, r.ClearID("a"))
		return r.WithContractSelection(func(s *RebuildContractSelection) error {
			require.NoError(t, s.FreezeCompleteIDs())
			require.Empty(t, collect(s.CompleteIDs()))
			require.Empty(t, collect(s.SelectedIDs()))
			require.Empty(t, collect(s.ShapeRefs()))
			return nil
		})
	}))
}

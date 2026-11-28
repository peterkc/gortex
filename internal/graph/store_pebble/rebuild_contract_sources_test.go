package store_pebble

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/contracts"
)

func TestRebuildContractSourcesSpillReplay(t *testing.T) {
	const count = 80
	ctx, lease := t.Context(), t.TempDir()
	fat := strings.Repeat("shape-input", 1500)
	makeRecord := func(i int) contracts.Contract {
		return contracts.Contract{
			ID: "http::GET::/x", Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
			FilePath: "x.go", SymbolID: "x.go::Handler", Line: count - i, Confidence: 0.9,
			Meta: map[string]any{"note": fat, "status_codes": []int{200, 202}, "envelope": []map[string]any{nil, {}}},
		}
	}
	want := blake3.New()
	for i := range count {
		e := contractFactsEncoder{}
		writeFactRecord(&e, makeRecord(i))
		require.NoError(t, e.err)
		_, err := want.Write(e.buf)
		require.NoError(t, err)
	}
	for _, spillAt := range []int{1, 1 << 20} {
		stats, err := WithRebuildContractSources(ctx, lease, spillAt,
			func(appendRecord func(contracts.Contract) error) error {
				for i := range count {
					if err := appendRecord(makeRecord(i)); err != nil {
						return err
					}
				}
				return nil
			},
			func(read func(func(contracts.Contract) error) error) error {
				for range 2 {
					got := blake3.New()
					var seen int
					err := read(func(c contracts.Contract) error {
						require.Equal(t, makeRecord(seen), c)
						seen++
						e := contractFactsEncoder{}
						writeFactRecord(&e, c)
						if e.err != nil {
							return e.err
						}
						_, err := got.Write(e.buf)
						return err
					})
					if err != nil {
						return err
					}
					require.Equal(t, count, seen)
					require.Equal(t, want.Sum(nil), got.Sum(nil))
				}
				return nil
			})
		require.NoError(t, err)
		require.Equal(t, count, stats.Records)
		require.Positive(t, stats.SpillRuns)
		require.Positive(t, stats.SpillBytes)
		if spillAt == 1 {
			require.Equal(t, count-1, stats.SpillRuns)
			require.Less(t, stats.PeakBatchBytes, len(fat)*2, "at most one record")
		} else {
			require.LessOrEqual(t, stats.PeakBatchBytes, spillAt)
		}
		contents, err := os.ReadDir(lease)
		require.NoError(t, err)
		require.Empty(t, contents)
	}
}

func TestRebuildContractSourcesFailureCleansOwnedScratch(t *testing.T) {
	ctx, lease := t.Context(), t.TempDir()
	keep := filepath.Join(lease, "unowned")
	require.NoError(t, os.WriteFile(keep, []byte("untouched"), 0o600))
	failure := errors.New("test source read failed")
	for _, failDuringCollect := range []bool{true, false} {
		used := false
		stats, err := WithRebuildContractSources(ctx, lease, 1,
			func(add func(contracts.Contract) error) error {
				for _, id := range []string{"env::FIRST", "env::SECOND"} {
					if err := add(contracts.Contract{ID: id}); err != nil {
						return err
					}
				}
				if failDuringCollect {
					return failure
				}
				return nil
			},
			func(read func(func(contracts.Contract) error) error) error {
				used = true
				return read(func(contracts.Contract) error { return failure })
			})
		require.ErrorIs(t, err, failure)
		require.Zero(t, stats)
		require.Equal(t, !failDuringCollect, used)
		contents, err := os.ReadDir(lease)
		require.NoError(t, err)
		require.Len(t, contents, 1)
		require.Equal(t, "unowned", contents[0].Name())
	}
}

func TestRebuildContractSourcesReplayBeyondMergeRunCap(t *testing.T) {
	const count = goldFileCap + 1
	stats, err := WithRebuildContractSources(t.Context(), t.TempDir(), 1,
		func(add func(contracts.Contract) error) error {
			for i := range count {
				if err := add(contracts.Contract{ID: "env::REPEATED", Line: i + 1}); err != nil {
					return err
				}
			}
			return nil
		},
		func(read func(func(contracts.Contract) error) error) error {
			var seen int
			err := read(func(c contracts.Contract) error {
				seen++
				require.Equal(t, seen, c.Line)
				return nil
			})
			require.NoError(t, err)
			require.Equal(t, count, seen)
			return nil
		})
	require.NoError(t, err)
	require.Equal(t, count, stats.Records)
	require.Equal(t, count-1, stats.SpillRuns)
	require.Positive(t, stats.SpillBytes)
}

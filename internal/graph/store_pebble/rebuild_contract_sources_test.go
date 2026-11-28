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

func TestRebuildContractSourcesLostRunRefuses(t *testing.T) {
	ctx, lease := t.Context(), t.TempDir()
	stats, err := WithRebuildContractSources(ctx, lease, 1,
		func(add func(contracts.Contract) error) error {
			if err := add(contracts.Contract{ID: "env::FIRST"}); err != nil {
				return err
			}
			return add(contracts.Contract{ID: "env::SECOND"})
		},
		func(read func(func(contracts.Contract) error) error) error {
			dirs, err := os.ReadDir(lease)
			require.NoError(t, err)
			require.Len(t, dirs, 1)
			require.NoError(t, os.Remove(contractSourceRunPath(filepath.Join(lease, dirs[0].Name()), 0)))
			var seen int
			require.ErrorIs(t, read(func(contracts.Contract) error {
				seen++
				return nil
			}), os.ErrNotExist)
			require.Zero(t, seen)
			return nil // even an unchecked result must not succeed
		})
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Zero(t, stats)
	left, err := os.ReadDir(lease)
	require.NoError(t, err)
	require.Empty(t, left)
}

func TestRebuildContractSourceRunFailures(t *testing.T) {
	for _, mode := range []string{"final write", "replay", "empty", "tail"} {
		t.Run(mode, func(t *testing.T) {
			ctx, lease := t.Context(), t.TempDir()
			var dir string
			stats, err := WithRebuildContractSources(ctx, lease, 1,
				func(add func(contracts.Contract) error) error {
					entries, err := os.ReadDir(lease)
					require.NoError(t, err)
					require.Len(t, entries, 1)
					dir = filepath.Join(lease, entries[0].Name())
					if mode == "empty" {
						return nil
					}
					for i := range 2 {
						require.NoError(t, add(contracts.Contract{ID: "env::A", Line: i + 1}))
					}
					if mode == "final write" {
						return os.Mkdir(contractSourceRunPath(dir, 1), 0o700)
					}
					return nil
				},
				func(read func(func(contracts.Contract) error) error) error {
					require.NotEqual(t, "final write", mode)
					if mode == "replay" {
						require.NoError(t, os.Remove(contractSourceRunPath(dir, 1)))
					}
					var lines []int
					err := read(func(c contracts.Contract) error {
						lines = append(lines, c.Line)
						return nil
					})
					switch mode {
					case "replay":
						require.Error(t, err)
						require.Equal(t, []int{1}, lines)
					case "empty":
						require.NoError(t, err)
						require.Empty(t, lines)
					case "tail":
						require.NoError(t, err)
						require.Equal(t, []int{1, 2}, lines)
					}
					return nil // an ignored replay failure must still abort
				})
			if mode == "final write" || mode == "replay" {
				require.Error(t, err)
				require.Zero(t, stats)
			} else {
				require.NoError(t, err)
				if mode == "tail" {
					require.Equal(t, 2, stats.Records)
					require.Equal(t, 1, stats.SpillRuns)
				} else {
					require.Zero(t, stats)
				}
			}
			entries, err := os.ReadDir(lease)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
}

func TestRebuildContractSourceRecordSizeLimit(t *testing.T) {
	// Lengths in the final range have the same uvarint width.
	c := contracts.Contract{ID: strings.Repeat("a", goldBufferBytes)}
	e := contractFactsEncoder{}
	writeFactRecord(&e, c)
	require.NoError(t, e.err)
	c.ID = c.ID[:len(c.ID)-(len(e.buf)+8+48-goldBufferBytes-1)]
	e = contractFactsEncoder{}
	writeFactRecord(&e, c)
	require.NoError(t, e.err)
	require.Equal(t, goldBufferBytes+1, len(e.buf)+8+48)
	var runs sortedRuns
	var stats RebuildContractSourceStats
	require.ErrorIs(t, appendContractSource(t.Context(), &runs, &stats, c), ErrRebuildLinkInputSize)
	require.Zero(t, stats)
	require.Empty(t, runs.rows)
}

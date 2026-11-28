package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cockroachdb/pebble/v2/sstable"
	"github.com/zzet/gortex/internal/contracts"
)

// RebuildContractSourceStats accounts for the encoded record buffer. SpillBytes
// counts actual table bytes written before the final drain.
type RebuildContractSourceStats struct {
	Records, PeakBatchBytes, SpillRuns int
	SpillBytes                         int64
}

// WithRebuildContractSources freezes the post-parse record stream across main.
// scratchParent is the private worker lease. The collector and the reader are
// serial and valid only inside their respective callbacks. The reader can be
// called more than once, and preserves receipt and record order on each visit.
// The caller discards any tentative result if this call returns an error.
func WithRebuildContractSources(ctx context.Context, scratchParent string, spillAt int,
	collect func(func(contracts.Contract) error) error,
	use func(read func(func(contracts.Contract) error) error) error,
) (stats RebuildContractSourceStats, err error) {
	if !filepath.IsAbs(scratchParent) || spillAt < 1 || collect == nil || use == nil {
		return stats, invalid("contract sources need a private lease, spill threshold and callbacks")
	}
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	dir, err := os.MkdirTemp(scratchParent, "contract-sources-")
	if err != nil {
		return stats, err
	}
	defer func() {
		err = errors.Join(err, os.RemoveAll(dir))
		if err != nil {
			stats = RebuildContractSourceStats{}
		}
	}()
	runs := sortedRuns{dir: dir, options: headOptions(), format: sstable.TableFormatPebblev4, limit: min(spillAt, goldBufferBytes)}
	var captureErr error
	collecting := true
	err = collect(func(c contracts.Contract) error {
		if !collecting {
			return invalid("contract source collector is no longer active")
		}
		if captureErr == nil {
			captureErr = appendContractSource(ctx, &runs, &stats, c)
		}
		return captureErr
	})
	collecting = false
	if err = errors.Join(err, captureErr, ctx.Err()); err != nil {
		return stats, err
	}
	count := stats.SpillRuns
	if len(runs.rows) != 0 {
		if err := runs.writeRun(contractSourceRunPath(dir, count)); err != nil {
			return stats, err
		}
		count++
	}
	runs.rows = nil
	active := true
	defer func() { active = false }()
	err = use(func(yield func(contracts.Contract) error) error {
		if !active || yield == nil {
			return invalid("inactive contract source reader or missing visitor")
		}
		// Ordinals are monotone between runs as well as inside each run. Read
		// one table at a time rather than opening all runs to merge them.
		var next uint64
		for i := range count {
			if err := visitContractSourceRun(ctx, &runs, contractSourceRunPath(dir, i), &next, yield); err != nil {
				return err
			}
		}
		if next != uint64(stats.Records) {
			return invalid("contract source record count changed")
		}
		return ctx.Err()
	})
	return stats, errors.Join(err, ctx.Err())
}

func contractSourceRunPath(dir string, ordinal int) string {
	return filepath.Join(dir, fmt.Sprintf("source-%08d.sst", ordinal))
}

func appendContractSource(ctx context.Context, runs *sortedRuns, stats *RebuildContractSourceStats, c contracts.Contract) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e := contractFactsEncoder{}
	writeFactRecord(&e, c)
	if e.err != nil {
		return e.err
	}
	key := binary.BigEndian.AppendUint64(nil, uint64(stats.Records))
	size := len(key) + len(e.buf) + 48
	if size > goldBufferBytes {
		return fmt.Errorf("%w: contract source record", ErrRebuildLinkInputSize)
	}
	if len(runs.rows) != 0 && runs.bytes+size > runs.limit {
		path := contractSourceRunPath(runs.dir, stats.SpillRuns)
		if err := runs.writeRun(path); err != nil {
			return err
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		stats.SpillRuns++
		stats.SpillBytes += info.Size()
	}
	runs.rows = append(runs.rows, row{key, e.buf})
	runs.bytes += size
	stats.Records++
	stats.PeakBatchBytes = max(stats.PeakBatchBytes, runs.bytes)
	return nil
}

func visitContractSourceRun(ctx context.Context, runs *sortedRuns, path string, next *uint64, yield func(contracts.Contract) error) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	table, err := openGoldTable(ctx, path, runs.options)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, table.Close()) }()
	it, err := table.NewIter(sstable.NoTransforms, nil, nil, sstable.TableBlobContext{})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, it.Close()) }()
	var key [8]byte
	for kv := it.First(); kv != nil; kv = it.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		binary.BigEndian.PutUint64(key[:], *next)
		if !bytes.Equal(kv.K.UserKey, key[:]) {
			return invalid("contract source record ordinal changed")
		}
		raw, _, err := kv.Value(nil)
		if err != nil {
			return err
		}
		d := recordCursor{buf: raw}
		c := readFactRecord(&d)
		if err := d.finish(); err != nil {
			return err
		}
		if err := yield(c); err != nil {
			return err
		}
		*next++
	}
	return it.Error()
}

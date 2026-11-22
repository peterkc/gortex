package indexer

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/objstorage/objstorageprovider"
	"github.com/cockroachdb/pebble/v2/sstable"
	"github.com/cockroachdb/pebble/v2/vfs"
)

const (
	rebuildCppTUKey        byte = 't'
	rebuildCppHeuristicKey byte = 'h'
	rebuildCppUnionKey     byte = 'u'
	rebuildCppReplayWindow      = 8 << 20
)

type rebuildCppReplay struct {
	dir   string
	db    *pebble.DB
	hasTU bool
}

// openRebuildCppReplay returns no reader until the exact sidecar has been
// checked and all scratch writes have completed. Close before releasing lease.
func openRebuildCppReplay(ctx context.Context, lease string, expected rebuildDigest) (_ *rebuildCppReplay, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(lease, "cpp-replay-")
	if err != nil {
		return nil, err
	}
	db, err := pebble.Open(filepath.Join(dir, "db"), &pebble.Options{
		ErrorIfExists: true, CacheSize: 8 << 20, DisableWAL: true, DisableAutomaticCompactions: true,
	})
	if err != nil {
		return nil, errors.Join(err, os.RemoveAll(dir))
	}
	r := &rebuildCppReplay{dir: dir, db: db}
	defer func() {
		if err != nil {
			if closeErr := r.Close(); closeErr != nil {
				err = errors.Join(err, closeErr)
			}
		}
	}()
	w := rebuildCppReplayRun{db: db, path: filepath.Join(dir, "run.sst"), rows: make(map[string]string)}
	var ordinal uint64
	if err := visitRebuildInputSidecar(ctx, lease, expected, func(row rebuildSidecarRow) error {
		switch row.Kind {
		case rebuildCppTU:
			r.hasTU = true
			// The validated paths cannot contain NUL. A present key with
			// an empty value replaces a previous nonempty TU just the same.
			return w.put(ctx, string(rebuildCppTUKey)+string(row.Path), string(bytes.Join(row.Dirs, []byte{0})))
		case rebuildCppHeuristicDir:
			key := binary.BigEndian.AppendUint64([]byte{rebuildCppHeuristicKey}, ordinal)
			ordinal++
			return w.put(ctx, string(key), string(row.Path))
		}
		return nil // database receipts and followed manifests do not supply paths
	}); err != nil {
		return nil, err
	}
	if err := w.flush(ctx); err != nil {
		return nil, err
	}
	// Only final TU values contribute. Pebble's iterator pins this view as
	// we ingest the disjoint union keys; identical union keys collapse.
	if r.hasTU {
		if err := r.walk(ctx, rebuildCppTUKey, func(it *pebble.Iterator) (bool, error) {
			raw, err := it.ValueAndErr()
			if err != nil {
				return false, err
			}
			return visitPackedRebuildCppDirs(raw, func(dir []byte) (bool, error) {
				return false, w.put(ctx, string(rebuildCppUnionKey)+string(dir), "")
			})
		}); err != nil {
			return nil, err
		}
		if err := w.flush(ctx); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r, nil
}

// Close releases only this replay's scratch, not the build lease or sidecar.
// The owner calls it once, after all visits return.
func (r *rebuildCppReplay) Close() error {
	return errors.Join(r.db.Close(), os.RemoveAll(r.dir))
}

// Visit implements resolver.RebuildCppIncludePaths. It does not read the
// sidecar or checkout again. A callback error wins even if it also requests stop.
func (r *rebuildCppReplay) Visit(ctx context.Context, importingFile string, visit func(string) (bool, error)) (bool, error) {
	if err := ctx.Err(); err != nil {
		return r.hasTU, err
	}
	emit := func(dir []byte) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		stop, err := visit(string(dir))
		if err != nil {
			return false, err
		}
		return stop, ctx.Err()
	}
	if !r.hasTU {
		return false, r.walk(ctx, rebuildCppHeuristicKey, func(it *pebble.Iterator) (bool, error) {
			dir, err := it.ValueAndErr()
			if err != nil {
				return false, err
			}
			return emit(dir)
		})
	}
	if len(importingFile) <= rebuildCppEntryLimit {
		if found, err := r.visitTU(importingFile, emit); found || err != nil {
			return true, err
		}
	}
	return true, r.walk(ctx, rebuildCppUnionKey, func(it *pebble.Iterator) (bool, error) {
		return emit(it.Key()[1:])
	})
}

func (r *rebuildCppReplay) visitTU(file string, emit func([]byte) (bool, error)) (hasDirs bool, err error) {
	raw, closer, err := r.db.Get([]byte(string(rebuildCppTUKey) + file))
	if errors.Is(err, pebble.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := closer.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	_, err = visitPackedRebuildCppDirs(raw, emit)
	return len(raw) != 0, err
}

func visitPackedRebuildCppDirs(raw []byte, visit func([]byte) (bool, error)) (bool, error) {
	for len(raw) != 0 {
		dir, rest, _ := bytes.Cut(raw, []byte{0})
		if stop, err := visit(dir); stop || err != nil {
			return stop, err
		}
		raw = rest
	}
	return false, nil
}

func (r *rebuildCppReplay) walk(ctx context.Context, prefix byte, visit func(*pebble.Iterator) (bool, error)) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	it, err := r.db.NewIterWithContext(ctx, &pebble.IterOptions{LowerBound: []byte{prefix}, UpperBound: []byte{prefix + 1}})
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := it.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	for ok := it.First(); ok; ok = it.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if stop, err := visit(it); stop || err != nil {
			return err
		}
	}
	if err := it.Error(); err != nil {
		return err
	}
	return ctx.Err()
}

// Use synchronous SST writes rather than waiting on Pebble's background
// memtable flush, which can stall when its disk writes fail. One sorted run
// sorts only its charged window; the sidecar bounds individual keys/values.
// With no memtable writes and automatic compactions off, all index writes
// complete in flush before a caller can use the reader.
type rebuildCppReplayRun struct {
	db   *pebble.DB
	path string
	rows map[string]string
	used int
}

func (w *rebuildCppReplayRun) put(ctx context.Context, key, value string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	const rowCharge = 64 // map entry and sorted key slice, not just path bytes
	charge := len(key) + len(value) + rowCharge
	if w.used+charge > rebuildCppReplayWindow {
		if err := w.flush(ctx); err != nil {
			return err
		}
	}
	if old, ok := w.rows[key]; ok {
		charge -= len(key) + len(old) + rowCharge
	}
	w.rows[key] = value
	w.used += charge
	return nil
}

func (w *rebuildCppReplayRun) flush(ctx context.Context) error {
	if len(w.rows) == 0 {
		return ctx.Err()
	}
	if err := w.write(ctx); err != nil {
		return err
	}
	if err := w.db.Ingest(ctx, []string{w.path}); err != nil {
		return err
	}
	// Ingest normally unlinks the source; surface a failure to remove
	// it before the same scratch name can be reused.
	if err := os.Remove(w.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	clear(w.rows)
	w.used = 0
	return ctx.Err()
}

func (w *rebuildCppReplayRun) write(ctx context.Context) (err error) {
	file, err := vfs.Default.Create(w.path, vfs.WriteCategoryUnspecified)
	if err != nil {
		return err
	}
	table := sstable.NewWriter(objstorageprovider.NewFileWritable(file), sstable.WriterOptions{
		TableFormat: w.db.FormatMajorVersion().MinTableFormat(),
	})
	defer func() { err = errors.Join(err, table.Close()) }()
	for _, key := range slices.Sorted(maps.Keys(w.rows)) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := table.Set([]byte(key), []byte(w.rows[key])); err != nil {
			return err
		}
	}
	return nil
}

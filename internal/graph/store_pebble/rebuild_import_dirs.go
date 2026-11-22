package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/objstorage"
	"github.com/cockroachdb/pebble/v2/objstorage/objstorageprovider"
	"github.com/cockroachdb/pebble/v2/sstable"
	"github.com/cockroachdb/pebble/v2/vfs"
	"github.com/zzet/gortex/internal/graph"
)

// ErrRebuildImportDirectories marks a failed private directory census or visit.
var ErrRebuildImportDirectories = errors.New("pebble rebuild import directories failed")

const (
	importDirExact byte = iota + 1
	importDirLast
)

const (
	importDirSourceRows = 1 << 22
	importDirFiles      = 1 << 18
	importDirRowBytes   = 16 << 10
	importDirBuffer     = 8 << 20
	importDirInputBytes = 64 << 20 // charged scratch rows, including both buckets
	importDirRuns       = 128
	importDirDiskBytes  = 128 << 20 // physical SST bytes across all runs and the final table
)

// RebuildImportDirectoryStats describes a completed census. SpillBytes counts
// closed threshold-triggered runs and excludes the final drain and merged SST.
type RebuildImportDirectoryStats struct {
	Files, Rows int
	SpillBytes  int64
}

// RebuildImportDirectories lends the full cold pass's frozen directory buckets.
// It is valid only in WithImportDirectories' callback. Visits are serial and
// must not reenter this index. A callback's false stops that bucket, not the pass.
type RebuildImportDirectories struct {
	iter   sstable.Iterator
	active bool
}

// WithImportDirectories scans one complete working node snapshot before use.
// scratchParent must be the existing private worker lease. The caller propagates
// lookup errors and abandons tentative work on any error, including cleanup. A
// complete visit with no rows and no error is an authoritative miss for this snapshot.
func (v RebuildView) WithImportDirectories(ctx context.Context, scratchParent string, spillAt int,
	use func(*RebuildImportDirectories) error,
) (stats RebuildImportDirectoryStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildImportDirectoryStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildImportDirectories, err)
		}
	}()
	if v.build == nil || !filepath.IsAbs(scratchParent) || spillAt < 1 || use == nil {
		return stats, invalid("import directory census requires a working view, private lease, spill threshold and callback")
	}
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	dir, err := os.MkdirTemp(scratchParent, "import-dirs-")
	if err != nil {
		return stats, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	p := importDirectorySort{
		dir: dir, limit: min(spillAt, importDirBuffer), remaining: importDirDiskBytes,
		runs: sortedRuns{options: headOptions()}, stats: &stats,
	}
	if err := p.collect(ctx, v); err != nil {
		return stats, err
	}
	index := RebuildImportDirectories{active: true}
	defer func() { index.active = false }()
	if stats.Rows > 0 {
		file := filepath.Join(dir, "buckets.sst")
		if err := p.seal(ctx, file); err != nil {
			return stats, err
		}
		table, openErr := openLinkTable(ctx, file)
		if openErr != nil {
			return stats, openErr
		}
		defer func() { err = errors.Join(err, table.Close()) }()
		index.iter, err = table.NewIter(sstable.NoTransforms, nil, nil, sstable.TableBlobContext{})
		if err != nil {
			return stats, err
		}
		defer func() { err = errors.Join(err, index.iter.Close()); index.iter = nil }()
	}
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	return stats, errors.Join(use(&index), ctx.Err(), v.mutationError())
}

// VisitExactDirectory visits identities whose graph path has exactly dir as its parent.
func (i *RebuildImportDirectories) VisitExactDirectory(ctx context.Context, dir string, yield func(graph.FileNodeIdentity) (bool, error)) error {
	return i.visit(ctx, importDirExact, dir, yield)
}

// VisitLastDirectoryComponent visits identities whose parent has this last
// component. Parents without a slash occur only in the exact directory bucket.
func (i *RebuildImportDirectories) VisitLastDirectoryComponent(ctx context.Context, component string, yield func(graph.FileNodeIdentity) (bool, error)) error {
	return i.visit(ctx, importDirLast, component, yield)
}

func (i *RebuildImportDirectories) visit(ctx context.Context, tier byte, bucket string, yield func(graph.FileNodeIdentity) (bool, error)) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("%w: %w", ErrRebuildImportDirectories, err)
		}
	}()
	if !i.active || yield == nil {
		return invalid("inactive import directory index or missing visitor")
	}
	if err := ctx.Err(); err != nil || i.iter == nil {
		return err
	}
	// Every persisted bucket fits within a complete encoded row. A longer
	// key cannot match and need not be copied to the seek buffer.
	if len(bucket) > importDirRowBytes {
		return nil
	}
	prefix := keyStrings([]byte{tier}, bucket)
	for kv := i.iter.SeekGE(prefix, 0); kv != nil && bytes.HasPrefix(kv.K.UserKey, prefix); kv = i.iter.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		value, _, err := kv.Value(nil)
		if err != nil {
			return err
		}
		file, err := readImportDirectoryFile(kv.K.UserKey[len(prefix):], value)
		if err != nil {
			return err
		}
		more, err := yield(file)
		if err != nil || !more || ctx.Err() != nil {
			return errors.Join(err, ctx.Err(), i.iter.Error())
		}
	}
	return errors.Join(ctx.Err(), i.iter.Error())
}

func readImportDirectoryFile(key, value []byte) (graph.FileNodeIdentity, error) {
	filePath, rest, err := readKeyString(key)
	if err != nil {
		return graph.FileNodeIdentity{}, err
	}
	id, rest, err := readKeyString(rest)
	if err != nil {
		return graph.FileNodeIdentity{}, err
	}
	if len(rest) != 0 || id == "" {
		return graph.FileNodeIdentity{}, invalid("bad import directory file key")
	}
	d := recordCursor{buf: value}
	file := graph.FileNodeIdentity{ID: id, FilePath: filePath, RepoPrefix: d.str(), WorkspaceID: d.str(), Language: d.str()}
	return file, d.finish()
}

type importDirectorySort struct {
	dir               string
	limit, totalBytes int
	remaining         int64
	runs              sortedRuns
	stats             *RebuildImportDirectoryStats
}

func (p *importDirectorySort) collect(ctx context.Context, v RebuildView) error {
	return v.read(ctx, func(snap *pebble.Snapshot) (err error) {
		prefix := []byte{familyNode}
		it, err := snap.NewIterWithContext(ctx, &pebble.IterOptions{LowerBound: prefix, UpperBound: prefixEnd(prefix)})
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, it.Close()) }()
		scanned := 0
		for ok := it.First(); ok; ok = it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			// Charge the saved key and value before fetching or decoding the
			// value. The private DB itself owns its iterator and pinned blocks.
			lv := it.LazyValue()
			if scanned == importDirSourceRows || len(it.Key()) > linkRecordBytes || lv.Len() > linkRecordBytes-len(it.Key()) {
				return invalid("import directory census exceeds source row or record bound")
			}
			scanned++
			raw, err := it.ValueAndErr()
			if err != nil {
				return err
			}
			n, err := decodeNodeLight(raw)
			if err != nil {
				return err
			}
			if n.ID == "" || !bytes.Equal(it.Key(), pendingNodeKey(n.ID)) {
				return invalid("import directory census node identity does not match working key")
			}
			if n.Kind != graph.KindFile {
				continue
			}
			if err := p.addFile(ctx, n); err != nil {
				return err
			}
		}
		return errors.Join(ctx.Err(), it.Error())
	})
}

func (p *importDirectorySort) addFile(ctx context.Context, n *graph.Node) error {
	if p.stats.Files == importDirFiles || len(n.FilePath)+len(n.ID)+len(n.RepoPrefix)+len(n.WorkspaceID)+len(n.Language) > importDirRowBytes {
		return invalid("import directory census exceeds file count or identity bound")
	}
	p.stats.Files++
	// Match native filePathDir, not a basename of the file or its ID.
	dir := path.Dir(filepath.ToSlash(n.FilePath))
	value := appendMetaString(nil, n.RepoPrefix)
	value = appendMetaString(value, n.WorkspaceID)
	value = appendMetaString(value, n.Language)
	if err := p.add(ctx, keyStrings([]byte{importDirExact}, dir, n.FilePath, n.ID), value); err != nil {
		return err
	}
	if slash := strings.LastIndexByte(dir, '/'); slash >= 0 && slash+1 < len(dir) {
		return p.add(ctx, keyStrings([]byte{importDirLast}, dir[slash+1:], n.FilePath, n.ID), value)
	}
	return nil
}

func (p *importDirectorySort) add(ctx context.Context, key, value []byte) error {
	size := len(key) + len(value) + 48 // row slice headers as in sortedRuns
	if size > importDirRowBytes || size > importDirInputBytes-p.totalBytes {
		return invalid("import directory census exceeds encoded row or total sort bound")
	}
	if len(p.runs.rows) > 0 && size > p.limit-p.runs.bytes {
		before := p.remaining
		if err := p.flush(ctx); err != nil {
			return err
		}
		p.stats.SpillBytes += before - p.remaining
	}
	p.runs.rows = append(p.runs.rows, row{key, value})
	p.runs.bytes += size
	p.totalBytes += size
	p.stats.Rows++
	return nil
}

// Use the existing run merge, but supply its paths ourselves: sortedRuns.flush
// uses an uncapped file writer. No rows remain for merge's implicit flush.
func (p *importDirectorySort) flush(ctx context.Context) (err error) {
	if len(p.runs.paths) == importDirRuns {
		return invalid("import directory census exceeds sorted run bound")
	}
	file := filepath.Join(p.dir, fmt.Sprintf("run-%03d.sst", len(p.runs.paths)))
	w, err := newImportDirectoryTable(ctx, file, &p.remaining)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, w.Close()) }()
	slices.SortFunc(p.runs.rows, func(a, b row) int { return bytes.Compare(a.key, b.key) })
	for _, r := range p.runs.rows {
		if err := w.Set(r.key, r.value); err != nil {
			return err
		}
	}
	p.runs.paths = append(p.runs.paths, file)
	clear(p.runs.rows)
	p.runs.rows = p.runs.rows[:0]
	p.runs.bytes = 0
	return nil
}

func (p *importDirectorySort) seal(ctx context.Context, file string) error {
	if err := p.flush(ctx); err != nil {
		return err
	}
	p.runs.rows = nil
	w, err := newImportDirectoryTable(ctx, file, &p.remaining)
	if err != nil {
		return err
	}
	if err := errors.Join(p.runs.merge(ctx, func(r row) error { return w.Set(r.key, r.value) }), w.Close()); err != nil {
		return err
	}
	for _, run := range p.runs.paths {
		if err := os.Remove(run); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// Charge all physical writes, including indexes and footers, before forwarding
// them. Plain uncompressed runs bound the merge's index bytes by the disk cap.
type importDirectoryWritable struct {
	objstorage.Writable
	ctx       context.Context
	remaining *int64
}

func (w importDirectoryWritable) Write(b []byte) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if int64(len(b)) > *w.remaining {
		return invalid("import directory census exceeds physical SST byte bound")
	}
	*w.remaining -= int64(len(b))
	return w.Writable.Write(b)
}

func newImportDirectoryTable(ctx context.Context, path string, remaining *int64) (*sstable.Writer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := vfs.Default.Create(path, vfs.WriteCategoryUnspecified)
	if err != nil {
		return nil, err
	}
	w := importDirectoryWritable{Writable: objstorageprovider.NewFileWritable(f), ctx: ctx, remaining: remaining}
	return sstable.NewWriter(w, sstable.WriterOptions{
		TableFormat: sstable.TableFormatPebblev4, Compression: sstable.NoCompression, BlockSize: 32 << 10,
	}), nil
}

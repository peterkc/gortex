package store_pebble

import (
	"bytes"
	"container/heap"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/objstorage/objstorageprovider"
	"github.com/cockroachdb/pebble/v2/sstable"
	"github.com/cockroachdb/pebble/v2/vfs"
	"github.com/zeebo/blake3"
)

const goldBufferBytes = 64 << 20
const goldFileCap = 1024
const goldLevel = 6

type row struct{ key, value []byte }

func newTable(path string, opts sstable.WriterOptions) (*sstable.Writer, error) {
	file, err := vfs.Default.Create(path, vfs.WriteCategoryUnspecified)
	if err != nil {
		return nil, err
	}
	return sstable.NewWriter(objstorageprovider.NewFileWritable(file), opts), nil
}

func openGoldTable(ctx context.Context, path string, opts *pebble.Options) (*sstable.Reader, error) {
	f, err := vfs.Default.Open(path)
	if err != nil {
		return nil, err
	}
	readable, err := sstable.NewSimpleReadable(f)
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	table, err := sstable.NewReader(ctx, readable, opts.MakeReaderOptions())
	if err != nil {
		return nil, errors.Join(err, readable.Close())
	}
	return table, nil
}

// sortedRuns owns its private scratch SSTs. The caller removes their scratch
// directory on success or failure. Key and value slices become run-owned.
type sortedRuns struct {
	dir, name string
	options   *pebble.Options
	format    sstable.TableFormat
	limit     int
	rows      []row
	bytes     int
	paths     []string
}

func (s *sortedRuns) add(key, value []byte) error {
	size := len(key) + len(value) + 48
	if len(s.rows) != 0 && s.bytes+size > s.limit {
		if err := s.flush(); err != nil {
			return err
		}
	}
	s.rows = append(s.rows, row{key, value})
	s.bytes += size
	return nil
}

type sortedRunLimitError struct{ name string }

func (e *sortedRunLimitError) Error() string {
	return fmt.Sprintf("pebble %s exceeds %d sorted runs", e.name, goldFileCap)
}

func (s *sortedRuns) flush() error {
	if len(s.rows) == 0 {
		return nil
	}
	if len(s.paths) >= goldFileCap {
		return &sortedRunLimitError{name: s.name}
	}
	path := filepath.Join(s.dir, fmt.Sprintf("%s-%04d.sst", s.name, len(s.paths)))
	if err := s.writeRun(path); err != nil {
		return err
	}
	s.paths = append(s.paths, path)
	return nil
}

// writeRun writes just one buffer. The owner tracks its path for a merge or
// reconstructs the name when runs already have disjoint ordered ranges.
func (s *sortedRuns) writeRun(path string) (err error) {
	slices.SortFunc(s.rows, func(a, b row) int { return bytes.Compare(a.key, b.key) })
	w, err := newTable(path, s.options.MakeWriterOptions(goldLevel, s.format))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, w.Close()) }()
	var last []byte
	for _, row := range s.rows {
		if bytes.Equal(last, row.key) {
			return invalid("duplicate canonical key in sorted run")
		}
		if err := w.Set(row.key, row.value); err != nil {
			return err
		}
		last = row.key
	}
	clear(s.rows)
	s.rows = s.rows[:0]
	s.bytes = 0
	return nil
}

type runCursor struct {
	iter sstable.Iterator
	row  row
}

type runQueue []*runCursor

func (q runQueue) Len() int           { return len(q) }
func (q runQueue) Less(i, j int) bool { return bytes.Compare(q[i].row.key, q[j].row.key) < 0 }
func (q runQueue) Swap(i, j int)      { q[i], q[j] = q[j], q[i] }
func (q *runQueue) Push(v any)        { *q = append(*q, v.(*runCursor)) }
func (q *runQueue) Pop() any {
	last := len(*q) - 1
	v := (*q)[last]
	(*q)[last] = nil
	*q = (*q)[:last]
	return v
}

func (s *sortedRuns) merge(ctx context.Context, visit func(row) error) (err error) {
	if err := s.flush(); err != nil {
		return err
	}
	var q runQueue
	for _, path := range s.paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		table, openErr := openGoldTable(ctx, path, s.options)
		if openErr != nil {
			return openErr
		}
		defer func() { err = errors.Join(err, table.Close()) }()
		i, iterErr := table.NewIter(sstable.NoTransforms, nil, nil, sstable.TableBlobContext{})
		if iterErr != nil {
			return iterErr
		}
		defer func() { err = errors.Join(err, i.Close()) }()
		if kv := i.First(); kv != nil {
			value, _, err := kv.Value(nil)
			if err != nil {
				return err
			}
			heap.Push(&q, &runCursor{iter: i, row: row{kv.K.UserKey, value}})
		} else if err := i.Error(); err != nil {
			return err
		}
	}
	var last []byte
	for q.Len() > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		c := heap.Pop(&q).(*runCursor)
		if bytes.Equal(last, c.row.key) {
			return invalid("duplicate canonical key across sorted runs")
		}
		if err := visit(c.row); err != nil {
			return err
		}
		last = append(last[:0], c.row.key...)
		if kv := c.iter.Next(); kv != nil {
			value, _, err := kv.Value(nil)
			if err != nil {
				return err
			}
			c.row = row{kv.K.UserKey, value}
			heap.Push(&q, c)
		} else if err := c.iter.Error(); err != nil {
			return err
		}
	}
	return nil
}

// publishGold is the b1 private full-result writer. The staged path uses the
// same merge to hand sealed tables to the process that owns the live head.
func publishGold(ctx context.Context, db *pebble.DB, rows *sortedRuns, seal *goldSeal) (err error) {
	base := db.NewSnapshot()
	defer func() { err = errors.Join(err, base.Close()) }()
	paths, err := writeGoldTables(ctx, base, rows, goldFileCap, seal.visit, seal.finish)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return ctx.Err()
	}
	return db.Ingest(ctx, paths)
}

// writeGoldTables streams an exact difference. With no base, it emits sets
// only. Visit and finish validate the final logical stream, not the delta.
func writeGoldTables(
	ctx context.Context, base pebble.Reader, rows *sortedRuns, maxTables int,
	visit func(row) error, finish func(context.Context) error,
) ([]string, error) {
	var old goldBaseCursor
	if base != nil {
		it, err := base.NewIterWithContext(ctx, nil)
		if err != nil {
			return nil, err
		}
		old = it
	}
	return writeGoldDiff(ctx, old, rows, maxTables, visit, finish)
}

// goldBaseCursor covers a private Pebble snapshot or ordered, retained SSTs.
// Key and value are borrowed until the next move. The diff owns the cursor.
type goldBaseCursor interface {
	First() bool
	Next() bool
	Key() []byte
	ValueAndErr() ([]byte, error)
	Error() error
	Close() error
}

func writeGoldDiff(
	ctx context.Context, old goldBaseCursor, rows *sortedRuns, maxTables int,
	visit func(row) error, finish func(context.Context) error,
) ([]string, error) {
	return writeCheckedGoldDiff(ctx, old, rows, maxTables, visit, finish, nil)
}

func writeCheckedGoldDiff(
	ctx context.Context, old goldBaseCursor, rows *sortedRuns, maxTables int,
	visit func(row) error, finish func(context.Context) error, change func(row) error,
) (paths []string, err error) {
	var w *sstable.Writer
	closeTable := func() error {
		if w == nil {
			return nil
		}
		old := w
		w = nil
		return old.Close()
	}
	defer func() { err = errors.Join(err, closeTable()) }()
	live := false
	if old != nil {
		defer func() { err = errors.Join(err, old.Close()) }()
		live = old.First()
	}
	write := func(key, value []byte, remove bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if change != nil {
			if remove {
				var err error
				value, err = old.ValueAndErr()
				if err != nil {
					return err
				}
			}
			if err := change(row{key, value}); err != nil {
				return err
			}
		}
		if w == nil || w.Raw().EstimatedSize() >= goldBufferBytes {
			if err := closeTable(); err != nil {
				return err
			}
			if len(paths) >= maxTables {
				return fmt.Errorf("%w: at most %d data tables", ErrStageTableLimit, maxTables)
			}
			path := filepath.Join(rows.dir, fmt.Sprintf("gold-%04d.sst", len(paths)))
			w, err = newTable(path, rows.options.MakeWriterOptions(goldLevel, rows.format))
			if err != nil {
				return err
			}
			paths = append(paths, path)
		}
		if remove {
			return w.Delete(key)
		}
		return w.Set(key, value)
	}
	advance := func() bool {
		for ok := old.Next(); ok; ok = old.Next() {
			if !bytes.Equal(old.Key(), publicationWitnessKey) {
				return true
			}
		}
		return false
	}
	if live && bytes.Equal(old.Key(), publicationWitnessKey) {
		live = advance()
	}
	if err := rows.merge(ctx, func(r row) error {
		if err := visit(r); err != nil {
			return err
		}
		for live && bytes.Compare(old.Key(), r.key) < 0 {
			if err := write(old.Key(), nil, true); err != nil {
				return err
			}
			live = advance()
		}
		if live && bytes.Equal(old.Key(), r.key) {
			value, err := old.ValueAndErr()
			if err != nil {
				return err
			}
			if !bytes.Equal(value, r.value) {
				if err := write(r.key, r.value, false); err != nil {
					return err
				}
			}
			live = advance()
			return nil
		}
		return write(r.key, r.value, false)
	}); err != nil {
		return nil, err
	}
	for live {
		if err := write(old.Key(), nil, true); err != nil {
			return nil, err
		}
		live = advance()
	}
	if old != nil {
		if err := old.Error(); err != nil {
			return nil, err
		}
	}
	if err := closeTable(); err != nil {
		return nil, err
	}
	if err := finish(ctx); err != nil {
		return nil, err
	}
	return paths, nil
}

// goldWitness records one digest per family in a complete ordered stream.
type goldWitness struct {
	digest   *blake3.Hasher
	families []FamilyWitness
	current  FamilyWitness
}

func (w *goldWitness) add(r row) {
	if r.key[0] != w.current.Family {
		w.flush()
		w.current = FamilyWitness{Family: r.key[0]}
		w.digest.Reset()
	}
	w.current.Count++
	writeWitnessRow(w.digest, r.key, r.value)
}

func (w *goldWitness) flush() {
	if w.current.Count != 0 {
		w.current.Digest = HeadDigest(w.digest.Sum(nil))
		w.families = append(w.families, w.current)
		w.current = FamilyWitness{}
	}
}

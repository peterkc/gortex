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

func (s *sortedRuns) flush() (err error) {
	if len(s.rows) == 0 {
		return nil
	}
	if len(s.paths) >= goldFileCap {
		return fmt.Errorf("pebble %s exceeds %d sorted runs", s.name, goldFileCap)
	}
	slices.SortFunc(s.rows, func(a, b row) int { return bytes.Compare(a.key, b.key) })
	path := filepath.Join(s.dir, fmt.Sprintf("%s-%04d.sst", s.name, len(s.paths)))
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
	s.paths = append(s.paths, path)
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
		f, err := vfs.Default.Open(path)
		if err != nil {
			return err
		}
		readable, err := sstable.NewSimpleReadable(f)
		if err != nil {
			return errors.Join(err, f.Close())
		}
		table, err := sstable.NewReader(ctx, readable, s.options.MakeReaderOptions())
		if err != nil {
			return errors.Join(err, readable.Close())
		}
		defer func() { err = errors.Join(err, table.Close()) }()
		i, err := table.NewIter(sstable.NoTransforms, nil, nil, sstable.TableBlobContext{})
		if err != nil {
			return err
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

// publishGold writes the selected rows once, emitting non-overlapping SSTs.
// Validation observes the actual sorted bytes before Ingest is allowed.
func publishGold(ctx context.Context, db *pebble.DB, rows *sortedRuns, seal *goldSeal) (err error) {
	var w *sstable.Writer
	var paths []string
	closeTable := func() error {
		if w == nil {
			return nil
		}
		old := w
		w = nil
		return old.Close()
	}
	defer func() { err = errors.Join(err, closeTable()) }()
	// Compare the sealed stream with the last serving sequence. The old
	// iterator participates only in the delta, never in seal validation.
	base := db.NewSnapshot()
	defer func() { err = errors.Join(err, base.Close()) }()
	old, err := base.NewIterWithContext(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, old.Close()) }()
	live := old.First()
	write := func(key, value []byte, remove bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if w == nil || w.Raw().EstimatedSize() >= goldBufferBytes {
			if err := closeTable(); err != nil {
				return err
			}
			if len(paths) >= goldFileCap {
				return fmt.Errorf("pebble publish exceeds %d tables", goldFileCap)
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
	if err := rows.merge(ctx, func(r row) error {
		if err := seal.visit(r); err != nil {
			return err
		}
		for live && bytes.Compare(old.Key(), r.key) < 0 {
			if err := write(old.Key(), nil, true); err != nil {
				return err
			}
			live = old.Next()
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
			live = old.Next()
			return nil
		}
		return write(r.key, r.value, false)
	}); err != nil {
		return err
	}
	for live {
		if err := write(old.Key(), nil, true); err != nil {
			return err
		}
		live = old.Next()
	}
	if err := old.Error(); err != nil {
		return err
	}
	if err := closeTable(); err != nil {
		return err
	}
	if err := seal.finish(ctx); err != nil {
		return err
	}
	if len(paths) == 0 {
		return ctx.Err()
	}
	return db.Ingest(ctx, paths)
}

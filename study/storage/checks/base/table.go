package base

import (
	"bufio"
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/compress/snappy"
)

// Row tables are sorted by (key,id). Their range directories are a sparse
// CSR/CSC analogue: an absent endpoint has an empty range, not a fake node.
type row struct {
	Key string `parquet:"key,dict"`
	ID  string `parquet:"id,dict"`
	Raw []byte `parquet:"payload"`
}
type rowRange struct {
	Key   string `parquet:"key,dict"`
	First int64  `parquet:"first,delta"`
	End   int64  `parquet:"end,delta"`
}

func encodedRows[T any](records iter.Seq2[T, error], identity func(T) (string, string)) iter.Seq2[row, error] {
	return func(yield func(row, error) bool) {
		for value, err := range records {
			if err != nil {
				yield(row{}, err)
				return
			}
			raw, err := json.Marshal(value)
			key, id := identity(value)
			if !yield(row{Key: key, ID: id, Raw: raw}, err) {
				return
			}
		}
	}
}

// External sort, Parquet compression and range construction all stay inside
// the engine's construction CPU/memory accounting. Scratch is not a cache.
func writeTable(ctx context.Context, dir, name string, rows iter.Seq2[row, error], unique bool) (err error) {
	scratch, err := os.MkdirTemp(dir, "sort-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(scratch)) }()
	input, err := os.Create(filepath.Join(scratch, "input"))
	if err != nil {
		return err
	}
	buffer := bufio.NewWriterSize(input, 64<<10)
	for r, e := range rows {
		if e != nil {
			return errors.Join(e, input.Close())
		}
		if e = ctx.Err(); e != nil {
			return errors.Join(e, input.Close())
		}
		if r.Key == "" && unique {
			return errors.Join(fmt.Errorf("empty %s identity", name), input.Close())
		}
		if _, e = fmt.Fprintf(buffer, "%s\t%s\t%s\n", hex.EncodeToString([]byte(r.Key)), hex.EncodeToString([]byte(r.ID)), r.Raw); e != nil {
			return errors.Join(e, input.Close())
		}
	}
	if err = errors.Join(buffer.Flush(), input.Close()); err != nil {
		return err
	}
	sorted := filepath.Join(scratch, "sorted")
	cmd := exec.CommandContext(ctx, "sort", "-S", "4M", "-T", scratch, "-o", sorted, input.Name())
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	if output, e := cmd.CombinedOutput(); e != nil {
		return fmt.Errorf("sort %s: %w: %s", name, e, output)
	}
	source, err := os.Open(sorted)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, source.Close()) }()
	data, err := os.Create(filepath.Join(dir, name+".parquet"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, data.Close()) }()
	ranges, err := os.Create(filepath.Join(dir, name+".ranges.parquet"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, ranges.Close()) }()
	options := []parquet.WriterOption{parquet.MaxRowsPerRowGroup(1024), parquet.PageBufferSize(32 << 10), parquet.Compression(&snappy.Codec{})}
	writer := parquet.NewGenericWriter[row](data, options...)
	directory := parquet.NewGenericWriter[rowRange](ranges, options...)
	defer func() {
		if writer != nil {
			err = errors.Join(err, writer.Close())
		}
		if directory != nil {
			err = errors.Join(err, directory.Close())
		}
	}()
	expected := sha256.New()
	reader := bufio.NewReaderSize(source, 64<<10)
	var current rowRange
	var index int64
	for {
		line, e := reader.ReadString('\n')
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		parts := strings.SplitN(strings.TrimSuffix(line, "\n"), "\t", 3)
		if len(parts) != 3 {
			return fmt.Errorf("invalid sort row")
		}
		key, e := hex.DecodeString(parts[0])
		if e != nil {
			return e
		}
		id, e := hex.DecodeString(parts[1])
		if e != nil {
			return e
		}
		if index > 0 && current.Key == string(key) && unique {
			return fmt.Errorf("duplicate %s identity %q", name, key)
		}
		if index == 0 || current.Key != string(key) {
			if index > 0 {
				current.End = index
				if _, e = directory.Write([]rowRange{current}); e != nil {
					return e
				}
			}
			current = rowRange{Key: string(key), First: index}
		}
		value := row{Key: string(key), ID: string(id), Raw: []byte(parts[2])}
		if e = json.NewEncoder(expected).Encode(value); e != nil {
			return e
		}
		if _, e = writer.Write([]row{value}); e != nil {
			return e
		}
		index++
	}
	if index > 0 {
		current.End = index
		if _, err = directory.Write([]rowRange{current}); err != nil {
			return err
		}
	}
	closeErr := errors.Join(writer.Close(), directory.Close())
	writer = nil
	directory = nil
	if closeErr != nil {
		return closeErr
	}
	actual := sha256.New()
	if err = scanTable(ctx, dir, name, newBlockCache(4<<20), nil, func(r row) error {
		return json.NewEncoder(actual).Encode(r)
	}); err != nil {
		return err
	}
	if !bytes.Equal(expected.Sum(nil), actual.Sum(nil)) {
		return fmt.Errorf("full-payload Parquet round trip differs: %s", name)
	}
	return nil
}

const blockSize int64 = 32 << 10

type blockKey struct {
	Path   string
	Offset int64
}
type cacheEntry struct {
	key   blockKey
	bytes []byte
}
type blockCache struct {
	mu          sync.Mutex
	limit, used int64
	entries     map[blockKey]*list.Element
	lru         *list.List
}

func newBlockCache(limit int64) *blockCache {
	return &blockCache{limit: limit, entries: map[blockKey]*list.Element{}, lru: list.New()}
}

type cachedFile struct {
	file  *os.File
	cache *blockCache
}

func (f cachedFile) ReadAt(target []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, fmt.Errorf("negative read offset")
	}
	total := 0
	for len(target) > 0 {
		aligned := offset / blockSize * blockSize
		key := blockKey{Path: f.file.Name(), Offset: aligned}
		f.cache.mu.Lock()
		element, ok := f.cache.entries[key]
		if !ok {
			bytes := make([]byte, blockSize)
			n, err := f.file.ReadAt(bytes, aligned)
			if err != nil && err != io.EOF {
				f.cache.mu.Unlock()
				return total, err
			}
			bytes = bytes[:n]
			element = f.cache.lru.PushFront(cacheEntry{key: key, bytes: bytes})
			f.cache.entries[key] = element
			f.cache.used += int64(n)
			for f.cache.used > f.cache.limit && f.cache.lru.Len() > 1 {
				last := f.cache.lru.Back()
				entry := last.Value.(cacheEntry)
				delete(f.cache.entries, entry.key)
				f.cache.used -= int64(len(entry.bytes))
				f.cache.lru.Remove(last)
			}
		}
		f.cache.lru.MoveToFront(element)
		bytes := element.Value.(cacheEntry).bytes
		start := offset - aligned
		if start >= int64(len(bytes)) {
			f.cache.mu.Unlock()
			return total, io.EOF
		}
		n := copy(target, bytes[start:])
		f.cache.mu.Unlock()
		target = target[n:]
		offset += int64(n)
		total += n
	}
	return total, nil
}
func openRows[T any](path string, cache *blockCache) (*parquet.GenericReader[T], *os.File, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	stat, err := file.Stat()
	if err != nil {
		return nil, nil, errors.Join(err, file.Close())
	}
	pf, err := parquet.OpenFile(cachedFile{file, cache}, stat.Size())
	if err != nil {
		return nil, nil, errors.Join(err, file.Close())
	}
	if !parquet.EqualNodes(parquet.SchemaOf(new(T)), pf.Schema()) {
		return nil, nil, errors.Join(fmt.Errorf("unexpected table schema: %s", path), file.Close())
	}
	return parquet.NewGenericReader[T](pf), file, nil
}
func tableRange(path, key string, cache *blockCache) (result rowRange, found bool, err error) {
	reader, file, err := openRows[rowRange](path, cache)
	if err != nil {
		return result, false, err
	}
	defer func() { err = errors.Join(err, reader.Close(), file.Close()) }()
	low, high := int64(0), reader.NumRows()
	one := make([]rowRange, 1)
	for low < high {
		middle := low + (high-low)/2
		if err = reader.SeekToRow(middle); err != nil {
			return result, false, err
		}
		if _, err = reader.Read(one); err != nil && err != io.EOF {
			return result, false, err
		}
		if one[0].Key < key {
			low = middle + 1
		} else {
			high = middle
		}
	}
	if low == reader.NumRows() {
		return result, false, nil
	}
	if err = reader.SeekToRow(low); err != nil {
		return result, false, err
	}
	n, e := reader.Read(one)
	if e != nil && e != io.EOF {
		return result, false, e
	}
	if n != 1 {
		return result, false, fmt.Errorf("truncated range directory")
	}
	result = one[0]
	if result.Key != key {
		return result, false, nil
	}
	if result.First < 0 || result.End <= result.First {
		return result, false, fmt.Errorf("invalid row range")
	}
	return result, true, nil
}
func scanTable(ctx context.Context, dir, name string, cache *blockCache, span *rowRange, visit func(row) error) (err error) {
	reader, file, err := openRows[row](filepath.Join(dir, name+".parquet"), cache)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, reader.Close(), file.Close()) }()
	first, end := int64(0), reader.NumRows()
	if span != nil {
		first, end = span.First, span.End
	}
	if first < 0 || first > end || end > reader.NumRows() {
		return fmt.Errorf("range outside table")
	}
	if first == end {
		return nil
	}
	if err = reader.SeekToRow(first); err != nil {
		return err
	}
	buffer := make([]row, 64)
	for first < end {
		if err = ctx.Err(); err != nil {
			return err
		}
		n, e := reader.Read(buffer[:min(int64(len(buffer)), end-first)])
		if e != nil && e != io.EOF {
			return e
		}
		if n == 0 {
			return fmt.Errorf("truncated table")
		}
		for _, r := range buffer[:n] {
			if err = visit(r); err != nil {
				return err
			}
		}
		first += int64(n)
	}
	return nil
}

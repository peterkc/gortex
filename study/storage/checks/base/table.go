package base

import (
	"bufio"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/compress/snappy"
	"gxstudy/runner"
)

// Row tables are sorted by (key,id). Their range directories are a sparse
// CSR/CSC analogue: an absent endpoint has an empty range, not a fake node.
type row struct {
	Key   string `parquet:"key,dict"`
	ID    string `parquet:"id,dict"`
	Local uint64 `parquet:"local,plain,uncompressed"`
	Raw   []byte `parquet:"payload"`
}
type rowRange struct {
	Tag     uint64 `parquet:"tag,plain,uncompressed"`
	Grouped bool   `parquet:"grouped"`
	Numeric bool   `parquet:"numeric"`
	Key     string `parquet:"key,dict"`
	Number  uint64 `parquet:"number,plain,uncompressed"`
	First   int64  `parquet:"first,delta"`
	End     int64  `parquet:"end,delta"`
}

type rowSpool struct {
	file   *os.File
	buffer *bufio.Writer
}

func newRowSpool(path string) (*rowSpool, error) {
	file, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &rowSpool{file: file, buffer: bufio.NewWriterSize(file, 64<<10)}, nil
}
func (s *rowSpool) put(r row) error {
	_, err := fmt.Fprintf(s.buffer, "%s\t%s\t%s\n", hex.EncodeToString([]byte(r.Key)), hex.EncodeToString([]byte(r.ID)), hex.EncodeToString(r.Raw))
	return err
}
func (s *rowSpool) close() error { return errors.Join(s.buffer.Flush(), s.file.Close()) }

type tableVerification struct {
	Rows         int64  `json:"rows"`
	InputSHA256  string `json:"input_sha256"`
	OutputSHA256 string `json:"output_sha256"`
	CPU          int64  `json:"cpu_ns"`
}

func verifyTable(ctx context.Context, dir, name, expected string, count int64) (result tableVerification, err error) {
	before, err := runner.ProcessCPU()
	if err != nil {
		return result, err
	}
	actual := sha256.New()
	var rows int64
	err = scanTable(ctx, dir, name, newBlockCache(4<<20), nil, func(r row) error { rows++; return json.NewEncoder(actual).Encode(r) })
	after, cpuErr := runner.ProcessCPU()
	result = tableVerification{Rows: rows, InputSHA256: expected, OutputSHA256: hex.EncodeToString(actual.Sum(nil)), CPU: after - before}
	if err = errors.Join(err, cpuErr); err != nil {
		return result, err
	}
	if rows != count || result.InputSHA256 != result.OutputSHA256 {
		return result, fmt.Errorf("full-payload Parquet round trip differs: %s", name)
	}
	if result.CPU < 0 {
		return result, fmt.Errorf("negative verification CPU")
	}
	return result, nil
}

// Source decoding has already fanned out into bounded on-disk sort inputs.
// All sort/index/compression/seal CPU remains construction work. Only the
// separately recorded, complete read-back verification is excluded from the tie.
func writeSortedTable(ctx context.Context, dir, scratch, name string, unique bool) (verification tableVerification, err error) {
	input := filepath.Join(scratch, name+".input")
	sorted := filepath.Join(scratch, name+".sorted")
	cmd := exec.CommandContext(ctx, "sort", "-S", "4M", "-T", scratch, "-o", sorted, input)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	if output, e := cmd.CombinedOutput(); e != nil {
		return verification, fmt.Errorf("sort %s: %w: %s", name, e, output)
	}
	source, err := os.Open(sorted)
	if err != nil {
		return verification, err
	}
	defer func() { err = errors.Join(err, source.Close()) }()
	data, err := os.Create(filepath.Join(dir, name+".parquet"))
	if err != nil {
		return verification, err
	}
	defer func() { err = errors.Join(err, data.Close()) }()
	options := []parquet.WriterOption{parquet.MaxRowsPerRowGroup(1024), parquet.PageBufferSize(32 << 10), parquet.Compression(&snappy.Codec{})}
	directory, ranges, err := newRangeDirectory(dir, name, options)
	if err != nil {
		return verification, err
	}
	writer := parquet.NewGenericWriter[row](data, options...)
	if ranges != nil {
		defer func() { err = errors.Join(err, ranges.Close()) }()
	}
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
		if err = ctx.Err(); err != nil {
			return verification, err
		}
		line, e := reader.ReadString('\n')
		if e == io.EOF {
			break
		}
		if e != nil {
			return verification, e
		}
		parts := strings.SplitN(strings.TrimSuffix(line, "\n"), "\t", 3)
		if len(parts) != 3 {
			return verification, fmt.Errorf("invalid sort row")
		}
		key, e := hex.DecodeString(parts[0])
		if e != nil {
			return verification, e
		}
		id, e := hex.DecodeString(parts[1])
		if e != nil {
			return verification, e
		}
		if unique && ((len(key) == 0 && name != "kinds") || (index > 0 && current.Key == string(key))) {
			return verification, fmt.Errorf("empty or duplicate %s identity %q", name, key)
		}
		if index == 0 || current.Key != string(key) {
			if index > 0 && directory != nil {
				current.End = index
				if _, e = directory.Write([]rowRange{storedRange(name, current)}); e != nil {
					return verification, e
				}
			}
			current = rowRange{Key: string(key), First: index}
		}
		raw, e := hex.DecodeString(parts[2])
		if e != nil {
			return verification, e
		}
		value := row{Key: string(key), ID: string(id), Raw: raw}
		if graphTable(name) {
			local := string(id)
			if name == "out" || name == "in" || name == "names" {
				local = local[len(local)-16:]
			}
			value.Local, e = strconv.ParseUint(local, 16, 64)
			if e != nil {
				return verification, e
			}
			value.ID = ""
			if numericTable(name) {
				value.Key = ""
			}
		}
		if e = json.NewEncoder(expected).Encode(value); e != nil {
			return verification, e
		}
		if _, e = writer.Write([]row{value}); e != nil {
			return verification, e
		}
		index++
	}
	if index > 0 && directory != nil {
		current.End = index
		if _, err = directory.Write([]rowRange{storedRange(name, current)}); err != nil {
			return verification, err
		}
	}
	closeErr := writer.Close()
	if directory != nil {
		closeErr = errors.Join(closeErr, directory.Close())
	}
	writer = nil
	directory = nil
	if closeErr != nil {
		return verification, closeErr
	}
	verification, err = verifyTable(ctx, dir, name, hex.EncodeToString(expected.Sum(nil)), index)
	if err != nil {
		return verification, err
	}
	return verification, errors.Join(os.Remove(input), os.Remove(sorted))
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
			if n == 0 {
				f.cache.mu.Unlock()
				return total, io.EOF
			}
			bytes = bytes[:n]
			element = f.cache.lru.PushFront(cacheEntry{key: key, bytes: bytes})
			f.cache.entries[key] = element
			f.cache.used += int64(cap(bytes))
			for f.cache.used > f.cache.limit && f.cache.lru.Len() > 1 {
				last := f.cache.lru.Back()
				entry := last.Value.(cacheEntry)
				delete(f.cache.entries, entry.key)
				f.cache.used -= int64(cap(entry.bytes))
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

type tableFile struct {
	file    *os.File
	parquet *parquet.File
}

func openTable(path string, cache *blockCache) (*tableFile, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	stat, err := file.Stat()
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	parsed, err := parquet.OpenFile(cachedFile{file, cache}, stat.Size())
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return &tableFile{file: file, parquet: parsed}, nil
}
func openRows[T any](path string, cache *blockCache) (*parquet.GenericReader[T], *os.File, error) {
	opened, err := openTable(path, cache)
	if err != nil {
		return nil, nil, err
	}
	if !parquet.EqualNodes(parquet.SchemaOf(new(T)), opened.parquet.Schema()) {
		return nil, nil, errors.Join(fmt.Errorf("unexpected table schema: %s", path), opened.file.Close())
	}
	return parquet.NewGenericReader[T](opened.parquet), opened.file, nil
}

func tableRange(table *tableFile, key string) (result rowRange, found bool, err error) {
	reader := parquet.NewGenericReader[rowRange](table.parquet)
	defer func() { err = errors.Join(err, reader.Close()) }()
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
		if rangeKey(one[0]) < key {
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
	if rangeKey(result) != key {
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
	return scanReader(ctx, reader, span, visit)
}
func (s *Segment) scan(ctx context.Context, name string, span *rowRange, visit func(row) error) (err error) {
	reader := parquet.NewGenericReader[row](s.tables[name].parquet)
	defer func() { err = errors.Join(err, reader.Close()) }()
	return scanReader(ctx, reader, span, visit)
}
func scanReader(ctx context.Context, reader *parquet.GenericReader[row], span *rowRange, visit func(row) error) (err error) {
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

func servingRangeTable(name string) bool {
	return name == "names" || name == "paths" || name == "terminals"
}
func newRangeDirectory(dir, name string, options []parquet.WriterOption) (*parquet.GenericWriter[rowRange], *os.File, error) {
	if !servingRangeTable(name) && name != "out" && name != "in" {
		return nil, nil, nil
	}
	file, err := os.Create(filepath.Join(dir, name+".ranges.parquet"))
	if err != nil {
		return nil, nil, err
	}
	return parquet.NewGenericWriter[rowRange](file, options...), file, nil
}

func graphTable(name string) bool {
	switch name {
	case "nodes", "ids", "names", "qnames", "paths", "edges", "out", "in", "terminals", "kinds":
		return true
	}
	return false
}
func numericTable(name string) bool {
	switch name {
	case "nodes", "edges", "out", "in", "terminals":
		return true
	}
	return false
}
func localKey(id uint64) string { return fmt.Sprintf("%016x", id) }
func storedRange(name string, r rowRange) rowRange {
	if numericTable(name) {
		r.Numeric = true
		key := r.Key
		if name == "out" || name == "in" {
			r.Grouped = true
			r.Tag, _ = strconv.ParseUint(key[:16], 16, 64)
			key = key[16:]
		}
		r.Number, _ = strconv.ParseUint(key, 16, 64)
		r.Key = ""
	}
	return r
}
func rangeKey(r rowRange) string {
	if r.Numeric {
		if r.Grouped {
			return localKey(r.Tag) + localKey(r.Number)
		}
		return localKey(r.Number)
	}
	return r.Key
}

const offsetHeaderSize = 24

type offsetDirectory struct {
	file  *os.File
	width uint64
}

func offsetWidth(rows uint64) uint64 {
	if rows <= math.MaxUint32 {
		return 4
	}
	return 8
}
func decodeOffset(raw []byte, width uint64) uint64 {
	if width == 4 {
		return uint64(binary.LittleEndian.Uint32(raw))
	}
	return binary.LittleEndian.Uint64(raw)
}

// Dense N+1 offsets include zero-degree nodes and unresolved terminals.
func writeOffsets(ctx context.Context, dir, name string, count, kinds, rows uint64) error {
	return writeOffsetsWidth(ctx, dir, name, count, kinds, offsetWidth(rows))
}
func writeOffsetsWidth(ctx context.Context, dir, name string, count, kinds, width uint64) (err error) {
	cache := newBlockCache(4 << 20)
	reader, file, err := openRows[rowRange](filepath.Join(dir, name+".ranges.parquet"), cache)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, reader.Close(), file.Close()) }()
	output, err := os.Create(filepath.Join(dir, name+".offsets"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, output.Close()) }()
	buffer := bufio.NewWriter(output)
	one := make([]rowRange, 1)
	next, e := reader.Read(one)
	if e != nil && e != io.EOF {
		return e
	}
	offset := uint64(0)
	if err = binary.Write(buffer, binary.LittleEndian, []uint64{count, kinds, width}); err != nil {
		return err
	}
	for tag := uint64(0); tag < kinds; tag++ {
		for id := uint64(0); id <= count; id++ {
			if err = ctx.Err(); err != nil {
				return err
			}
			if width == 4 {
				if offset > math.MaxUint32 {
					return fmt.Errorf("offset exceeds directory width: %s", name)
				}
				err = binary.Write(buffer, binary.LittleEndian, uint32(offset))
			} else {
				err = binary.Write(buffer, binary.LittleEndian, offset)
			}
			if err != nil {
				return err
			}
			if next == 1 && one[0].Tag == tag && one[0].Number == id {
				offset = uint64(one[0].End)
				next, e = reader.Read(one)
				if e != nil && e != io.EOF {
					return e
				}
			}
		}
	}
	if next != 0 {
		return fmt.Errorf("offset endpoint outside local IDs")
	}
	return buffer.Flush()
}

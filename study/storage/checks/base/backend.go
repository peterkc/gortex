// Package base is a throwaway Parquet study adapter, not a product backend.
package base

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/blevesearch/bleve/v2"
	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/vfs"
	"github.com/parquet-go/parquet-go"
	"gxstudy/adapter"
	"gxstudy/dataset"
)

const cacheBytes int64 = 256 << 20
const format = "gxstudy-parquet-base/v1"

type Backend struct{ SideIndex bool }

func (b Backend) Name() string {
	if b.SideIndex {
		return "parquet-side"
	}
	return "parquet-base"
}
func (b Backend) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{
		Operations: map[string]string{"id": "sorted Parquet ID directory and row range", "name": "sorted Parquet exact-name directory and ID-ordered row range", "file": "sorted Parquet file directory and row range", "in": "CSC-style endpoint ranges; kind filter over full rows", "out": "CSR-style endpoint ranges; kind filter over full rows", "symbols": "native Bleve BM25, identical to B", "content": "native Bleve BM25, identical to B", "vector": "emulated exact cosine scan of Parquet records", "delta": "emulated full rebuild from Parquet records"},
		Ranking:    map[string]adapter.RankPolicy{"symbols": {Mode: "native"}, "content": {Mode: "native"}, "multi_symbols": {Mode: "native"}, "vector": {Exact: true}, "multi_vector": {Exact: true}},
		Libraries:  []string{"github.com/parquet-go/parquet-go v0.30.1", "github.com/blevesearch/bleve/v2 v2.6.1", "github.com/cockroachdb/pebble/v2 v2.1.7 (side branch only)"},
		Deployment: "test-only per-repository sealed Parquet directory; reference JSON catalog",
		Notes:      []string{"GraphAr-style sorted full-payload tables with sparse endpoint range directories, not GraphAr SDK/wire compatibility or dense N+1 offsets.", "Shared 256 MiB serving-cache payload budget across all repositories: Parquet only, or 128 MiB Parquet plus 128 MiB Pebble. No answer cache or resident graph. Parsed Parquet metadata and handles survive for the Segment lifetime; OS cache, metadata and identical Bleve memory are outside that budget and included in RSS/cgroup peaks.", "Full-payload lookup copies and both adjacency copies count in serving bytes; external-sort CPU and temporary disk belong to construction.", "Build/open/Reader subset only. Product catalog, daemon, retire and recovery are not established."},
	}
}

type sealEntry struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	Category string `json:"category"`
}
type seal struct {
	Verification map[string]tableVerification `json:"verification"`
	Format       string                       `json:"format"`
	Repo         string                       `json:"repo"`
	SideIndex    bool                         `json:"side_index"`
	Meta         dataset.Manifest             `json:"meta"`
	Files        []sealEntry                  `json:"files"`
}
type Segment struct {
	dir              string
	seal             seal
	cache            *blockCache
	tables           map[string]*tableFile
	side             *pebble.DB
	symbols, content bleve.Index
	once             sync.Once
	closeErr         error
}

var caches struct {
	sync.Mutex
	users   int
	parquet *blockCache
	pebble  *pebble.Cache
	side    bool
}

func acquireCaches(side bool) (*blockCache, *pebble.Cache, error) {
	caches.Lock()
	defer caches.Unlock()
	if caches.users > 0 && side != caches.side {
		return nil, nil, fmt.Errorf("cannot mix cache modes in one process")
	}
	if caches.users == 0 {
		limit := cacheBytes
		if side {
			limit /= 2
		}
		caches.parquet = newBlockCache(limit)
		caches.side = side
		if side {
			caches.pebble = pebble.NewCache(cacheBytes / 2)
		}
	}
	caches.users++
	return caches.parquet, caches.pebble, nil
}
func releaseCaches() {
	caches.Lock()
	defer caches.Unlock()
	caches.users--
	if caches.users == 0 {
		if caches.pebble != nil {
			caches.pebble.Unref()
		}
		caches.parquet = nil
		caches.pebble = nil
	}
}
func indexKey(prefix string, fields ...string) []byte {
	var key strings.Builder
	key.WriteString(prefix)
	key.WriteByte('/')
	for _, field := range fields {
		key.WriteString(base64.RawURLEncoding.EncodeToString([]byte(field)))
		key.WriteByte('/')
	}
	return []byte(key.String())
}

type sideWriter struct {
	db    *pebble.DB
	batch *pebble.Batch
	cache *pebble.Cache
}

func newSideWriter(path string) (*sideWriter, error) {
	cache := pebble.NewCache(cacheBytes / 2)
	db, err := pebble.Open(path, &pebble.Options{Cache: cache})
	if err != nil {
		cache.Unref()
		return nil, err
	}
	return &sideWriter{db: db, batch: db.NewBatch(), cache: cache}, nil
}
func (s *sideWriter) add(n dataset.Node, raw []byte) error {
	for _, key := range [][]byte{indexKey("N", n.ID), indexKey("A", n.Name, n.ID), indexKey("P", n.FilePath, n.ID)} {
		if err := s.batch.Set(key, raw, nil); err != nil {
			return err
		}
	}
	if s.batch.Len() >= 4<<20 {
		if err := s.batch.Commit(pebble.Sync); err != nil {
			return err
		}
		s.batch.Reset()
	}
	return nil
}
func (s *sideWriter) finish() error {
	if err := s.batch.Commit(pebble.Sync); err != nil {
		return err
	}
	return s.db.Flush()
}
func (s *sideWriter) close() error {
	err := errors.Join(s.batch.Close(), s.db.Close())
	s.cache.Unref()
	return err
}

type inputRows struct {
	nodes   iter.Seq2[dataset.Node, error]
	edges   iter.Seq2[dataset.Edge, error]
	files   iter.Seq2[dataset.File, error]
	symbols iter.Seq2[dataset.Symbol, error]
	content iter.Seq2[dataset.Content, error]
	vectors iter.Seq2[dataset.Vector, error]
}

func ingestRows(ctx context.Context, input inputRows, spools map[string]*rowSpool, side *sideWriter, addSymbol, addContent func(string, string) error) (counts dataset.Counts, err error) {
	put := func(table, key, id string, raw []byte) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		return spools[table].put(row{Key: key, ID: id, Raw: raw})
	}
	for n, e := range input.nodes {
		if e != nil {
			return counts, e
		}
		raw, e := json.Marshal(n)
		if e != nil {
			return counts, e
		}
		for _, v := range []struct{ table, key string }{{"nodes", n.ID}, {"names", n.Name}, {"paths", n.FilePath}} {
			if e = put(v.table, v.key, n.ID, raw); e != nil {
				return counts, e
			}
		}
		if side != nil {
			if e = side.add(n, raw); e != nil {
				return counts, e
			}
		}
		counts.Nodes++
	}
	for edge, e := range input.edges {
		if e != nil {
			return counts, e
		}
		raw, e := json.Marshal(edge)
		if e != nil {
			return counts, e
		}
		if e = put("out", edge.From, edge.Kind+"\x00"+edge.To, raw); e != nil {
			return counts, e
		}
		if e = put("in", edge.To, edge.Kind+"\x00"+edge.From, raw); e != nil {
			return counts, e
		}
		counts.Edges++
		if edge.CrossRepo {
			counts.CrossEdges++
		}
	}
	for f, e := range input.files {
		if e != nil {
			return counts, e
		}
		raw, e := json.Marshal(f)
		if e != nil {
			return counts, e
		}
		if e = put("files", f.Path, f.Path, raw); e != nil {
			return counts, e
		}
		counts.Files++
	}
	for s, e := range input.symbols {
		if e != nil {
			return counts, e
		}
		raw, e := json.Marshal(s)
		if e != nil {
			return counts, e
		}
		if e = put("symbols", s.NodeID, s.NodeID, raw); e != nil {
			return counts, e
		}
		if e = addSymbol(s.NodeID, s.Tokens); e != nil {
			return counts, e
		}
		counts.Symbols++
	}
	for c, e := range input.content {
		if e != nil {
			return counts, e
		}
		raw, e := json.Marshal(c)
		if e != nil {
			return counts, e
		}
		if e = put("content", c.ChunkID, c.ChunkID, raw); e != nil {
			return counts, e
		}
		if e = addContent(c.ChunkID, c.Body); e != nil {
			return counts, e
		}
		counts.Content++
	}
	for v, e := range input.vectors {
		if e != nil {
			return counts, e
		}
		if e = validVector(v.Values); e != nil {
			return counts, e
		}
		raw, e := json.Marshal(v)
		if e != nil {
			return counts, e
		}
		if e = put("vectors", v.NodeID, v.NodeID, raw); e != nil {
			return counts, e
		}
		counts.Vectors++
	}
	return counts, nil
}

// Sealed read-only opens may coexist, as in the unchanged B adapter.
type sealedFS struct {
	vfs.FS
	lockPath string
}
type noLock struct{}

func (noLock) Close() error { return nil }
func (f sealedFS) Lock(path string) (io.Closer, error) {
	if path != f.lockPath {
		return nil, fmt.Errorf("unexpected sealed lock path")
	}
	return noLock{}, nil
}

func (b Backend) Build(ctx context.Context, d dataset.Dataset, dir string) (ref adapter.SegmentRef, stats adapter.BuildStats, err error) {
	if err := ctx.Err(); err != nil {
		return ref, stats, err
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return ref, stats, err
	}
	dir = absolute
	if err = os.Mkdir(dir, 0755); err != nil {
		return ref, stats, err
	}
	scratch, err := os.MkdirTemp(dir, "sort-")
	if err != nil {
		return ref, stats, err
	}
	scratchOwned := true
	defer func() {
		if scratchOwned {
			err = errors.Join(err, os.RemoveAll(scratch))
		}
	}()
	names := []string{"nodes", "names", "paths", "out", "in", "files", "symbols", "content", "vectors"}
	spools := map[string]*rowSpool{}
	// Keep closure errors visible, including before a private build can seal.
	closeSpools := func() error {
		var e error
		for name, spool := range spools {
			e = errors.Join(e, spool.close())
			delete(spools, name)
		}
		return e
	}
	for _, name := range names {
		spool, e := newRowSpool(filepath.Join(scratch, name+".input"))
		if e != nil {
			return ref, stats, errors.Join(e, closeSpools())
		}
		spools[name] = spool
	}
	var side *sideWriter
	if b.SideIndex {
		side, err = newSideWriter(filepath.Join(dir, "side"))
		if err != nil {
			return ref, stats, errors.Join(err, closeSpools())
		}
	}
	var counts dataset.Counts
	err = BuildText(ctx, filepath.Join(dir, "symbols.bleve"), func(addSymbol func(string, string) error) error {
		return BuildText(ctx, filepath.Join(dir, "content.bleve"), func(addContent func(string, string) error) error {
			var e error
			counts, e = ingestRows(ctx, inputRows{d.NodeRows(), d.EdgeRows(), d.FileRows(), d.SymbolRows(), d.ContentRows(), d.VectorRows()}, spools, side, addSymbol, addContent)
			return e
		})
	})
	err = errors.Join(err, closeSpools())
	if side != nil {
		if err == nil {
			err = side.finish()
		}
		err = errors.Join(err, side.close())
	}
	if err != nil {
		return ref, stats, err
	}
	verifications := map[string]tableVerification{}
	for _, name := range names {
		unique := name != "names" && name != "paths" && name != "out" && name != "in"
		verified, e := writeSortedTable(ctx, dir, scratch, name, unique)
		if e != nil {
			return ref, stats, e
		}
		verifications[name] = verified
	}
	if err = os.RemoveAll(scratch); err != nil {
		return ref, stats, err
	}
	scratchOwned = false
	meta := d.Meta
	counts.SkippedSpans = meta.Counts.SkippedSpans
	meta.Counts = counts
	manifest := seal{Verification: verifications, Format: format, Repo: d.Repo, SideIndex: b.SideIndex, Meta: meta}
	var dirs []string
	err = filepath.WalkDir(dir, func(path string, de fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if de.IsDir() {
			dirs = append(dirs, path)
			return nil
		}
		info, e := de.Info()
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("nonregular serving artifact")
		}
		name, e := filepath.Rel(dir, path)
		if e != nil {
			return e
		}
		hash, e := dataset.Hash(path)
		if e != nil {
			return e
		}
		category := "graph"
		if strings.HasPrefix(name, "vectors.") {
			category = "vectors"
		}
		if strings.HasPrefix(name, "symbols.") || strings.HasPrefix(name, "content.") {
			category = "text"
		}
		manifest.Files = append(manifest.Files, sealEntry{Name: name, Size: info.Size(), SHA256: hash, Category: category})
		stats.Bytes += info.Size()
		return syncPath(path)
	})
	if err != nil {
		return ref, stats, err
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return ref, stats, err
	}
	if err = os.WriteFile(filepath.Join(dir, "seal.json"), raw, 0644); err != nil {
		return ref, stats, err
	}
	if err = syncPath(filepath.Join(dir, "seal.json")); err != nil {
		return ref, stats, err
	}
	slices.Reverse(dirs)
	dirs = append(dirs, filepath.Dir(dir))
	for _, path := range dirs {
		if err = syncPath(path); err != nil {
			return ref, stats, err
		}
	}
	hash := sha256.Sum256(raw)
	digest := hex.EncodeToString(hash[:])
	stats.Mode = "full"
	stats.Bytes += int64(len(raw))
	stats.Notes = []string{"full Parquet sort/compress/seal plus identical Bleve; side index included when selected"}
	return adapter.SegmentRef{Repo: d.Repo, Version: digest, Digest: digest, Path: dir}, stats, nil
}
func syncPath(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}
func (b Backend) Open(ctx context.Context, dir string, ref adapter.SegmentRef) (adapter.Segment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	if dir != ref.Path || canonical != dir || ref.Version != ref.Digest {
		return nil, fmt.Errorf("segment reference/path alias mismatch")
	}
	sealPath, err := dataset.SafeFile(dir, "seal.json")
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(sealPath)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(raw)
	if hex.EncodeToString(hash[:]) != ref.Digest {
		return nil, fmt.Errorf("seal digest mismatch")
	}
	var manifest seal
	if err = json.Unmarshal(raw, &manifest); err != nil {
		return nil, err
	}
	if manifest.Format != format || manifest.Repo != ref.Repo || manifest.SideIndex != b.SideIndex {
		return nil, fmt.Errorf("seal identity mismatch")
	}
	seen := map[string]bool{}
	for _, entry := range manifest.Files {
		if seen[entry.Name] {
			return nil, fmt.Errorf("duplicate seal entry")
		}
		seen[entry.Name] = true
		path, e := dataset.SafeFile(dir, entry.Name)
		if e != nil {
			return nil, e
		}
		info, e := os.Stat(path)
		if e != nil {
			return nil, e
		}
		if info.Size() != entry.Size {
			return nil, fmt.Errorf("artifact size mismatch: %s", entry.Name)
		}
		// Like B, immutable payload hashes are bound at seal; every Open checks sizes.
	}
	for _, name := range []string{"nodes", "names", "paths", "out", "in", "files", "symbols", "content", "vectors"} {
		for _, suffix := range []string{".parquet", ".ranges.parquet"} {
			if !seen[name+suffix] {
				return nil, fmt.Errorf("missing required table %s", name+suffix)
			}
		}
	}
	counts := manifest.Meta.Counts
	expectedRows := map[string]int{"nodes": counts.Nodes, "names": counts.Nodes, "paths": counts.Nodes, "out": counts.Edges, "in": counts.Edges, "files": counts.Files, "symbols": counts.Symbols, "content": counts.Content, "vectors": counts.Vectors}
	for name, rows := range expectedRows {
		verification, ok := manifest.Verification[name]
		_, hashError := hex.DecodeString(verification.InputSHA256)
		if !ok || len(verification.InputSHA256) != 64 || hashError != nil || verification.InputSHA256 != verification.OutputSHA256 || verification.Rows != int64(rows) || verification.CPU < 0 {
			return nil, fmt.Errorf("missing/invalid verification witness: %s", name)
		}
	}
	cache, pCache, err := acquireCaches(b.SideIndex)
	if err != nil {
		return nil, err
	}
	s := &Segment{dir: dir, seal: manifest, cache: cache, tables: map[string]*tableFile{}}
	fail := func(e error) (adapter.Segment, error) { return nil, errors.Join(e, s.Close()) }
	for _, name := range []string{"nodes", "names", "paths", "out", "in", "files", "symbols", "content", "vectors"} {
		for _, suffix := range []string{"", ".ranges"} {
			key := name + suffix
			table, e := openTable(filepath.Join(dir, key+".parquet"), cache)
			if e != nil {
				return fail(e)
			}
			s.tables[key] = table
			schema := parquet.SchemaOf(new(row))
			if suffix != "" {
				schema = parquet.SchemaOf(new(rowRange))
			}
			if !parquet.EqualNodes(schema, table.parquet.Schema()) {
				return fail(fmt.Errorf("unexpected table schema: %s", key))
			}
		}
	}
	if b.SideIndex {
		s.side, err = pebble.Open(filepath.Join(dir, "side"), &pebble.Options{ReadOnly: true, Cache: pCache, FS: sealedFS{FS: vfs.Default, lockPath: filepath.Join(dir, "side", "LOCK")}})
		if err != nil {
			return fail(err)
		}
	}
	s.symbols, err = bleve.OpenUsing(filepath.Join(dir, "symbols.bleve"), map[string]interface{}{"read_only": true})
	if err != nil {
		return fail(err)
	}
	s.content, err = bleve.OpenUsing(filepath.Join(dir, "content.bleve"), map[string]interface{}{"read_only": true})
	if err != nil {
		return fail(err)
	}
	return s, nil
}
func (s *Segment) Close() error {
	s.once.Do(func() {
		for _, table := range s.tables {
			s.closeErr = errors.Join(s.closeErr, table.file.Close())
		}
		if s.content != nil {
			s.closeErr = errors.Join(s.closeErr, s.content.Close())
		}
		if s.symbols != nil {
			s.closeErr = errors.Join(s.closeErr, s.symbols.Close())
		}
		if s.side != nil {
			s.closeErr = errors.Join(s.closeErr, s.side.Close())
		}
		releaseCaches()
	})
	return s.closeErr
}
func (s *Segment) sideNodes(ctx context.Context, prefix []byte) (nodes []dataset.Node, err error) {
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	iterator, err := s.side.NewIter(&pebble.IterOptions{LowerBound: prefix})
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, iterator.Close()) }()
	for ok := iterator.First(); ok && bytes.HasPrefix(iterator.Key(), prefix); ok = iterator.Next() {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		var n dataset.Node
		if err = json.Unmarshal(iterator.Value(), &n); err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, iterator.Error()
}

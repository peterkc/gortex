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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/blevesearch/bleve/v2"
	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/vfs"
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
		Notes:      []string{"GraphAr-style sorted full-payload tables with sparse endpoint range directories, not GraphAr SDK/wire compatibility or dense N+1 offsets.", "Shared 256 MiB serving-cache payload budget across all repositories: Parquet only, or 128 MiB Parquet plus 128 MiB Pebble. No answer cache or resident graph. OS cache, metadata and identical Bleve memory are outside that budget and included in RSS/cgroup peaks.", "Full-payload lookup copies and both adjacency copies count in serving bytes; external-sort CPU and temporary disk belong to construction.", "Build/open/Reader subset only. Product catalog, daemon, retire and recovery are not established."},
	}
}

type sealEntry struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	Category string `json:"category"`
}
type seal struct {
	RoundTrip bool             `json:"round_trip_verified"`
	Format    string           `json:"format"`
	Repo      string           `json:"repo"`
	SideIndex bool             `json:"side_index"`
	Meta      dataset.Manifest `json:"meta"`
	Files     []sealEntry      `json:"files"`
}
type Segment struct {
	dir              string
	seal             seal
	cache            *blockCache
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
func buildSide(ctx context.Context, d dataset.Dataset, path string) (err error) {
	cache := pebble.NewCache(cacheBytes / 2)
	defer cache.Unref()
	db, err := pebble.Open(path, &pebble.Options{Cache: cache})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	batch := db.NewBatch()
	defer func() { err = errors.Join(err, batch.Close()) }()
	for n, e := range d.NodeRows() {
		if e != nil {
			return e
		}
		if e = ctx.Err(); e != nil {
			return e
		}
		raw, e := json.Marshal(n)
		if e != nil {
			return e
		}
		for _, key := range [][]byte{indexKey("N", n.ID), indexKey("A", n.Name, n.ID), indexKey("P", n.FilePath, n.ID)} {
			if e = batch.Set(key, raw, nil); e != nil {
				return e
			}
		}
		if batch.Len() >= 4<<20 {
			if e = batch.Commit(pebble.Sync); e != nil {
				return e
			}
			batch.Reset()
		}
	}
	if err = batch.Commit(pebble.Sync); err != nil {
		return err
	}
	return db.Flush()
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

func (b Backend) Build(ctx context.Context, d dataset.Dataset, dir string) (adapter.SegmentRef, adapter.BuildStats, error) {
	var ref adapter.SegmentRef
	var stats adapter.BuildStats
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
	for v, e := range d.VectorRows() {
		if e != nil {
			return ref, stats, e
		}
		if e = validVector(v.Values); e != nil {
			return ref, stats, e
		}
	}
	jobs := []struct {
		name string
		rows func() error
	}{
		{"nodes", func() error {
			return writeTable(ctx, dir, "nodes", encodedRows(d.NodeRows(), func(n dataset.Node) (string, string) { return n.ID, n.ID }), true)
		}},
		{"names", func() error {
			return writeTable(ctx, dir, "names", encodedRows(d.NodeRows(), func(n dataset.Node) (string, string) { return n.Name, n.ID }), false)
		}},
		{"paths", func() error {
			return writeTable(ctx, dir, "paths", encodedRows(d.NodeRows(), func(n dataset.Node) (string, string) { return n.FilePath, n.ID }), false)
		}},
		{"out", func() error {
			return writeTable(ctx, dir, "out", encodedRows(d.EdgeRows(), func(e dataset.Edge) (string, string) { return e.From, e.Kind + "\x00" + e.To }), false)
		}},
		{"in", func() error {
			return writeTable(ctx, dir, "in", encodedRows(d.EdgeRows(), func(e dataset.Edge) (string, string) { return e.To, e.Kind + "\x00" + e.From }), false)
		}},
		{"files", func() error {
			return writeTable(ctx, dir, "files", encodedRows(d.FileRows(), func(f dataset.File) (string, string) { return f.Path, f.Path }), true)
		}},
		{"symbols", func() error {
			return writeTable(ctx, dir, "symbols", encodedRows(d.SymbolRows(), func(s dataset.Symbol) (string, string) { return s.NodeID, s.NodeID }), true)
		}},
		{"content", func() error {
			return writeTable(ctx, dir, "content", encodedRows(d.ContentRows(), func(c dataset.Content) (string, string) { return c.ChunkID, c.ChunkID }), true)
		}},
		{"vectors", func() error {
			return writeTable(ctx, dir, "vectors", encodedRows(d.VectorRows(), func(v dataset.Vector) (string, string) { return v.NodeID, v.NodeID }), true)
		}},
	}
	for _, job := range jobs {
		if err = job.rows(); err != nil {
			return ref, stats, fmt.Errorf("build %s: %w", job.name, err)
		}
	}
	if b.SideIndex {
		if err = buildSide(ctx, d, filepath.Join(dir, "side")); err != nil {
			return ref, stats, err
		}
	}
	if err = buildText(ctx, filepath.Join(dir, "symbols.bleve"), func(add func(string, string) error) error {
		for s, e := range d.SymbolRows() {
			if e != nil {
				return e
			}
			if e = add(s.NodeID, s.Tokens); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		return ref, stats, err
	}
	if err = buildText(ctx, filepath.Join(dir, "content.bleve"), func(add func(string, string) error) error {
		for c, e := range d.ContentRows() {
			if e != nil {
				return e
			}
			if e = add(c.ChunkID, c.Body); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		return ref, stats, err
	}
	meta := d.Meta
	if meta.Counts, err = dataset.CountRows(d); err != nil {
		return ref, stats, err
	}
	manifest := seal{RoundTrip: true, Format: format, Repo: d.Repo, SideIndex: b.SideIndex, Meta: meta}
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
	if !manifest.RoundTrip || manifest.Format != format || manifest.Repo != ref.Repo || manifest.SideIndex != b.SideIndex {
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
	cache, pCache, err := acquireCaches(b.SideIndex)
	if err != nil {
		return nil, err
	}
	s := &Segment{dir: dir, seal: manifest, cache: cache}
	fail := func(e error) (adapter.Segment, error) { return nil, errors.Join(e, s.Close()) }
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

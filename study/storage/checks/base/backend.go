// Package base is a throwaway Parquet study adapter, not a product backend.
package base

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
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
	"gxstudy/runner"
)

const cacheBytes int64 = 256 << 20
const format = "gxstudy-parquet-base/v2"

type Backend struct{ SideIndex bool }

func (b Backend) Name() string {
	if b.SideIndex {
		return "parquet-side"
	}
	return "parquet-base"
}
func (b Backend) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{
		Operations: map[string]string{"id": "sorted logical-ID directory to canonical local-ID row", "name": "sorted exact-name local-ID candidates to canonical rows", "file": "sorted Parquet file directory and row range", "in": "CSC local-ID references with N+1 offsets; kind filter after dereference", "out": "CSR local-ID references with N+1 offsets; kind filter after dereference", "symbols": "native Bleve BM25, identical to B", "content": "native Bleve BM25, identical to B", "vector": "emulated exact cosine scan of Parquet records", "delta": "emulated full rebuild from Parquet records"},
		Ranking:    map[string]adapter.RankPolicy{"symbols": {Mode: "native"}, "content": {Mode: "native"}, "multi_symbols": {Mode: "native"}, "vector": {Exact: true}, "multi_vector": {Exact: true}},
		Libraries:  []string{"github.com/parquet-go/parquet-go v0.30.1", "github.com/blevesearch/bleve/v2 v2.6.1", "github.com/cockroachdb/pebble/v2 v2.1.7 (side branch only)"},
		Deployment: "test-only per-repository sealed Parquet directory; reference JSON catalog",
		Notes:      []string{"Dense local-ID canonical binary payload rows, ID-only lookup tables, and dense N+1 endpoint offsets; not GraphAr SDK/wire compatibility.", "Shared 256 MiB serving-cache payload budget across all repositories: Parquet only, or 128 MiB Parquet plus 128 MiB Pebble. No answer cache or resident graph. Parsed Parquet metadata and handles survive for the Segment lifetime; OS cache, metadata and identical Bleve memory are outside that budget and included in RSS/cgroup peaks.", "Canonical payloads, ID-only lookup copies, dense offset directories and optional side index count in serving bytes; external-sort CPU and temporary disk belong to construction.", "Build/open/Reader subset only. Product catalog, daemon, retire and recovery are not established."},
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
	offsets          map[string]*os.File
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
	identity os.FileInfo
	db       *pebble.DB
	batch    *pebble.Batch
	cache    *pebble.Cache
}

func newSideWriter(path string) (*sideWriter, error) {
	cache := pebble.NewCache(cacheBytes / 2)
	db, err := pebble.Open(path, &pebble.Options{Cache: cache, DisableWAL: true})
	if err != nil {
		cache.Unref()
		return nil, err
	}
	identity, err := os.Lstat(path)
	if err != nil {
		err = errors.Join(err, db.Close())
		cache.Unref()
		return nil, err
	}
	return &sideWriter{db: db, batch: db.NewBatch(), cache: cache, identity: identity}, nil
}
func (s *sideWriter) add(n dataset.Node, local uint64) error {
	hash := sha256.Sum256([]byte(n.ID))
	for _, key := range [][]byte{append([]byte("N/"), hash[:]...), append(indexKey("A", n.Name), []byte(n.ID)...), append(indexKey("Q", n.QualName), localBytes(local)...), append(indexKey("P", n.FilePath), localBytes(local)...)} {
		if err := s.batch.Set(key, localBytes(local), nil); err != nil {
			return err
		}
	}
	if s.batch.Len() >= 4<<20 {
		if err := s.batch.Commit(pebble.NoSync); err != nil {
			return err
		}
		s.batch.Reset()
	}
	return nil
}
func (s *sideWriter) finish() error {
	if err := s.batch.Commit(pebble.NoSync); err != nil {
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
	// This map exists only during construction. Serving resolves through counted
	// on-disk ID rows, including reversible unresolved endpoint identities.
	locals := map[string]uint64{}
	kinds := map[string]uint64{}
	for n, e := range input.nodes {
		if e != nil {
			return counts, e
		}
		if _, exists := locals[n.ID]; n.ID == "" || exists {
			return counts, fmt.Errorf("empty or duplicate node ID %q", n.ID)
		}
		local := uint64(counts.Nodes)
		locals[n.ID] = local
		raw, e := encodeNode(n)
		if e != nil {
			return counts, e
		}
		if e = put("nodes", localKey(local), localKey(local), raw); e != nil {
			return counts, e
		}
		for _, v := range []struct{ table, key string }{{"ids", n.ID}, {"names", n.Name}, {"qnames", n.QualName}, {"paths", n.FilePath}} {
			sortID := localKey(local)
			if v.table == "names" {
				// Hex preserves logical-ID order; NUL sorts before extensions.
				// The suffix alone survives as the fixed-width Local column.
				sortID = hex.EncodeToString([]byte(n.ID)) + "\x00" + sortID
			}
			if e = put(v.table, v.key, sortID, nil); e != nil {
				return counts, e
			}
		}
		if side != nil {
			if e = side.add(n, local); e != nil {
				return counts, e
			}
		}
		counts.Nodes++
	}
	endpoint := func(id string) (uint64, error) {
		if local, exists := locals[id]; exists {
			return local, nil
		}
		local := uint64(len(locals))
		locals[id] = local
		if e := put("ids", id, localKey(local), nil); e != nil {
			return 0, e
		}
		return local, put("terminals", localKey(local), localKey(local), []byte(id))
	}
	for edge, e := range input.edges {
		if e != nil {
			return counts, e
		}
		from, e := endpoint(edge.From)
		if e != nil {
			return counts, e
		}
		to, e := endpoint(edge.To)
		if e != nil {
			return counts, e
		}
		tag, exists := kinds[edge.Kind]
		if !exists {
			tag = uint64(len(kinds))
			kinds[edge.Kind] = tag
			if e = put("kinds", edge.Kind, localKey(tag), nil); e != nil {
				return counts, e
			}
		}
		local := uint64(counts.Edges)
		raw, e := encodeEdge(edge, from, to)
		if e != nil {
			return counts, e
		}
		if e = put("edges", localKey(local), localKey(local), raw); e != nil {
			return counts, e
		}
		if e = put("out", localKey(tag)+localKey(from), localKey(to)+edge.FilePath+"\x00"+localKey(uint64(edge.Line))+localKey(local), nil); e != nil {
			return counts, e
		}
		if e = put("in", localKey(tag)+localKey(to), localKey(from)+edge.FilePath+"\x00"+localKey(uint64(edge.Line))+localKey(local), nil); e != nil {
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
	names := []string{"nodes", "ids", "names", "qnames", "paths", "edges", "out", "in", "terminals", "kinds", "files", "symbols", "content", "vectors"}
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
		if err == nil {
			err = runner.RemoveEmptyBuildWALs(filepath.Join(dir, "side"), side.identity)
		}
	}
	if err != nil {
		return ref, stats, err
	}
	verifications := map[string]tableVerification{}
	for _, name := range names {
		unique := name != "names" && name != "qnames" && name != "paths" && name != "out" && name != "in"
		verified, e := writeSortedTable(ctx, dir, scratch, name, unique)
		if e != nil {
			return ref, stats, e
		}
		verifications[name] = verified
	}
	for _, name := range []string{"out", "in"} {
		if err = writeOffsets(ctx, dir, name, uint64(verifications["ids"].Rows), uint64(verifications["kinds"].Rows)); err != nil {
			return ref, stats, err
		}
		if err = os.Remove(filepath.Join(dir, name+".ranges.parquet")); err != nil {
			return ref, stats, err
		}
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
			if strings.HasSuffix(entry.Name, ".offsets") {
				return nil, fmt.Errorf("truncated offset directory: %s", entry.Name)
			}
			return nil, fmt.Errorf("artifact size mismatch: %s", entry.Name)
		}
		// Like B, immutable payload hashes are bound at seal; every Open checks sizes.
	}
	for _, name := range []string{"nodes", "ids", "names", "qnames", "paths", "edges", "out", "in", "terminals", "kinds", "files", "symbols", "content", "vectors"} {
		for _, suffix := range []string{".parquet", ".ranges.parquet"} {
			if !servingRangeTable(name) && suffix == ".ranges.parquet" {
				continue
			}
			if !seen[name+suffix] {
				return nil, fmt.Errorf("missing required table %s", name+suffix)
			}
		}
	}
	counts := manifest.Meta.Counts
	expectedRows := map[string]int{"nodes": counts.Nodes, "names": counts.Nodes, "qnames": counts.Nodes, "paths": counts.Nodes, "edges": counts.Edges, "out": counts.Edges, "in": counts.Edges, "files": counts.Files, "symbols": counts.Symbols, "content": counts.Content, "vectors": counts.Vectors}
	expectedRows["kinds"] = int(manifest.Verification["kinds"].Rows)
	expectedRows["ids"] = int(manifest.Verification["ids"].Rows)
	expectedRows["terminals"] = expectedRows["ids"] - counts.Nodes
	if expectedRows["terminals"] < 0 {
		return nil, fmt.Errorf("invalid local ID count")
	}
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
	s := &Segment{dir: dir, seal: manifest, cache: cache, tables: map[string]*tableFile{}, offsets: map[string]*os.File{}}
	fail := func(e error) (adapter.Segment, error) { return nil, errors.Join(e, s.Close()) }
	for _, name := range []string{"nodes", "ids", "names", "qnames", "paths", "edges", "out", "in", "terminals", "kinds", "files", "symbols", "content", "vectors"} {
		for _, suffix := range []string{"", ".ranges"} {
			if !servingRangeTable(name) && suffix == ".ranges" {
				continue
			}
			key := name + suffix
			table, e := openTable(filepath.Join(dir, key+".parquet"), cache)
			if e != nil {
				return fail(e)
			}
			s.tables[key] = table
			if suffix == "" && table.parquet.NumRows() != int64(expectedRows[name]) {
				return fail(fmt.Errorf("table row count differs: %s", name))
			}
			schema := parquet.SchemaOf(new(row))
			if suffix != "" {
				schema = parquet.SchemaOf(new(rowRange))
			}
			if !parquet.EqualNodes(schema, table.parquet.Schema()) {
				return fail(fmt.Errorf("unexpected table schema: %s", key))
			}
		}
	}
	for _, name := range []string{"out", "in"} {
		file, e := os.Open(filepath.Join(dir, name+".offsets"))
		if e != nil {
			return fail(e)
		}
		s.offsets[name] = file
	}
	if err = validateLayoutOnce(dir, ref.Digest, func() error { return s.validateLocalLayout(ctx) }); err != nil {
		return fail(err)
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
		for _, file := range s.offsets {
			s.closeErr = errors.Join(s.closeErr, file.Close())
		}
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
func (s *Segment) sideNodes(ctx context.Context, prefix []byte, limit int) (nodes []dataset.Node, err error) {
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
		local, e := parseLocal(iterator.Value())
		if e != nil {
			return nil, e
		}
		n, e := s.nodeAt(ctx, local)
		if e != nil {
			return nil, e
		}
		nodes = append(nodes, n)
		if limit > 0 && len(nodes) == limit {
			break
		}
	}
	return nodes, iterator.Error()
}

func (s *Segment) offsetSpan(name string, local, tag uint64) (span rowRange, err error) {
	if local >= uint64(s.seal.Verification["ids"].Rows) {
		return span, fmt.Errorf("local endpoint outside base")
	}
	var raw [16]byte
	if _, err = (cachedFile{s.offsets[name], s.cache}).ReadAt(raw[:], 16+int64(tag*(uint64(s.seal.Verification["ids"].Rows)+1)+local)*8); err != nil {
		return span, fmt.Errorf("truncated offset directory: %s: %w", name, err)
	}
	span.First = int64(binary.LittleEndian.Uint64(raw[:8]))
	span.End = int64(binary.LittleEndian.Uint64(raw[8:]))
	if span.First < 0 || span.First > span.End || span.End > s.tables[name].parquet.NumRows() {
		return span, fmt.Errorf("invalid offset directory: %s", name)
	}
	return span, nil
}

// Sealed bases are immutable. Cache only successful validation, keyed by both
// canonical path and seal digest; failed first adoption never marks a base valid.
var validatedLayouts sync.Map

type layoutValidation struct {
	once sync.Once
	err  error
}

func validateLayoutOnce(path, digest string, validate func() error) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	key := struct{ Path, Digest string }{absolute, digest}
	entry, _ := validatedLayouts.LoadOrStore(key, &layoutValidation{})
	state := entry.(*layoutValidation)
	state.once.Do(func() { state.err = validate() })
	if state.err != nil {
		validatedLayouts.CompareAndDelete(key, state)
	}
	return state.err
}

func (s *Segment) validateLocalLayout(ctx context.Context) error {
	for _, name := range []string{"nodes", "edges"} {
		type localRow struct {
			Local uint64 `parquet:"local,plain,uncompressed"`
		}
		reader := parquet.NewGenericReader[localRow](s.tables[name].parquet)
		rows := make([]localRow, 256)
		index := uint64(0)
		var scanErr error
		for {
			if scanErr = ctx.Err(); scanErr != nil {
				break
			}
			n, e := reader.Read(rows)
			for _, r := range rows[:n] {
				if r.Local != index {
					scanErr = fmt.Errorf("duplicate or non-dense local ID: %s", name)
					break
				}
				index++
			}
			if scanErr != nil {
				break
			}
			if e == io.EOF {
				break
			}
			if e != nil {
				scanErr = e
				break
			}
			if n == 0 {
				scanErr = fmt.Errorf("truncated local ID column: %s", name)
				break
			}
		}
		if e := errors.Join(scanErr, reader.Close()); e != nil {
			return e
		}
	}
	count := uint64(s.seal.Verification["ids"].Rows)
	for _, name := range []string{"out", "in"} {
		info, err := os.Stat(filepath.Join(s.dir, name+".offsets"))
		if err != nil {
			return err
		}
		kinds := uint64(s.seal.Verification["kinds"].Rows)
		if info.Size() != 16+int64(kinds*(count+1))*8 {
			return fmt.Errorf("truncated offset directory: %s", name)
		}
		var header [16]byte
		if _, err = s.offsets[name].ReadAt(header[:], 0); err != nil {
			return err
		}
		if binary.LittleEndian.Uint64(header[:8]) != count || binary.LittleEndian.Uint64(header[8:]) != kinds {
			return fmt.Errorf("invalid offset directory header: %s", name)
		}
		reader := io.NewSectionReader(s.offsets[name], 16, info.Size()-16)
		buffer := make([]byte, 64<<10)
		remaining := kinds * (count + 1)
		previous := uint64(0)
		first := true
		for remaining > 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			block := buffer[:min(uint64(len(buffer)), remaining*8)]
			if _, err = io.ReadFull(reader, block); err != nil {
				return fmt.Errorf("truncated offset directory: %s: %w", name, err)
			}
			for position := 0; position < len(block); position += 8 {
				offset := binary.LittleEndian.Uint64(block[position:])
				if (first && offset != 0) || offset < previous || offset > uint64(s.tables[name].parquet.NumRows()) {
					return fmt.Errorf("invalid offset directory: %s", name)
				}
				first = false
				previous = offset
			}
			remaining -= uint64(len(block)) / 8
		}
		if previous != uint64(s.tables[name].parquet.NumRows()) {
			return fmt.Errorf("invalid offset directory end: %s", name)
		}
	}
	return nil
}

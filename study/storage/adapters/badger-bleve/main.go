package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/blevesearch/bleve/v2"
	_ "github.com/blevesearch/bleve/v2/analysis/analyzer/custom"
	_ "github.com/blevesearch/bleve/v2/analysis/token/lowercase"
	_ "github.com/blevesearch/bleve/v2/analysis/tokenizer/regexp"
	"github.com/blevesearch/bleve/v2/search/query"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"gxstudy/runner"
)

// kvStore is consumed by the common graph layout; engines own iterator lifetimes.
type kvStore interface {
	Get([]byte) ([]byte, error)
	Scan([]byte, func([]byte, []byte) error) error
	Close() error
}

const servingCacheBytes int64 = 256 << 20

type backend struct{}
type segment struct {
	kv               kvStore
	symbols, content bleve.Index
}
type sealFile struct {
	Name   string
	Size   int64
	SHA256 string
}
type sealManifest struct {
	Repo   string
	Engine string
	Files  []sealFile
}

func main() {
	b := backend{}
	runner.Main(b, runner.ReferenceCatalog(b))
}
func (backend) Name() string { return engineName }
func (backend) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{
		Ranking: map[string]adapter.RankPolicy{
			"symbols": {Mode: "native"}, "content": {Mode: "native"}, "multi_symbols": {Mode: "native"},
		},
		Operations: map[string]string{
			"id":      "native KV point lookup; JSON record",
			"name":    "native KV name secondary index and point lookups",
			"file":    "native KV file secondary index and point lookups",
			"out":     "native KV endpoint/kind prefix index; JSON edge records",
			"in":      "native KV endpoint/kind prefix index; JSON edge records",
			"symbols": "native Bleve Scorch BM25; Unicode letter/digit tokenizer",
			"content": "native Bleve Scorch BM25; Unicode letter/digit tokenizer",
			"vector":  "emulated exact cosine scan of stored float32 vector blobs",
			"delta":   "emulated full rebuild from stored primary records",
		}, Libraries: []string{engineVersion, "github.com/blevesearch/bleve/v2 v2.6.1"},
		Deployment: "embedded; one graph KV store and two Bleve indexes per sealed repository segment; reference JSON catalog",
		Notes:      []string{engineCacheNotes, "No answer cache or in-memory graph/vector copy. Bleve ranking is native, not the oracle BM25 formula: sqrt term frequency and disjunction coordination can change rankings.", "Default multi operations and traversal use harness fan-out/BFS. No native multi/traversal interface.", "Open checks manifest digest and artifact sizes, not every file hash; immutable files are trusted after sealing. Build hashes and fsyncs all artifacts."},
	}
}
func key(prefix string, fields ...string) []byte {
	var b strings.Builder
	b.WriteString(prefix)
	b.WriteByte('/')
	for _, f := range fields {
		b.WriteString(base64.RawURLEncoding.EncodeToString([]byte(f)))
		b.WriteByte('/')
	}
	return []byte(b.String())
}
func vectorBytes(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(x))
	}
	return b
}
func vectorValues(b []byte) ([]float32, error) {
	if len(b) != dataset.Dimensions*4 {
		return nil, fmt.Errorf("invalid vector byte length %d", len(b))
	}
	v := make([]float32, dataset.Dimensions)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v, nil
}
func recordID(k []byte) (string, error) {
	parts := strings.Split(string(k), "/")
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid primary key")
	}
	b, e := base64.RawURLEncoding.DecodeString(parts[1])
	return string(b), e
}
func writeRecords(ctx context.Context, d dataset.Dataset, put func([]byte, []byte) error) error {
	putJSON := func(k []byte, v any) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		b, e := json.Marshal(v)
		if e != nil {
			return e
		}
		return put(k, b)
	}
	if e := putJSON(key("M"), dataset.Dataset{Repo: d.Repo, Meta: d.Meta}); e != nil {
		return e
	}
	for _, n := range d.Nodes {
		if e := putJSON(key("N", n.ID), n); e != nil {
			return e
		}
		for _, k := range [][]byte{key("A", n.Name, n.ID), key("P", n.FilePath, n.ID), key("K", n.Kind, n.ID)} {
			if e := put(k, []byte(n.ID)); e != nil {
				return e
			}
		}
	}
	for i, e := range d.Edges {
		seq := fmt.Sprintf("%016x", i)
		if err := putJSON(key("O", e.From, e.Kind, seq), e); err != nil {
			return err
		}
		if err := putJSON(key("I", e.To, e.Kind, seq), e); err != nil {
			return err
		}
	}
	for _, f := range d.Files {
		if e := putJSON(key("F", f.Path), f); e != nil {
			return e
		}
	}
	for _, s := range d.Symbols {
		if e := putJSON(key("S", s.NodeID), s); e != nil {
			return e
		}
	}
	for _, c := range d.Content {
		if e := putJSON(key("C", c.ChunkID), c); e != nil {
			return e
		}
	}
	for _, v := range d.Vectors {
		if e := ctx.Err(); e != nil {
			return e
		}
		if len(v.Values) != dataset.Dimensions {
			return fmt.Errorf("invalid vector dimensions for %q", v.NodeID)
		}
		if e := put(key("V", v.NodeID), vectorBytes(v.Values)); e != nil {
			return e
		}
	}
	return nil
}
func buildText(ctx context.Context, path string, rows func(func(string, string) error) error) (err error) {
	m := bleve.NewIndexMapping()
	m.ScoringModel = "bm25"
	if err = m.AddCustomTokenizer("gxwords", map[string]interface{}{"type": "regexp", "regexp": `[\p{L}\p{N}]+`}); err != nil {
		return err
	}
	if err = m.AddCustomAnalyzer("gxtext", map[string]interface{}{"type": "custom", "tokenizer": "gxwords", "token_filters": []string{"to_lower"}}); err != nil {
		return err
	}
	m.DefaultAnalyzer = "gxtext"
	m.DefaultField = "text"
	dm := bleve.NewDocumentStaticMapping()
	fm := bleve.NewTextFieldMapping()
	fm.Analyzer = "gxtext"
	fm.Store = false
	fm.IncludeInAll = false
	fm.IncludeTermVectors = false
	dm.AddFieldMappingsAt("text", fm)
	m.DefaultMapping = dm
	ix, err := bleve.NewUsing(path, m, "scorch", bleve.Config.DefaultKVStore, map[string]interface{}{"unsafe_batch": false})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, ix.Close()) }()
	batch := ix.NewBatch()
	err = rows(func(id, text string) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		if e := batch.Index(id, map[string]string{"text": text}); e != nil {
			return e
		}
		if batch.Size() >= 1000 {
			if e := ix.Batch(batch); e != nil {
				return e
			}
			batch = ix.NewBatch()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if batch.Size() > 0 {
		return ix.Batch(batch)
	}
	return nil
}
func (backend) Build(ctx context.Context, d dataset.Dataset, path string) (adapter.SegmentRef, adapter.BuildStats, error) {
	var ref adapter.SegmentRef
	var stats adapter.BuildStats
	path, e := filepath.Abs(path)
	if e != nil {
		return ref, stats, e
	}
	if e = os.Mkdir(path, 0755); e != nil {
		return ref, stats, e
	}
	if e = buildKV(ctx, filepath.Join(path, "graph"), d); e != nil {
		return ref, stats, e
	}
	e = buildText(ctx, filepath.Join(path, "symbols.bleve"), func(add func(string, string) error) error {
		for _, s := range d.Symbols {
			if e := add(s.NodeID, s.Tokens); e != nil {
				return e
			}
		}
		return nil
	})
	if e != nil {
		return ref, stats, e
	}
	e = buildText(ctx, filepath.Join(path, "content.bleve"), func(add func(string, string) error) error {
		for _, c := range d.Content {
			if e := add(c.ChunkID, c.Body); e != nil {
				return e
			}
		}
		return nil
	})
	if e != nil {
		return ref, stats, e
	}
	manifest := sealManifest{Repo: d.Repo, Engine: engineVersion}
	var dirs []string
	e = filepath.WalkDir(path, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if de.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		info, err := de.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unexpected segment entry %s", p)
		}
		hash, err := dataset.Hash(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(path, p)
		if err != nil {
			return err
		}
		manifest.Files = append(manifest.Files, sealFile{Name: rel, Size: info.Size(), SHA256: hash})
		stats.Bytes += info.Size()
		f, err := os.OpenFile(p, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		return errors.Join(f.Sync(), f.Close())
	})
	if e != nil {
		return ref, stats, e
	}
	b, e := json.Marshal(manifest)
	if e != nil {
		return ref, stats, e
	}
	if e = os.WriteFile(filepath.Join(path, "seal.json"), b, 0644); e != nil {
		return ref, stats, e
	}
	f, e := os.OpenFile(filepath.Join(path, "seal.json"), os.O_RDWR, 0)
	if e != nil {
		return ref, stats, e
	}
	if e = errors.Join(f.Sync(), f.Close()); e != nil {
		return ref, stats, e
	}
	// Sync children before parents; publication of this immutable folder is catalog-owned.
	slices.Reverse(dirs)
	dirs = append(dirs, filepath.Dir(path))
	for _, dir := range dirs {
		f, e := os.Open(dir)
		if e != nil {
			return ref, stats, e
		}
		if e = errors.Join(f.Sync(), f.Close()); e != nil {
			return ref, stats, e
		}
	}
	digest := sha256.Sum256(b)
	id := hex.EncodeToString(digest[:])
	ref = adapter.SegmentRef{Repo: d.Repo, Version: id, Path: path, Digest: id}
	stats.Mode = "full"
	stats.Bytes += int64(len(b))
	stats.Notes = []string{"graph KV plus native Bleve indexes; all closed files hashed and fsynced before seal"}
	return ref, stats, nil
}
func (backend) Open(ctx context.Context, path string, ref adapter.SegmentRef) (adapter.Segment, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if path != ref.Path || ref.Version != ref.Digest {
		return nil, fmt.Errorf("segment ref mismatch")
	}
	b, e := os.ReadFile(filepath.Join(path, "seal.json"))
	if e != nil {
		return nil, e
	}
	digest := sha256.Sum256(b)
	if hex.EncodeToString(digest[:]) != ref.Digest {
		return nil, fmt.Errorf("seal digest mismatch")
	}
	var manifest sealManifest
	if e = json.Unmarshal(b, &manifest); e != nil {
		return nil, e
	}
	if manifest.Repo != ref.Repo || manifest.Engine != engineVersion {
		return nil, fmt.Errorf("seal identity mismatch")
	}
	for _, f := range manifest.Files {
		if !filepath.IsLocal(f.Name) {
			return nil, fmt.Errorf("invalid seal path")
		}
		st, e := os.Stat(filepath.Join(path, f.Name))
		if e != nil {
			return nil, e
		}
		if st.Size() != f.Size {
			return nil, fmt.Errorf("artifact size mismatch: %s", f.Name)
		}
	}
	kv, e := openKV(filepath.Join(path, "graph"))
	if e != nil {
		return nil, e
	}
	s := &segment{kv: kv}
	s.symbols, e = bleve.OpenUsing(filepath.Join(path, "symbols.bleve"), map[string]interface{}{"read_only": true})
	if e != nil {
		return nil, errors.Join(e, kv.Close())
	}
	s.content, e = bleve.OpenUsing(filepath.Join(path, "content.bleve"), map[string]interface{}{"read_only": true})
	if e != nil {
		return nil, errors.Join(e, s.symbols.Close(), kv.Close())
	}
	return s, nil
}
func (s *segment) Close() error {
	return errors.Join(s.symbols.Close(), s.content.Close(), s.kv.Close())
}
func (s *segment) NodeByID(ctx context.Context, id string) (dataset.Node, error) {
	var n dataset.Node
	if e := ctx.Err(); e != nil {
		return n, e
	}
	b, e := s.kv.Get(key("N", id))
	if e != nil {
		return n, e
	}
	e = json.Unmarshal(b, &n)
	return n, e
}
func (s *segment) nodes(ctx context.Context, prefix []byte) ([]dataset.Node, error) {
	var a []dataset.Node
	e := s.kv.Scan(prefix, func(_, v []byte) error {
		n, e := s.NodeByID(ctx, string(v))
		if e != nil {
			return e
		}
		a = append(a, n)
		return nil
	})
	return a, e
}
func (s *segment) NodesByName(ctx context.Context, name string, k int) ([]dataset.Node, error) {
	if k == 0 {
		return nil, nil
	}
	a, e := s.nodes(ctx, key("A", name))
	if e != nil {
		return nil, e
	}
	slices.SortFunc(a, func(a, b dataset.Node) int { return strings.Compare(a.ID, b.ID) })
	if k >= 0 && len(a) > k {
		a = a[:k]
	}
	return a, nil
}
func (s *segment) FileNodes(ctx context.Context, file string) ([]dataset.Node, error) {
	return s.nodes(ctx, key("P", file))
}
func (s *segment) edges(ctx context.Context, direction, id, kind string) ([]dataset.Edge, error) {
	p := key(direction, id)
	if kind != "" && kind != "any" {
		p = key(direction, id, kind)
	}
	var a []dataset.Edge
	e := s.kv.Scan(p, func(_, v []byte) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		var row dataset.Edge
		if e := json.Unmarshal(v, &row); e != nil {
			return e
		}
		a = append(a, row)
		return nil
	})
	return a, e
}
func (s *segment) OutEdges(ctx context.Context, id, kind string) ([]dataset.Edge, error) {
	return s.edges(ctx, "O", id, kind)
}
func (s *segment) InEdges(ctx context.Context, id, kind string) ([]dataset.Edge, error) {
	return s.edges(ctx, "I", id, kind)
}
func searchText(ctx context.Context, ix bleve.Index, text string, k int) ([]adapter.Hit, error) {
	if k == 0 {
		return nil, nil
	}
	terms := ix.Mapping().AnalyzerNamed("gxtext").Analyze([]byte(text))
	seen := map[string]bool{}
	var queries []query.Query
	for _, t := range terms {
		term := string(t.Term)
		if seen[term] {
			continue
		}
		seen[term] = true
		q := bleve.NewTermQuery(term)
		q.SetField("text")
		queries = append(queries, q)
	}
	if len(queries) == 0 {
		return nil, nil
	}
	if k < 0 {
		n, e := ix.DocCount()
		if e != nil {
			return nil, e
		}
		k = int(n)
	}
	req := bleve.NewSearchRequestOptions(bleve.NewDisjunctionQuery(queries...), k, 0, false)
	req.SortBy([]string{"-_score", "_id"})
	res, e := ix.SearchInContext(ctx, req)
	if e != nil {
		return nil, e
	}
	if res.Status.Failed > 0 {
		return nil, fmt.Errorf("Bleve search partial failure: %v", res.Status.Errors)
	}
	hits := make([]adapter.Hit, 0, len(res.Hits))
	for _, h := range res.Hits {
		hits = append(hits, adapter.Hit{ID: h.ID, Score: h.Score})
	}
	return hits, nil
}
func (s *segment) SearchSymbols(ctx context.Context, q string, k int) ([]adapter.Hit, error) {
	return searchText(ctx, s.symbols, q, k)
}
func (s *segment) SearchContent(ctx context.Context, q string, k int) ([]adapter.Hit, error) {
	return searchText(ctx, s.content, q, k)
}
func (s *segment) VectorTopK(ctx context.Context, q []float32, k int) ([]adapter.Hit, error) {
	if k == 0 {
		return nil, nil
	}
	if len(q) != dataset.Dimensions {
		return nil, fmt.Errorf("invalid query dimensions")
	}
	var qnorm float64
	for _, x := range q {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return nil, fmt.Errorf("nonfinite query vector")
		}
		qnorm += float64(x) * float64(x)
	}
	var hits []adapter.Hit
	e := s.kv.Scan(key("V"), func(pk, b []byte) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		v, e := vectorValues(b)
		if e != nil {
			return e
		}
		id, e := recordID(pk)
		if e != nil {
			return e
		}
		var dot, norm float64
		for i, x := range v {
			dot += float64(x) * float64(q[i])
			norm += float64(x) * float64(x)
		}
		score := 0.0
		if norm > 0 && qnorm > 0 {
			score = dot / math.Sqrt(norm*qnorm)
		}
		hits = append(hits, adapter.Hit{ID: id, Score: score})
		// Keep only k scores, never a persistent vector copy or result cache.
		if k > 0 && len(hits) >= 2*k {
			hits = adapter.Top(hits, k)
		}
		return nil
	})
	if e != nil {
		return nil, e
	}
	return adapter.Top(hits, k), nil
}
func readRows[T any](kv kvStore, prefix string) ([]T, error) {
	var rows []T
	e := kv.Scan(key(prefix), func(_, b []byte) error {
		var row T
		if e := json.Unmarshal(b, &row); e != nil {
			return e
		}
		rows = append(rows, row)
		return nil
	})
	return rows, e
}
func (s *segment) records(ctx context.Context) (dataset.Dataset, error) {
	var d dataset.Dataset
	b, e := s.kv.Get(key("M"))
	if e != nil {
		return d, e
	}
	if e = json.Unmarshal(b, &d); e != nil {
		return d, e
	}
	if d.Nodes, e = readRows[dataset.Node](s.kv, "N"); e != nil {
		return d, e
	}
	if d.Edges, e = readRows[dataset.Edge](s.kv, "O"); e != nil {
		return d, e
	}
	if d.Files, e = readRows[dataset.File](s.kv, "F"); e != nil {
		return d, e
	}
	if d.Symbols, e = readRows[dataset.Symbol](s.kv, "S"); e != nil {
		return d, e
	}
	if d.Content, e = readRows[dataset.Content](s.kv, "C"); e != nil {
		return d, e
	}
	e = s.kv.Scan(key("V"), func(k, b []byte) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		id, e := recordID(k)
		if e != nil {
			return e
		}
		v, e := vectorValues(b)
		if e != nil {
			return e
		}
		d.Vectors = append(d.Vectors, dataset.Vector{NodeID: id, Values: v})
		return nil
	})
	return d, e
}
func (b backend) BuildDelta(ctx context.Context, old adapter.SegmentRef, c adapter.FileChange, path string) (adapter.SegmentRef, adapter.BuildStats, error) {
	opened, e := b.Open(ctx, old.Path, old)
	if e != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, e
	}
	s := opened.(*segment)
	d, e := s.records(ctx)
	e = errors.Join(e, s.Close())
	if e != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, e
	}
	ref, stats, e := b.Build(ctx, adapter.ApplyChange(d, c), path)
	stats.Notes = append(stats.Notes, "delta is a full rebuild; old primary records decoded from KV outside the serving path")
	return ref, stats, e
}

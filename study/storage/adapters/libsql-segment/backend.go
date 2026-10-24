package main

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"gxstudy/adapter"
	"gxstudy/dataset"
	"gxstudy/runner"
)

type Backend struct{}
type Segment struct {
	db      *sql.DB
	closeDB func() error
}
type seal struct{ Repo, SHA256 string }

func main() { b := Backend{}; runner.Main(b, runner.ReferenceCatalog(b)) }
func tokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}
func (Backend) Capabilities() adapter.Capabilities {
	c := adapter.Capabilities{Deployment: "embedded sealed SQLite file per repository; reference JSON catalog", Libraries: []string{engineVersion}, Ranking: map[string]adapter.RankPolicy{"symbols": {Mode: "native"}, "content": {Mode: "native"}}, Operations: map[string]string{
		"id": "native primary-key B-tree", "name": "native (name,id) B-tree", "file": "native file B-tree", "in": "native (destination,kind) B-tree", "out": "native (source,kind) B-tree", "symbols": "native FTS5 bm25()", "content": "native FTS5 bm25()", "vector": vectorMode, "delta": "emulated full rebuild", "traverse": "harness BFS over indexed edges", "multi": "harness fan-out", "catalog": "reference atomic JSON + flock"}, Notes: []string{"No answer cache or in-memory corpus. Native FTS5 unicode61 tokenizer and bm25 ranking; quoted OR terms. Native rank may differ from the harness positive-log1p BM25 oracle.", "page_size=4096, journal_mode=DELETE, synchronous=FULL; FTS optimize and ANALYZE before seal. Serving mmap_size=67108864, cache_size=-2048; at most eight reader connections per segment.", readOnlyNote}}
	if !nativeVector {
		c.Ranking["vector"] = adapter.RankPolicy{Exact: true}
		c.Ranking["multi_vector"] = adapter.RankPolicy{Exact: true}
	}
	return c
}
func (Backend) Build(ctx context.Context, d dataset.Dataset, dir string) (adapter.SegmentRef, adapter.BuildStats, error) {
	var ref adapter.SegmentRef
	var stats adapter.BuildStats
	if err := ctx.Err(); err != nil {
		return ref, stats, err
	}
	if err := os.Mkdir(dir, 0755); err != nil {
		return ref, stats, err
	}
	path := filepath.Join(dir, "segment.sqlite")
	db, closeDB, err := openDB(ctx, path, false)
	if err != nil {
		return ref, stats, err
	}
	defer closeDB()
	for _, q := range []string{"PRAGMA page_size=4096", "PRAGMA synchronous=FULL", "PRAGMA cache_size=-8192"} {
		if _, err = db.ExecContext(ctx, q); err != nil {
			return ref, stats, err
		}
	}
	var journal string
	if err = db.QueryRowContext(ctx, "PRAGMA journal_mode=DELETE").Scan(&journal); err != nil {
		return ref, stats, err
	}
	if journal != "delete" {
		return ref, stats, fmt.Errorf("unexpected journal mode: %s", journal)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ref, stats, err
	}
	defer tx.Rollback()
	for _, q := range []string{
		"CREATE TABLE metadata (key TEXT PRIMARY KEY,value TEXT NOT NULL)",
		"CREATE TABLE nodes (id TEXT PRIMARY KEY,name TEXT NOT NULL,file TEXT NOT NULL,raw BLOB NOT NULL)",
		"CREATE TABLE edges (src TEXT NOT NULL,dst TEXT NOT NULL,kind TEXT NOT NULL,raw BLOB NOT NULL)",
		"CREATE TABLE files (raw BLOB NOT NULL)",
		"CREATE TABLE symbols (id TEXT UNIQUE NOT NULL,body TEXT NOT NULL)",
		"CREATE TABLE content (id TEXT UNIQUE NOT NULL,node_id TEXT NOT NULL,file TEXT NOT NULL,body TEXT NOT NULL)",
		"CREATE TABLE vectors (node_id TEXT UNIQUE NOT NULL,embedding F32_BLOB(384) NOT NULL)",
		"CREATE VIRTUAL TABLE symbols_fts USING fts5(body,content='symbols',content_rowid='rowid',tokenize='unicode61 remove_diacritics 0')",
		"CREATE VIRTUAL TABLE content_fts USING fts5(body,content='content',content_rowid='rowid',tokenize='unicode61 remove_diacritics 0')",
	} {
		if _, err = tx.ExecContext(ctx, q); err != nil {
			return ref, stats, fmt.Errorf("schema %s: %w", q, err)
		}
	}
	meta, err := json.Marshal(d.Meta)
	if err != nil {
		return ref, stats, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO metadata VALUES ('repo',?),('manifest',?)", d.Repo, string(meta)); err != nil {
		return ref, stats, err
	}
	if err = insertRecords(ctx, tx, d); err != nil {
		return ref, stats, err
	}
	for _, q := range []string{
		"CREATE INDEX nodes_name ON nodes(name,id)", "CREATE INDEX nodes_file ON nodes(file)",
		"CREATE INDEX edges_out ON edges(src,kind)", "CREATE INDEX edges_in ON edges(dst,kind)",
		"INSERT INTO symbols_fts(symbols_fts) VALUES ('rebuild')", "INSERT INTO content_fts(content_fts) VALUES ('rebuild')",
		"INSERT INTO symbols_fts(symbols_fts) VALUES ('optimize')", "INSERT INTO content_fts(content_fts) VALUES ('optimize')",
	} {
		if _, err = tx.ExecContext(ctx, q); err != nil {
			return ref, stats, err
		}
	}
	if nativeVector {
		if _, err = tx.ExecContext(ctx, vectorIndexSQL); err != nil {
			return ref, stats, err
		}
	}
	if err = tx.Commit(); err != nil {
		return ref, stats, err
	}
	if _, err = db.ExecContext(ctx, "ANALYZE"); err != nil {
		return ref, stats, err
	}
	if err = closeDB(); err != nil {
		return ref, stats, err
	}
	if err = os.Chmod(path, 0444); err != nil {
		return ref, stats, err
	}
	hash, err := dataset.Hash(path)
	if err != nil {
		return ref, stats, err
	}
	manifest := filepath.Join(dir, "SEALED.json")
	if err = dataset.WriteJSON(manifest, seal{d.Repo, hash}); err != nil {
		return ref, stats, err
	}
	if err = os.Chmod(manifest, 0444); err != nil {
		return ref, stats, err
	}
	for _, p := range []string{path, manifest, dir} {
		f, e := os.Open(p)
		if e != nil {
			return ref, stats, e
		}
		e = f.Sync()
		e = errors.Join(e, f.Close())
		if e != nil {
			return ref, stats, e
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ref, stats, err
	}
	n, err := diskBytes(dir)
	if err != nil {
		return ref, stats, err
	}
	return adapter.SegmentRef{Repo: d.Repo, Path: abs, Version: hash, Digest: hash}, adapter.BuildStats{Mode: "full", Bytes: n, Notes: []string{"sealed standalone file; full rebuild including FTS/vector indexes"}}, nil
}
func insertRecords(ctx context.Context, tx *sql.Tx, d dataset.Dataset) error {
	queries := []string{"INSERT INTO nodes VALUES (?,?,?,?)", "INSERT INTO edges VALUES (?,?,?,?)", "INSERT INTO files VALUES (?)", "INSERT INTO symbols(id,body) VALUES (?,?)", "INSERT INTO content(id,node_id,file,body) VALUES (?,?,?,?)", "INSERT INTO vectors VALUES (?,?)"}
	stmts := make([]*sql.Stmt, len(queries))
	for i, q := range queries {
		st, err := tx.PrepareContext(ctx, q)
		if err != nil {
			return err
		}
		stmts[i] = st
		defer st.Close()
	}
	for _, n := range d.Nodes {
		raw, err := json.Marshal(n)
		if err != nil {
			return err
		}
		if _, err = stmts[0].ExecContext(ctx, n.ID, n.Name, n.FilePath, raw); err != nil {
			return err
		}
	}
	for _, e := range d.Edges {
		raw, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if _, err = stmts[1].ExecContext(ctx, e.From, e.To, e.Kind, raw); err != nil {
			return err
		}
	}
	for _, f := range d.Files {
		raw, err := json.Marshal(f)
		if err != nil {
			return err
		}
		if _, err = stmts[2].ExecContext(ctx, raw); err != nil {
			return err
		}
	}
	for _, s := range d.Symbols {
		if _, err := stmts[3].ExecContext(ctx, s.NodeID, s.Tokens); err != nil {
			return err
		}
	}
	for _, c := range d.Content {
		if _, err := stmts[4].ExecContext(ctx, c.ChunkID, c.NodeID, c.FilePath, c.Body); err != nil {
			return err
		}
	}
	for _, v := range d.Vectors {
		if err := validateVector(v.Values); err != nil {
			return err
		}
		if _, err := stmts[5].ExecContext(ctx, v.NodeID, vectorBytes(v.Values)); err != nil {
			return err
		}
	}
	return nil
}
func diskBytes(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			return 0, err
		}
		n += info.Size()
	}
	return n, nil
}
func (Backend) Open(ctx context.Context, dir string, ref adapter.SegmentRef) (adapter.Segment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if dir != ref.Path {
		return nil, fmt.Errorf("segment path mismatch")
	}
	var seal seal
	if err := dataset.ReadJSON(filepath.Join(dir, "SEALED.json"), &seal); err != nil {
		return nil, err
	}
	if seal.Repo != ref.Repo || seal.SHA256 != ref.Digest || ref.Version != ref.Digest {
		return nil, fmt.Errorf("sealed reference mismatch")
	}
	path := filepath.Join(dir, "segment.sqlite")
	hash, err := dataset.Hash(path)
	if err != nil {
		return nil, err
	}
	if hash != ref.Digest {
		return nil, fmt.Errorf("segment checksum mismatch")
	}
	db, closeDB, err := openDB(ctx, path, true)
	if err != nil {
		return nil, err
	}
	s := &Segment{db: db, closeDB: closeDB}
	var repo string
	if err = db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='repo'").Scan(&repo); err != nil || repo != ref.Repo {
		closeDB()
		return nil, errors.Join(err, fmt.Errorf("database repository mismatch"))
	}
	return s, nil
}
func (s *Segment) Close() error { return s.closeDB() }
func readJSONRows[T any](ctx context.Context, db *sql.DB, q string, args ...any) ([]T, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var v T
		if err = json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Segment) NodeByID(ctx context.Context, id string) (dataset.Node, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, "SELECT raw FROM nodes WHERE id=?", id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return dataset.Node{}, adapter.ErrNotFound
	}
	if err != nil {
		return dataset.Node{}, err
	}
	var n dataset.Node
	err = json.Unmarshal(raw, &n)
	return n, err
}
func (s *Segment) NodesByName(ctx context.Context, name string, k int) ([]dataset.Node, error) {
	return readJSONRows[dataset.Node](ctx, s.db, "SELECT raw FROM nodes WHERE name=? ORDER BY id LIMIT ?", name, k)
}
func (s *Segment) FileNodes(ctx context.Context, path string) ([]dataset.Node, error) {
	return readJSONRows[dataset.Node](ctx, s.db, "SELECT raw FROM nodes WHERE file=?", path)
}
func (s *Segment) edges(ctx context.Context, column, id, kind string) ([]dataset.Edge, error) {
	q := "SELECT raw FROM edges WHERE " + column + "=?"
	args := []any{id}
	if kind != "" && kind != "any" {
		q += " AND kind=?"
		args = append(args, kind)
	}
	return readJSONRows[dataset.Edge](ctx, s.db, q, args...)
}
func (s *Segment) OutEdges(ctx context.Context, id, kind string) ([]dataset.Edge, error) {
	return s.edges(ctx, "src", id, kind)
}
func (s *Segment) InEdges(ctx context.Context, id, kind string) ([]dataset.Edge, error) {
	return s.edges(ctx, "dst", id, kind)
}
func (s *Segment) SearchSymbols(ctx context.Context, q string, k int) ([]adapter.Hit, error) {
	return s.search(ctx, "symbols", q, k)
}
func (s *Segment) SearchContent(ctx context.Context, q string, k int) ([]adapter.Hit, error) {
	return s.search(ctx, "content", q, k)
}
func (s *Segment) search(ctx context.Context, table, q string, k int) ([]adapter.Hit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if k == 0 {
		return nil, nil
	}
	terms := tokens(q)
	if len(terms) == 0 {
		return nil, nil
	}
	slices.Sort(terms)
	terms = slices.Compact(terms)
	for i, t := range terms {
		terms[i] = "\"" + t + "\""
	}
	fts := table + "_fts"
	rows, err := s.db.QueryContext(ctx, "SELECT d.id,-bm25("+fts+") AS score FROM "+fts+" JOIN "+table+" d ON d.rowid="+fts+".rowid WHERE "+fts+" MATCH ? ORDER BY score DESC,d.id LIMIT ?", strings.Join(terms, " OR "), k)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []adapter.Hit
	for rows.Next() {
		var h adapter.Hit
		if err = rows.Scan(&h.ID, &h.Score); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}
func validateVector(q []float32) error {
	if len(q) != dataset.Dimensions {
		return fmt.Errorf("vector dimensions: %d", len(q))
	}
	var norm float64
	for _, v := range q {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return fmt.Errorf("nonfinite vector")
		}
		norm += float64(v) * float64(v)
	}
	if norm == 0 {
		return fmt.Errorf("zero vector")
	}
	return nil
}
func vectorBytes(q []float32) []byte {
	b := make([]byte, len(q)*4)
	for i, v := range q {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(v))
	}
	return b
}
func vectorValues(b []byte) ([]float32, error) {
	if len(b) != dataset.Dimensions*4 {
		return nil, fmt.Errorf("invalid stored vector length: %d", len(b))
	}
	q := make([]float32, dataset.Dimensions)
	for i := range q {
		q[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return q, nil
}
func cosine(a, b []float32) float64 {
	var dot, an, bn float64
	for i, x := range a {
		y := b[i]
		dot += float64(x) * float64(y)
		an += float64(x) * float64(x)
		bn += float64(y) * float64(y)
	}
	return dot / math.Sqrt(an*bn)
}
func (s *Segment) VectorTopK(ctx context.Context, q []float32, k int) ([]adapter.Hit, error) {
	if err := validateVector(q); err != nil {
		return nil, err
	}
	if k == 0 {
		return nil, nil
	}
	query := "SELECT node_id,embedding FROM vectors"
	var args []any
	if nativeVector {
		if k < 0 {
			return nil, adapter.ErrUnsupported
		}
		query = "SELECT v.node_id,v.embedding FROM vector_top_k('vectors_ann',?,?) t JOIN vectors v ON v.rowid=t.id"
		args = []any{vectorBytes(q), k}
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []adapter.Hit
	for rows.Next() {
		var id string
		var raw []byte
		if err = rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		v, err := vectorValues(raw)
		if err != nil {
			return nil, err
		}
		hits = append(hits, adapter.Hit{ID: id, Score: cosine(q, v)})
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return adapter.Top(hits, k), nil
}
func (b Backend) BuildDelta(ctx context.Context, ref adapter.SegmentRef, c adapter.FileChange, dir string) (adapter.SegmentRef, adapter.BuildStats, error) {
	seg, err := b.Open(ctx, ref.Path, ref)
	if err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, err
	}
	s := seg.(*Segment)
	defer s.Close()
	d := dataset.Dataset{Repo: ref.Repo}
	var meta string
	if err = s.db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='manifest'").Scan(&meta); err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, err
	}
	if err = json.Unmarshal([]byte(meta), &d.Meta); err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, err
	}
	if d.Nodes, err = readJSONRows[dataset.Node](ctx, s.db, "SELECT raw FROM nodes"); err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, err
	}
	if d.Edges, err = readJSONRows[dataset.Edge](ctx, s.db, "SELECT raw FROM edges ORDER BY rowid"); err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, err
	}
	if d.Files, err = readJSONRows[dataset.File](ctx, s.db, "SELECT raw FROM files"); err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, err
	}
	symbols, err := s.db.QueryContext(ctx, "SELECT id,body FROM symbols")
	if err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, err
	}
	for symbols.Next() {
		var v dataset.Symbol
		if err = symbols.Scan(&v.NodeID, &v.Tokens); err != nil {
			symbols.Close()
			return adapter.SegmentRef{}, adapter.BuildStats{}, err
		}
		d.Symbols = append(d.Symbols, v)
	}
	if err = errors.Join(symbols.Err(), symbols.Close()); err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, err
	}
	content, err := s.db.QueryContext(ctx, "SELECT id,node_id,file,body FROM content")
	if err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, err
	}
	for content.Next() {
		var v dataset.Content
		if err = content.Scan(&v.ChunkID, &v.NodeID, &v.FilePath, &v.Body); err != nil {
			content.Close()
			return adapter.SegmentRef{}, adapter.BuildStats{}, err
		}
		d.Content = append(d.Content, v)
	}
	if err = errors.Join(content.Err(), content.Close()); err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT node_id,embedding FROM vectors")
	if err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, err
	}
	for rows.Next() {
		var v dataset.Vector
		var raw []byte
		if err = rows.Scan(&v.NodeID, &raw); err != nil {
			rows.Close()
			return adapter.SegmentRef{}, adapter.BuildStats{}, err
		}
		v.Values, err = vectorValues(raw)
		if err != nil {
			rows.Close()
			return adapter.SegmentRef{}, adapter.BuildStats{}, err
		}
		d.Vectors = append(d.Vectors, v)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, err
	}
	return b.Build(ctx, adapter.ApplyChange(d, c), dir)
}
func fileURI(path string) string { return (&url.URL{Scheme: "file", Path: path}).String() }

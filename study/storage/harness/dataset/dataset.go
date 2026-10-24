// Package dataset defines the candidate-neutral study records and sealed files.
package dataset

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
)

const GeneratorVersion = "gxstudy-v2"
const Dimensions = 384

var RecordFiles = []string{"nodes.jsonl", "edges.jsonl", "files.jsonl", "symbols.jsonl", "content.jsonl", "vectors.bin"}

type Node struct {
	Payload   map[string]json.RawMessage `json:"payload"`
	ID        string                     `json:"id"`
	Kind      string                     `json:"kind"`
	Name      string                     `json:"name"`
	QualName  string                     `json:"qual_name"`
	FilePath  string                     `json:"file_path"`
	StartLine int                        `json:"start_line"`
	EndLine   int                        `json:"end_line"`
	Language  string                     `json:"language"`
}
type Edge struct {
	Confidence float64                    `json:"confidence"`
	Payload    map[string]json.RawMessage `json:"payload"`
	From       string                     `json:"from"`
	To         string                     `json:"to"`
	Kind       string                     `json:"kind"`
	FilePath   string                     `json:"file_path"`
	Line       int                        `json:"line"`
	CrossRepo  bool                       `json:"cross_repo"`
}
type File struct {
	Path        string `json:"path"`
	ContentHash string `json:"content_hash"`
	Size        int64  `json:"size"`
}
type Symbol struct {
	NodeID string `json:"node_id"`
	Tokens string `json:"tokens"`
}
type Content struct {
	ChunkID  string `json:"chunk_id"`
	NodeID   string `json:"node_id"`
	FilePath string `json:"file_path"`
	Body     string `json:"body"`
}
type Vector struct {
	NodeID string    `json:"node_id"`
	Values []float32 `json:"values"`
}
type Counts struct {
	Nodes        int `json:"nodes"`
	Edges        int `json:"edges"`
	Files        int `json:"files"`
	Symbols      int `json:"symbols"`
	Content      int `json:"content"`
	Vectors      int `json:"vectors"`
	CrossEdges   int `json:"cross_edges"`
	SkippedSpans int `json:"skipped_spans"`
}
type Manifest struct {
	Namespace       string            `json:"namespace,omitempty"`
	CrossRepoPolicy string            `json:"cross_repo_policy,omitempty"`
	Origin          string            `json:"origin,omitempty"`
	ParentRepo      string            `json:"parent_repo,omitempty"`
	DerivedFraction float64           `json:"derived_fraction,omitempty"`
	Version         string            `json:"generator_version"`
	Repo            string            `json:"repo,omitempty"`
	Repos           []string          `json:"repos,omitempty"`
	Seed            int64             `json:"seed"`
	SourceRevision  string            `json:"source_revision"`
	SourceSHA256    string            `json:"source_sha256"`
	Counts          Counts            `json:"counts"`
	SHA256          map[string]string `json:"sha256"`
}
type Dataset struct {
	Repo    string
	Nodes   []Node
	Edges   []Edge
	Files   []File
	Symbols []Symbol
	Content []Content
	Vectors []Vector
	Meta    Manifest
}

func (d Dataset) Counts() Counts {
	c := Counts{Nodes: len(d.Nodes), Edges: len(d.Edges), Files: len(d.Files), Symbols: len(d.Symbols), Content: len(d.Content), Vectors: len(d.Vectors), SkippedSpans: d.Meta.Counts.SkippedSpans}
	for _, e := range d.Edges {
		if e.CrossRepo {
			c.CrossEdges++
		}
	}
	return c
}
func Sum(a, b Counts) Counts {
	return Counts{a.Nodes + b.Nodes, a.Edges + b.Edges, a.Files + b.Files, a.Symbols + b.Symbols, a.Content + b.Content, a.Vectors + b.Vectors, a.CrossEdges + b.CrossEdges, a.SkippedSpans + b.SkippedSpans}
}
func Hash(path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return "", e
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func WriteJSON(path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	return os.WriteFile(path, append(b, '\n'), 0644)
}
func ReadJSON(path string, v any) error {
	b, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, v)
}
func writeLines[T any](path string, rows []T) error {
	f, e := os.Create(path)
	if e != nil {
		return e
	}
	w := bufio.NewWriterSize(f, 1<<20)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, r := range rows {
		if e = enc.Encode(r); e != nil {
			f.Close()
			return e
		}
	}
	if e = w.Flush(); e != nil {
		f.Close()
		return e
	}
	return f.Close()
}
func readLines[T any](path string, rows *[]T) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	dec := json.NewDecoder(bufio.NewReaderSize(f, 1<<20))
	for {
		var r T
		e = dec.Decode(&r)
		if e == io.EOF {
			return nil
		}
		if e != nil {
			return fmt.Errorf("decode %s: %w", path, e)
		}
		*rows = append(*rows, r)
	}
}
func writeVectors(path string, rows []Vector) error {
	f, e := os.Create(path)
	if e != nil {
		return e
	}
	w := bufio.NewWriterSize(f, 1<<20)
	for _, v := range rows {
		if len(v.Values) != Dimensions || len(v.NodeID) > 1<<20 {
			f.Close()
			return fmt.Errorf("invalid vector %s", v.NodeID)
		}
		if e = binary.Write(w, binary.LittleEndian, uint32(len(v.NodeID))); e != nil {
			f.Close()
			return e
		}
		if _, e = w.WriteString(v.NodeID); e != nil {
			f.Close()
			return e
		}
		if e = binary.Write(w, binary.LittleEndian, v.Values); e != nil {
			f.Close()
			return e
		}
	}
	if e = w.Flush(); e != nil {
		f.Close()
		return e
	}
	return f.Close()
}
func readVectors(path string) ([]Vector, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	var rows []Vector
	for {
		var n uint32
		e = binary.Read(r, binary.LittleEndian, &n)
		if e == io.EOF {
			return rows, nil
		}
		if e != nil {
			return nil, e
		}
		if n == 0 || n > 1<<20 {
			return nil, fmt.Errorf("invalid vector id length %d", n)
		}
		id := make([]byte, n)
		if _, e = io.ReadFull(r, id); e != nil {
			return nil, e
		}
		v := Vector{NodeID: string(id), Values: make([]float32, Dimensions)}
		if e = binary.Read(r, binary.LittleEndian, v.Values); e != nil {
			return nil, e
		}
		for _, x := range v.Values {
			if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
				return nil, fmt.Errorf("nonfinite vector")
			}
		}
		rows = append(rows, v)
	}
}
func Write(dir string, d Dataset) (Manifest, error) {
	if e := os.MkdirAll(dir, 0755); e != nil {
		return Manifest{}, e
	}
	m := d.Meta
	m.Version = GeneratorVersion
	m.Repo = d.Repo
	m.Counts = d.Counts()
	m.SHA256 = map[string]string{}
	paths := []error{writeLines(filepath.Join(dir, RecordFiles[0]), d.Nodes), writeLines(filepath.Join(dir, RecordFiles[1]), d.Edges), writeLines(filepath.Join(dir, RecordFiles[2]), d.Files), writeLines(filepath.Join(dir, RecordFiles[3]), d.Symbols), writeLines(filepath.Join(dir, RecordFiles[4]), d.Content), writeVectors(filepath.Join(dir, RecordFiles[5]), d.Vectors)}
	for _, e := range paths {
		if e != nil {
			return m, e
		}
	}
	for _, name := range RecordFiles {
		h, e := Hash(filepath.Join(dir, name))
		if e != nil {
			return m, e
		}
		m.SHA256[name] = h
	}
	return m, WriteJSON(filepath.Join(dir, "manifest.json"), m)
}
func Load(dir string) (Dataset, error) {
	var d Dataset
	if e := ReadJSON(filepath.Join(dir, "manifest.json"), &d.Meta); e != nil {
		return d, e
	}
	if d.Meta.Version != GeneratorVersion {
		return d, fmt.Errorf("unsupported dataset version %q", d.Meta.Version)
	}
	for _, name := range RecordFiles {
		h, e := Hash(filepath.Join(dir, name))
		if e != nil {
			return d, e
		}
		if h != d.Meta.SHA256[name] {
			return d, fmt.Errorf("checksum mismatch: %s", name)
		}
	}
	d.Repo = d.Meta.Repo
	errs := []error{readLines(filepath.Join(dir, RecordFiles[0]), &d.Nodes), readLines(filepath.Join(dir, RecordFiles[1]), &d.Edges), readLines(filepath.Join(dir, RecordFiles[2]), &d.Files), readLines(filepath.Join(dir, RecordFiles[3]), &d.Symbols), readLines(filepath.Join(dir, RecordFiles[4]), &d.Content)}
	for _, e := range errs {
		if e != nil {
			return d, e
		}
	}
	var e error
	d.Vectors, e = readVectors(filepath.Join(dir, RecordFiles[5]))
	if e != nil {
		return d, e
	}
	if d.Counts() != d.Meta.Counts {
		return d, fmt.Errorf("record counts do not match manifest")
	}
	seen := map[string]bool{}
	for _, n := range d.Nodes {
		if n.ID == "" || seen[n.ID] {
			return d, fmt.Errorf("empty or duplicate node id %q", n.ID)
		}
		seen[n.ID] = true
	}
	return d, nil
}
func TierManifest(dir string, repos []string, meta Manifest) error {
	meta.Repo = ""
	meta.Repos = slices.Clone(repos)
	meta.Counts = Counts{}
	meta.SHA256 = map[string]string{}
	for _, repo := range repos {
		var m Manifest
		if e := ReadJSON(filepath.Join(dir, repo, "manifest.json"), &m); e != nil {
			return e
		}
		meta.Counts = Sum(meta.Counts, m.Counts)
		for _, name := range append(slices.Clone(RecordFiles), "manifest.json") {
			p := filepath.Join(repo, name)
			h, e := Hash(filepath.Join(dir, p))
			if e != nil {
				return e
			}
			meta.SHA256[p] = h
		}
	}
	for _, name := range []string{"queries.json", "expected.json"} {
		if _, e := os.Stat(filepath.Join(dir, name)); e == nil {
			h, e := Hash(filepath.Join(dir, name))
			if e != nil {
				return e
			}
			meta.SHA256[name] = h
		}
	}
	return WriteJSON(filepath.Join(dir, "manifest.json"), meta)
}

// Package oracle implements exact reference answers. It is not a performance candidate.
package oracle

import (
	"context"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

type posting struct {
	doc int
	tf  int
}
type textIndex struct {
	ids      []string
	lengths  []int
	postings map[string][]posting
	avg      float64
}

func Tokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}
func newText(ids, bodies []string) textIndex {
	x := textIndex{ids: ids, postings: map[string][]posting{}}
	total := 0
	for i, b := range bodies {
		t := Tokens(b)
		x.lengths = append(x.lengths, len(t))
		total += len(t)
		freq := map[string]int{}
		for _, v := range t {
			freq[v]++
		}
		for v, n := range freq {
			x.postings[v] = append(x.postings[v], posting{i, n})
		}
	}
	if len(ids) > 0 {
		x.avg = float64(total) / float64(len(ids))
	}
	return x
}
func (x textIndex) search(q string, k int) []adapter.Hit {
	scores := map[int]float64{}
	terms := Tokens(q)
	slices.Sort(terms)
	terms = slices.Compact(terms)
	for _, t := range terms {
		p := x.postings[t]
		idf := math.Log(1 + (float64(len(x.ids)-len(p))+.5)/(float64(len(p))+.5))
		for _, v := range p {
			tf := float64(v.tf)
			den := tf + 1.2*(.25+.75*float64(x.lengths[v.doc])/x.avg)
			scores[v.doc] += idf * tf * 2.2 / den
		}
	}
	a := make([]adapter.Hit, 0, len(scores))
	for d, s := range scores {
		a = append(a, adapter.Hit{ID: x.ids[d], Score: s})
	}
	return adapter.Top(a, k)
}

type Segment struct {
	data             dataset.Dataset
	nodes            map[string]dataset.Node
	names            map[string][]dataset.Node
	files            map[string][]dataset.Node
	out, in          map[string][]dataset.Edge
	symbols, content textIndex
}

func New(d dataset.Dataset) *Segment {
	s := &Segment{data: d, nodes: map[string]dataset.Node{}, names: map[string][]dataset.Node{}, files: map[string][]dataset.Node{}, out: map[string][]dataset.Edge{}, in: map[string][]dataset.Edge{}}
	for _, n := range d.Nodes {
		s.nodes[n.ID] = n
		s.names[n.Name] = append(s.names[n.Name], n)
		s.files[n.FilePath] = append(s.files[n.FilePath], n)
	}
	for _, e := range d.Edges {
		s.out[e.From] = append(s.out[e.From], e)
		s.in[e.To] = append(s.in[e.To], e)
	}
	for _, m := range []map[string][]dataset.Node{s.names, s.files} {
		for k, v := range m {
			slices.SortFunc(v, func(a, b dataset.Node) int { return strings.Compare(a.ID, b.ID) })
			m[k] = v
		}
	}
	ids := make([]string, 0, len(d.Symbols))
	bodies := make([]string, 0, len(d.Symbols))
	for _, v := range d.Symbols {
		ids = append(ids, v.NodeID)
		bodies = append(bodies, v.Tokens)
	}
	s.symbols = newText(ids, bodies)
	ids = nil
	bodies = nil
	for _, v := range d.Content {
		ids = append(ids, v.ChunkID)
		bodies = append(bodies, v.Body)
	}
	s.content = newText(ids, bodies)
	return s
}
func (s *Segment) NodeByID(ctx context.Context, id string) (dataset.Node, error) {
	if e := ctx.Err(); e != nil {
		return dataset.Node{}, e
	}
	v, ok := s.nodes[id]
	if !ok {
		return v, adapter.ErrNotFound
	}
	return v, nil
}
func (s *Segment) NodesByName(ctx context.Context, name string, k int) ([]dataset.Node, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	a := s.names[name]
	if k >= 0 && len(a) > k {
		a = a[:k]
	}
	return slices.Clone(a), nil
}
func (s *Segment) FileNodes(ctx context.Context, path string) ([]dataset.Node, error) {
	return slices.Clone(s.files[path]), ctx.Err()
}
func edgeKind(a []dataset.Edge, kind string) []dataset.Edge {
	var b []dataset.Edge
	for _, e := range a {
		if kind == "" || kind == "any" || kind == e.Kind {
			b = append(b, e)
		}
	}
	return b
}
func (s *Segment) OutEdges(ctx context.Context, id, kind string) ([]dataset.Edge, error) {
	return edgeKind(s.out[id], kind), ctx.Err()
}
func (s *Segment) InEdges(ctx context.Context, id, kind string) ([]dataset.Edge, error) {
	return edgeKind(s.in[id], kind), ctx.Err()
}
func (s *Segment) SearchSymbols(ctx context.Context, q string, k int) ([]adapter.Hit, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	return s.symbols.search(q, k), nil
}
func (s *Segment) SearchContent(ctx context.Context, q string, k int) ([]adapter.Hit, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	return s.content.search(q, k), nil
}
func (s *Segment) VectorTopK(ctx context.Context, q []float32, k int) ([]adapter.Hit, error) {
	if len(q) != dataset.Dimensions {
		return nil, fmt.Errorf("vector dimensions: got %d", len(q))
	}
	norm := 0.
	for _, v := range q {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return nil, fmt.Errorf("nonfinite query vector")
		}
		norm += float64(v) * float64(v)
	}
	if norm == 0 {
		return nil, fmt.Errorf("zero query vector")
	}
	a := make([]adapter.Hit, 0, len(s.data.Vectors))
	for i, v := range s.data.Vectors {
		if i%1024 == 0 {
			if e := ctx.Err(); e != nil {
				return nil, e
			}
		}
		dot, vnorm := 0., 0.
		for j, x := range q {
			dot += float64(x) * float64(v.Values[j])
			vnorm += float64(v.Values[j]) * float64(v.Values[j])
		}
		a = append(a, adapter.Hit{ID: v.NodeID, Score: dot / math.Sqrt(norm*vnorm)})
	}
	return adapter.Top(a, k), nil
}
func (s *Segment) Close() error { return nil }

type Backend struct{}

func (Backend) Name() string { return "oracle" }
func (Backend) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{Ranking: map[string]adapter.RankPolicy{"vector": {Exact: true}, "multi_vector": {Exact: true}, "symbols": {Exact: true}, "content": {Exact: true}, "native_multi_symbols": {Exact: true}, "native_multi_vector": {Exact: true}}, Deployment: "embedded", Libraries: []string{}, Operations: map[string]string{"graph": "native in-memory maps", "search": "native plain BM25", "vector": "native exact cosine", "delta": "full rebuild", "catalog": "reference atomic JSON + flock"}, Notes: []string{"Reference correctness implementation; not a candidate storage engine."}}
}
func (Backend) Build(ctx context.Context, d dataset.Dataset, dir string) (adapter.SegmentRef, adapter.BuildStats, error) {
	if e := ctx.Err(); e != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, e
	}
	if _, e := os.Stat(filepath.Join(dir, "manifest.json")); e == nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, fmt.Errorf("refuse to overwrite existing segment")
	}
	_, e := dataset.Write(dir, d)
	if e != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, e
	}
	digest, e := dataset.Hash(filepath.Join(dir, "manifest.json"))
	if e != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, e
	}
	if e = os.WriteFile(filepath.Join(dir, "SEALED"), []byte(digest+"\n"), 0444); e != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, e
	}
	for _, name := range append(slices.Clone(dataset.RecordFiles), "manifest.json", "SEALED") {
		f, e := os.Open(filepath.Join(dir, name))
		if e != nil {
			return adapter.SegmentRef{}, adapter.BuildStats{}, e
		}
		e = f.Sync()
		f.Close()
		if e != nil {
			return adapter.SegmentRef{}, adapter.BuildStats{}, e
		}
		if e = os.Chmod(filepath.Join(dir, name), 0444); e != nil {
			return adapter.SegmentRef{}, adapter.BuildStats{}, e
		}
	}
	f, e := os.Open(dir)
	if e != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, e
	}
	e = f.Sync()
	f.Close()
	if e != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, e
	}
	ref := adapter.SegmentRef{Repo: d.Repo, Version: digest, Path: dir, Digest: digest}
	n, e := DiskBytes(dir)
	return ref, adapter.BuildStats{Mode: "full", Bytes: n}, e
}
func VerifyRef(ref adapter.SegmentRef) error {
	h, e := dataset.Hash(filepath.Join(ref.Path, "manifest.json"))
	if e != nil {
		return e
	}
	if h != ref.Digest {
		return fmt.Errorf("segment digest mismatch")
	}
	b, e := os.ReadFile(filepath.Join(ref.Path, "SEALED"))
	if e != nil {
		return e
	}
	if strings.TrimSpace(string(b)) != h {
		return fmt.Errorf("segment is not sealed")
	}
	return nil
}
func (Backend) Open(ctx context.Context, dir string, ref adapter.SegmentRef) (adapter.Segment, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if e := VerifyRef(ref); e != nil {
		return nil, e
	}
	d, e := dataset.Load(ref.Path)
	if e != nil {
		return nil, e
	}
	return New(d), nil
}
func (b Backend) BuildDelta(ctx context.Context, base adapter.SegmentRef, c adapter.FileChange, dir string) (adapter.SegmentRef, adapter.BuildStats, error) {
	d, e := dataset.Load(base.Path)
	if e != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, e
	}
	return b.Build(ctx, adapter.ApplyChange(d, c), dir)
}
func DiskBytes(dir string) (int64, error) {
	var n int64
	e := filepath.WalkDir(dir, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if !d.IsDir() {
			s, e := d.Info()
			if e != nil {
				return e
			}
			n += s.Size()
		}
		return nil
	})
	return n, e
}

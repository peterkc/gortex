package oracle

import (
	"context"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"iter"
	"math"
	"slices"
	"strings"
)

// DiskSegment scans sealed input records. Checker-owned views may retain edge
// spans, but never a graph payload, text or vector corpus map.
type DiskSegment struct {
	Data  dataset.Dataset
	index *edgeIndex
}

func (s *DiskSegment) Close() error { return nil }
func (s *DiskSegment) NodeByID(ctx context.Context, id string) (dataset.Node, error) {
	for n, err := range s.Data.NodeRows() {
		if err != nil {
			return dataset.Node{}, err
		}
		if err = ctx.Err(); err != nil {
			return dataset.Node{}, err
		}
		if n.ID == id {
			return n, nil
		}
	}
	return dataset.Node{}, adapter.ErrNotFound
}
func (s *DiskSegment) NodesByName(ctx context.Context, name string, k int) ([]dataset.Node, error) {
	var out []dataset.Node
	for n, err := range s.Data.NodeRows() {
		if err != nil {
			return nil, err
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if n.Name == name {
			out = append(out, n)
			slices.SortFunc(out, func(a, b dataset.Node) int { return strings.Compare(a.ID, b.ID) })
			if k >= 0 && len(out) > k {
				out = out[:k]
			}
		}
	}
	return out, nil
}
func (s *DiskSegment) FileNodes(ctx context.Context, path string) ([]dataset.Node, error) {
	var out []dataset.Node
	for n, err := range s.Data.NodeRows() {
		if err != nil {
			return nil, err
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if n.FilePath == path {
			out = append(out, n)
		}
	}
	return out, nil
}
func (s *DiskSegment) edges(ctx context.Context, id, kind string, incoming bool) ([]dataset.Edge, error) {
	if s.index != nil {
		return s.index.edges(ctx, id, kind, incoming)
	}
	var out []dataset.Edge
	for e, err := range s.Data.EdgeRows() {
		if err != nil {
			return nil, err
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		key := e.From
		if incoming {
			key = e.To
		}
		if key == id && (kind == "" || kind == "any" || kind == e.Kind) {
			out = append(out, e)
		}
	}
	return out, nil
}
func (s *DiskSegment) OutEdges(ctx context.Context, id, kind string) ([]dataset.Edge, error) {
	return s.edges(ctx, id, kind, false)
}
func (s *DiskSegment) InEdges(ctx context.Context, id, kind string) ([]dataset.Edge, error) {
	return s.edges(ctx, id, kind, true)
}

type diskText struct{ id, body, repo string }

func texts(ds []dataset.Dataset, content bool) iter.Seq2[diskText, error] {
	return func(yield func(diskText, error) bool) {
		for _, d := range ds {
			if content {
				for v, e := range d.ContentRows() {
					if !yield(diskText{v.ChunkID, v.Body, d.Repo}, e) {
						return
					}
				}
			} else {
				for v, e := range d.SymbolRows() {
					if !yield(diskText{v.NodeID, v.Tokens, d.Repo}, e) {
						return
					}
				}
			}
		}
	}
}
func diskSearch(ctx context.Context, ds []dataset.Dataset, content bool, q string, k int, visit func(adapter.Hit, string)) ([]adapter.Hit, error) {
	terms := Tokens(q)
	slices.Sort(terms)
	terms = slices.Compact(terms)
	if len(terms) == 0 {
		return nil, ctx.Err()
	}
	df := make([]int, len(terms))
	n, total := 0, 0
	for doc, err := range texts(ds, content) {
		if err != nil {
			return nil, err
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		tokens := Tokens(doc.body)
		n++
		total += len(tokens)
		for i, t := range terms {
			if slices.Contains(tokens, t) {
				df[i]++
			}
		}
	}
	if n == 0 {
		return nil, nil
	}
	avg := float64(total) / float64(n)
	var hits []adapter.Hit
	for doc, err := range texts(ds, content) {
		if err != nil {
			return nil, err
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		tokens := Tokens(doc.body)
		score := 0.
		matched := false
		for i, t := range terms {
			tf := 0.
			for _, v := range tokens {
				if v == t {
					tf++
				}
			}
			if tf == 0 {
				continue
			}
			matched = true
			idf := math.Log(1 + (float64(n-df[i])+.5)/(float64(df[i])+.5))
			score += idf * tf * 2.2 / (tf + 1.2*(.25+.75*float64(len(tokens))/avg))
		}
		if !matched {
			continue
		}
		hit := adapter.Hit{ID: doc.id, Score: score}
		if visit != nil {
			visit(hit, doc.repo)
		}
		hits = adapter.Top(append(hits, hit), k)
	}
	return hits, nil
}
func (s *DiskSegment) SearchSymbols(ctx context.Context, q string, k int) ([]adapter.Hit, error) {
	return diskSearch(ctx, []dataset.Dataset{s.Data}, false, q, k, nil)
}
func (s *DiskSegment) SearchContent(ctx context.Context, q string, k int) ([]adapter.Hit, error) {
	return diskSearch(ctx, []dataset.Dataset{s.Data}, true, q, k, nil)
}
func (s *DiskSegment) VectorTopK(ctx context.Context, q []float32, k int) ([]adapter.Hit, error) {
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
	var hits []adapter.Hit
	for v, err := range s.Data.VectorRows() {
		if err != nil {
			return nil, err
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		hits = adapter.Top(append(hits, adapter.Hit{ID: v.NodeID, Score: cosine(q, v.Values)}), k)
	}
	return hits, nil
}

type DiskMulti struct {
	adapter.Fanout
	Data []dataset.Dataset
}

func (m *DiskMulti) SearchSymbols(ctx context.Context, q string, k int) ([]adapter.Hit, error) {
	return diskSearch(ctx, m.Data, false, q, k, nil)
}
func DiskView(ds []dataset.Dataset) (map[string]adapter.Segment, *DiskMulti) {
	segments := map[string]adapter.Segment{}
	for _, d := range ds {
		segments[d.Repo] = &DiskSegment{Data: d}
	}
	return segments, &DiskMulti{Fanout: adapter.Fanout{Segments: segments}, Data: ds}
}

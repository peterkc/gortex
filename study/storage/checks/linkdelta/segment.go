package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/blevesearch/bleve/v2"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"gxstudy/oracle"
	"iter"
	"math"
	"os"
	"slices"
	"strings"
)

type baseResources interface {
	adapter.Segment
	NodeRows(context.Context) iter.Seq2[dataset.Node, error]
	EdgeRows(context.Context) iter.Seq2[dataset.Edge, error]
	FileRows(context.Context) iter.Seq2[dataset.File, error]
	SymbolRows(context.Context) iter.Seq2[dataset.Symbol, error]
	ContentRows(context.Context) iter.Seq2[dataset.Content, error]
	VectorRows(context.Context) iter.Seq2[dataset.Vector, error]
	TextIndex(string) (bleve.Index, error)
}
type layer struct {
	symbols, content bleve.Index
	rows             []row
	files, nodes     map[string]bool
}
type segment struct {
	external         adapter.Segment
	openedText       []bleve.Index
	symbols, content bleve.Index
	ref              adapter.SegmentRef
	base             *os.File
	index            []position
	keys             map[string][]int
	layers           []layer
}

func lookup(parts ...string) string { b, _ := json.Marshal(parts); return string(b) }
func (s *segment) Close() error {
	var err error
	for _, ix := range s.openedText {
		err = errors.Join(err, ix.Close())
	}
	if s.external != nil {
		err = errors.Join(err, s.external.Close())
	}
	if s.base != nil {
		err = errors.Join(err, s.base.Close())
	}
	return err
}
func (s *segment) masked(owner, source string, after int) bool {
	for i := after + 1; i < len(s.layers); i++ {
		l := s.layers[i]
		if (owner != "" && l.files[owner]) || (source != "" && l.nodes[source]) {
			return true
		}
	}
	return false
}
func matches(r row, field, key string) bool {
	switch field {
	case "":
		return true
	case "key":
		return r.Key == key
	case "name":
		return r.Name == key
	case "owner":
		return r.Owner == key
	case "source":
		return r.Source == key
	case "target":
		return r.Target == key
	}
	return false
}
func (s *segment) scan(ctx context.Context, family, field, key string, visit func(row) error) error {
	if s.external != nil {
		err := s.scanExternal(ctx, family, field, key, func(r row) error {
			if s.masked(r.Owner, r.Source, -1) {
				return nil
			}
			return visit(r)
		})
		if err != nil {
			return err
		}
	}
	indices := s.keys[lookup("family", family)]
	if field != "" {
		indices = s.keys[lookup(family, field, key)]
	}
	for _, i := range indices {
		if err := ctx.Err(); err != nil {
			return err
		}
		p := s.index[i]
		if s.masked(p.Owner, p.Source, -1) {
			continue
		}
		raw := make([]byte, p.Length)
		if _, err := s.base.ReadAt(raw, p.Offset); err != nil {
			return err
		}
		var r row
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		if err := visit(r); err != nil {
			return err
		}
	}
	for i, l := range s.layers {
		for _, r := range l.rows {
			if err := ctx.Err(); err != nil {
				return err
			}
			if r.Family != family || !matches(r, field, key) || s.masked(r.Owner, r.Source, i) {
				continue
			}
			if err := visit(r); err != nil {
				return err
			}
		}
	}
	return nil
}
func (s *segment) NodeByID(ctx context.Context, id string) (dataset.Node, error) {
	var n dataset.Node
	count := 0
	err := s.scan(ctx, "node", "key", id, func(r row) error { count++; return json.Unmarshal(r.Value, &n) })
	if err != nil {
		return n, err
	}
	if count == 0 {
		return n, adapter.ErrNotFound
	}
	if count != 1 {
		return n, fmt.Errorf("duplicate visible node %q", id)
	}
	return n, nil
}
func (s *segment) nodes(ctx context.Context, field, key string) ([]dataset.Node, error) {
	var out []dataset.Node
	err := s.scan(ctx, "node", field, key, func(r row) error {
		var n dataset.Node
		if err := json.Unmarshal(r.Value, &n); err != nil {
			return err
		}
		out = append(out, n)
		return nil
	})
	return out, err
}
func (s *segment) NodesByName(ctx context.Context, name string, k int) ([]dataset.Node, error) {
	if k == 0 {
		return nil, nil
	}
	out, err := s.nodes(ctx, "name", name)
	slices.SortFunc(out, func(a, b dataset.Node) int { return strings.Compare(a.ID, b.ID) })
	if k >= 0 && len(out) > k {
		out = out[:k]
	}
	return out, err
}
func (s *segment) FileNodes(ctx context.Context, path string) ([]dataset.Node, error) {
	return s.nodes(ctx, "owner", path)
}
func (s *segment) edges(ctx context.Context, field, id, kind string) ([]dataset.Edge, error) {
	var out []dataset.Edge
	err := s.scan(ctx, "edge", field, id, func(r row) error {
		var e dataset.Edge
		if err := json.Unmarshal(r.Value, &e); err != nil {
			return err
		}
		if kind == "" || kind == "any" || e.Kind == kind {
			out = append(out, e)
		}
		return nil
	})
	return out, err
}
func (s *segment) OutEdges(ctx context.Context, id, kind string) ([]dataset.Edge, error) {
	return s.edges(ctx, "source", id, kind)
}
func (s *segment) InEdges(ctx context.Context, id, kind string) ([]dataset.Edge, error) {
	return s.edges(ctx, "target", id, kind)
}
func (s *segment) search(ctx context.Context, family, q string, k int) ([]adapter.Hit, error) {
	if k == 0 {
		return nil, nil
	}
	terms := oracle.Tokens(q)
	slices.Sort(terms)
	terms = slices.Compact(terms)
	if len(terms) == 0 {
		return nil, nil
	}
	text := func(r row) (string, error) {
		if family == "symbol" {
			var v dataset.Symbol
			err := json.Unmarshal(r.Value, &v)
			return v.Tokens, err
		}
		var v dataset.Content
		err := json.Unmarshal(r.Value, &v)
		return v.Body, err
	}
	df := make([]int, len(terms))
	count, total := 0, 0
	err := s.scan(ctx, family, "", "", func(r row) error {
		body, err := text(r)
		if err != nil {
			return err
		}
		tokens := oracle.Tokens(body)
		count++
		total += len(tokens)
		for i, t := range terms {
			if slices.Contains(tokens, t) {
				df[i]++
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	avg := float64(total) / float64(count)
	var hits []adapter.Hit
	err = s.scan(ctx, family, "", "", func(r row) error {
		body, err := text(r)
		if err != nil {
			return err
		}
		tokens := oracle.Tokens(body)
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
			idf := math.Log(1 + (float64(count-df[i])+.5)/(float64(df[i])+.5))
			score += idf * tf * 2.2 / (tf + 1.2*(.25+.75*float64(len(tokens))/avg))
		}
		if matched {
			hits = adapter.Top(append(hits, adapter.Hit{ID: r.Key, Score: score}), k)
		}
		return nil
	})
	return hits, err
}
func (s *segment) SearchSymbols(ctx context.Context, q string, k int) ([]adapter.Hit, error) {
	if s.external != nil {
		if len(s.layers) == 0 {
			return s.external.SearchSymbols(ctx, q, k)
		}
		return nativeSearch(ctx, s.symbols, q, k)
	}
	return s.search(ctx, "symbol", q, k)
}
func (s *segment) SearchContent(ctx context.Context, q string, k int) ([]adapter.Hit, error) {
	if s.external != nil {
		if len(s.layers) == 0 {
			return s.external.SearchContent(ctx, q, k)
		}
		return nativeSearch(ctx, s.content, q, k)
	}
	return s.search(ctx, "content", q, k)
}
func (s *segment) VectorTopK(ctx context.Context, q []float32, k int) ([]adapter.Hit, error) {
	if s.external != nil && len(s.layers) == 0 {
		return s.external.VectorTopK(ctx, q, k)
	}
	if len(q) != dataset.Dimensions {
		return nil, fmt.Errorf("invalid vector dimensions")
	}
	norm := 0.
	for _, v := range q {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return nil, fmt.Errorf("nonfinite vector")
		}
		norm += float64(v) * float64(v)
	}
	if norm == 0 {
		return nil, fmt.Errorf("zero vector")
	}
	if k == 0 {
		return nil, nil
	}
	var hits []adapter.Hit
	err := s.scan(ctx, "vector", "", "", func(r row) error {
		var v dataset.Vector
		if err := json.Unmarshal(r.Value, &v); err != nil {
			return err
		}
		if len(v.Values) != len(q) {
			return fmt.Errorf("stored vector dimensions")
		}
		dot, n := 0., 0.
		for i, x := range v.Values {
			dot += float64(x) * float64(q[i])
			n += float64(x) * float64(x)
		}
		score := 0.
		if n > 0 {
			score = dot / math.Sqrt(n*norm)
		}
		hits = adapter.Top(append(hits, adapter.Hit{ID: v.NodeID, Score: score}), k)
		return nil
	})
	return hits, err
}

func (s *segment) scanExternal(ctx context.Context, family, field, key string, visit func(row) error) error {
	base := s.external.(baseResources)
	add := func(r row, v any) error {
		raw, e := json.Marshal(v)
		if e != nil {
			return e
		}
		r.Value = raw
		return visit(r)
	}
	if family == "node" && field != "" {
		var ns []dataset.Node
		var e error
		switch field {
		case "key":
			n, err := base.NodeByID(ctx, key)
			if errors.Is(err, adapter.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			ns = []dataset.Node{n}
		case "name":
			ns, e = base.NodesByName(ctx, key, -1)
		case "owner":
			ns, e = base.FileNodes(ctx, key)
		default:
			return fmt.Errorf("unsupported base node filter")
		}
		if e != nil {
			return e
		}
		for _, n := range ns {
			if e = add(row{Family: "node", Key: n.ID, Owner: n.FilePath, Source: n.ID, Name: n.Name}, n); e != nil {
				return e
			}
		}
		return nil
	}
	if family == "edge" && field != "" {
		var es []dataset.Edge
		var e error
		switch field {
		case "source":
			es, e = base.OutEdges(ctx, key, "any")
		case "target":
			es, e = base.InEdges(ctx, key, "any")
		default:
			return fmt.Errorf("unsupported base edge filter")
		}
		if e != nil {
			return e
		}
		for _, v := range es {
			if e = add(row{Family: "edge", Owner: v.FilePath, Source: v.From, Target: v.To}, v); e != nil {
				return e
			}
		}
		return nil
	}
	switch family {
	case "symbol":
		for v, e := range base.SymbolRows(ctx) {
			if e != nil {
				return e
			}
			if e = add(row{Family: family, Key: v.NodeID, Source: v.NodeID}, v); e != nil {
				return e
			}
		}
	case "content":
		for v, e := range base.ContentRows(ctx) {
			if e != nil {
				return e
			}
			if e = add(row{Family: family, Key: v.ChunkID, Source: v.NodeID, Owner: v.FilePath}, v); e != nil {
				return e
			}
		}
	case "vector":
		for v, e := range base.VectorRows(ctx) {
			if e != nil {
				return e
			}
			if e = add(row{Family: family, Key: v.NodeID, Source: v.NodeID}, v); e != nil {
				return e
			}
		}
	default:
		return fmt.Errorf("unsupported streamed base family %q", family)
	}
	return nil
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/blevesearch/bleve/v2"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"math"
	"slices"
	"strings"
)

type baseResources interface {
	adapter.Segment
	TextIndex(string) (bleve.Index, error)
}
type layer struct {
	symbols, content     bleve.Index
	files, nodes, chunks map[string]bool
	byID                 map[string]dataset.Node
	byName, byFile       map[string][]dataset.Node
	outgoing, incoming   map[string][]dataset.Edge
	vectors              map[string]dataset.Vector
	text                 map[string][]chunkOwner
}
type chunkOwner struct{ ID, Node, File string }
type ownership struct{ ByFile, ByNode map[string][]string }
type segment struct {
	external                      adapter.Segment
	openedText                    []bleve.Index
	symbols, content              bleve.Index
	ref                           adapter.SegmentRef
	layers                        []layer
	fileMask, nodeMask, chunkMask map[string]int
	owners                        ownership
}

func lookup(parts ...string) string { b, _ := json.Marshal(parts); return string(b) }
func indexLayer(rows []row) (l layer, err error) {
	l = layer{files: map[string]bool{}, nodes: map[string]bool{}, chunks: map[string]bool{}, byID: map[string]dataset.Node{}, byName: map[string][]dataset.Node{}, byFile: map[string][]dataset.Node{}, outgoing: map[string][]dataset.Edge{}, incoming: map[string][]dataset.Edge{}, vectors: map[string]dataset.Vector{}, text: map[string][]chunkOwner{}}
	for _, r := range rows {
		switch r.Family {
		case "mask_file":
			l.files[r.Key] = true
		case "mask_node":
			l.nodes[r.Key] = true
		case "mask_chunk":
			l.chunks[r.Key] = true
		case "node":
			var n dataset.Node
			if err = json.Unmarshal(r.Value, &n); err != nil {
				return l, err
			}
			if n.ID == "" || n.ID != r.Key || n.FilePath != r.Owner || n.Name != r.Name || n.ID != r.Source {
				return l, fmt.Errorf("node envelope mismatch")
			}
			if _, ok := l.byID[n.ID]; ok {
				return l, fmt.Errorf("duplicate layer node")
			}
			l.byID[n.ID] = n
			l.byName[n.Name] = append(l.byName[n.Name], n)
			l.byFile[n.FilePath] = append(l.byFile[n.FilePath], n)
		case "edge":
			var e dataset.Edge
			if err = json.Unmarshal(r.Value, &e); err != nil {
				return l, err
			}
			if e.From != r.Source || e.To != r.Target || e.FilePath != r.Owner {
				return l, fmt.Errorf("edge envelope mismatch")
			}
			l.outgoing[e.From] = append(l.outgoing[e.From], e)
			l.incoming[e.To] = append(l.incoming[e.To], e)
		case "vector":
			var v dataset.Vector
			if err = json.Unmarshal(r.Value, &v); err != nil {
				return l, err
			}
			if v.NodeID != r.Key || len(v.Values) != dataset.Dimensions {
				return l, fmt.Errorf("vector envelope mismatch")
			}
			if _, ok := l.vectors[v.NodeID]; ok {
				return l, fmt.Errorf("duplicate layer vector")
			}
			l.vectors[v.NodeID] = v
		case "symbol":
			var v dataset.Symbol
			if err = json.Unmarshal(r.Value, &v); err != nil {
				return l, err
			}
			if v.NodeID != r.Key || v.NodeID != r.Source {
				return l, fmt.Errorf("symbol envelope mismatch")
			}
			l.text["symbol"] = append(l.text["symbol"], chunkOwner{ID: v.NodeID, Node: v.NodeID})
		case "content":
			var v dataset.Content
			if err = json.Unmarshal(r.Value, &v); err != nil {
				return l, err
			}
			if v.ChunkID != r.Key || v.NodeID != r.Source || v.FilePath != r.Owner {
				return l, fmt.Errorf("content envelope mismatch")
			}
			l.text["content"] = append(l.text["content"], chunkOwner{v.ChunkID, v.NodeID, v.FilePath})
		case "file":
			var v dataset.File
			if err = json.Unmarshal(r.Value, &v); err != nil {
				return l, err
			}
		default:
			return l, fmt.Errorf("unknown indexed family")
		}
	}
	return l, nil
}
func (s *segment) indexMasks() {
	s.fileMask = map[string]int{}
	s.nodeMask = map[string]int{}
	s.chunkMask = map[string]int{}
	for i, l := range s.layers {
		for k := range l.files {
			s.fileMask[k] = i + 1
		}
		for k := range l.nodes {
			s.nodeMask[k] = i + 1
		}
		for k := range l.chunks {
			s.chunkMask[k] = i + 1
		}
	}
}
func (s *segment) masked(owner, source string, after int) bool {
	return (owner != "" && s.fileMask[owner] > after+1) || (source != "" && s.nodeMask[source] > after+1)
}
func (s *segment) Close() error {
	var err error
	for _, ix := range s.openedText {
		err = errors.Join(err, ix.Close())
	}
	return errors.Join(err, s.external.Close())
}
func (s *segment) NodeByID(ctx context.Context, id string) (dataset.Node, error) {
	n, e := s.external.NodeByID(ctx, id)
	if e != nil && !errors.Is(e, adapter.ErrNotFound) {
		return n, e
	}
	count := 0
	if e == nil && !s.masked(n.FilePath, n.ID, -1) {
		count = 1
	}
	for i, l := range s.layers {
		if v, ok := l.byID[id]; ok && !s.masked(v.FilePath, v.ID, i) {
			n = v
			count++
		}
	}
	if count == 0 {
		return dataset.Node{}, adapter.ErrNotFound
	}
	if count != 1 {
		return n, fmt.Errorf("duplicate visible node %q", id)
	}
	return n, nil
}
func (s *segment) nodes(ctx context.Context, field, key string) ([]dataset.Node, error) {
	var base []dataset.Node
	var err error
	if field == "name" {
		base, err = s.external.NodesByName(ctx, key, -1)
	} else {
		base, err = s.external.FileNodes(ctx, key)
	}
	if err != nil {
		return nil, err
	}
	out := make([]dataset.Node, 0, len(base))
	for _, n := range base {
		if !s.masked(n.FilePath, n.ID, -1) {
			out = append(out, n)
		}
	}
	for i, l := range s.layers {
		rows := l.byFile[key]
		if field == "name" {
			rows = l.byName[key]
		}
		for _, n := range rows {
			if err = ctx.Err(); err != nil {
				return nil, err
			}
			if !s.masked(n.FilePath, n.ID, i) {
				out = append(out, n)
			}
		}
	}
	return out, nil
}
func (s *segment) NodesByName(ctx context.Context, name string, k int) ([]dataset.Node, error) {
	if k == 0 {
		return nil, nil
	}
	if len(s.layers) == 0 {
		return s.external.NodesByName(ctx, name, k)
	}
	out, err := s.nodes(ctx, "name", name)
	slices.SortFunc(out, func(a, b dataset.Node) int { return strings.Compare(a.ID, b.ID) })
	if k >= 0 && len(out) > k {
		out = out[:k]
	}
	return out, err
}
func (s *segment) FileNodes(ctx context.Context, path string) ([]dataset.Node, error) {
	if len(s.layers) == 0 {
		return s.external.FileNodes(ctx, path)
	}
	return s.nodes(ctx, "file", path)
}
func (s *segment) edges(ctx context.Context, field, id, kind string) ([]dataset.Edge, error) {
	var base []dataset.Edge
	var err error
	if field == "source" {
		base, err = s.external.OutEdges(ctx, id, kind)
	} else {
		base, err = s.external.InEdges(ctx, id, kind)
	}
	if err != nil {
		return nil, err
	}
	out := make([]dataset.Edge, 0, len(base))
	for _, e := range base {
		if !s.masked(e.FilePath, e.From, -1) {
			out = append(out, e)
		}
	}
	for i, l := range s.layers {
		rows := l.incoming[id]
		if field == "source" {
			rows = l.outgoing[id]
		}
		for _, e := range rows {
			if err = ctx.Err(); err != nil {
				return nil, err
			}
			if (kind == "" || kind == "any" || kind == e.Kind) && !s.masked(e.FilePath, e.From, i) {
				out = append(out, e)
			}
		}
	}
	return out, nil
}
func (s *segment) OutEdges(ctx context.Context, id, kind string) ([]dataset.Edge, error) {
	if len(s.layers) == 0 {
		return s.external.OutEdges(ctx, id, kind)
	}
	return s.edges(ctx, "source", id, kind)
}
func (s *segment) InEdges(ctx context.Context, id, kind string) ([]dataset.Edge, error) {
	if len(s.layers) == 0 {
		return s.external.InEdges(ctx, id, kind)
	}
	return s.edges(ctx, "target", id, kind)
}
func (s *segment) SearchSymbols(ctx context.Context, q string, k int) ([]adapter.Hit, error) {
	if len(s.layers) == 0 {
		return s.external.SearchSymbols(ctx, q, k)
	}
	return nativeSearch(ctx, s.symbols, q, k)
}
func (s *segment) SearchContent(ctx context.Context, q string, k int) ([]adapter.Hit, error) {
	if len(s.layers) == 0 {
		return s.external.SearchContent(ctx, q, k)
	}
	return nativeSearch(ctx, s.content, q, k)
}
func (s *segment) VectorTopK(ctx context.Context, q []float32, k int) ([]adapter.Hit, error) {
	if len(s.layers) == 0 {
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
	// Every masked base vector belongs to an old node ID. Overfetch by that
	// upper bound, then use the same exact cosine and tie order for delta hits.
	baseK := k
	if k > 0 {
		if k > math.MaxInt-len(s.nodeMask) {
			baseK = -1
		} else {
			baseK += len(s.nodeMask)
		}
	}
	base, err := s.external.VectorTopK(ctx, q, baseK)
	if err != nil {
		return nil, err
	}
	var hits []adapter.Hit
	for _, h := range base {
		if s.nodeMask[h.ID] == 0 {
			hits = append(hits, h)
		}
	}
	for i, l := range s.layers {
		for id, v := range l.vectors {
			if err = ctx.Err(); err != nil {
				return nil, err
			}
			if s.masked("", id, i) {
				continue
			}
			dot, n := 0., 0.
			for j, x := range v.Values {
				dot += float64(x) * float64(q[j])
				n += float64(x) * float64(x)
			}
			score := 0.
			if n > 0 {
				score = dot / math.Sqrt(n*norm)
			}
			hits = adapter.Top(append(hits, adapter.Hit{ID: id, Score: score}), k)
		}
	}
	return adapter.Top(hits, k), nil
}

package base

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"math"
	"slices"
	"strings"

	"github.com/cockroachdb/pebble/v2"
	"github.com/parquet-go/parquet-go"
	"gxstudy/adapter"
	"gxstudy/dataset"
)

func decodeRow[T any](ctx context.Context, s *Segment, table string, r row) (value T, err error) {
	switch target := any(&value).(type) {
	case *dataset.Node:
		if table == "nodes" {
			*target, err = decodeNode(r.Raw)
		} else {
			*target, err = s.nodeAt(ctx, r.Local)
		}
	case *dataset.Edge:
		if table == "edges" {
			*target, err = decodeEdge(r.Raw, func(local uint64) (string, error) { return s.logicalAt(ctx, local) })
		} else {
			*target, err = s.edgeAt(ctx, r.Local)
		}
	default:
		err = json.Unmarshal(r.Raw, &value)
	}
	return value, err
}
func lookupRows[T any](ctx context.Context, s *Segment, table, key string, limit int) (values []T, err error) {
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	span, found, err := tableRange(s.tables[table+".ranges"], key)
	if err != nil || !found {
		return nil, err
	}
	if limit >= 0 && span.End-span.First > int64(limit) {
		span.End = span.First + int64(limit)
	}
	err = s.scan(ctx, table, &span, func(r row) error {
		if r.Key != key {
			return fmt.Errorf("range directory key mismatch")
		}
		value, e := decodeRow[T](ctx, s, table, r)
		if e != nil {
			return e
		}
		values = append(values, value)
		return nil
	})
	return values, err
}
func (s *Segment) nodeAt(ctx context.Context, local uint64) (n dataset.Node, err error) {
	if local >= uint64(s.seal.Meta.Counts.Nodes) {
		return n, adapter.ErrNotFound
	}
	span := rowRange{First: int64(local), End: int64(local) + 1}
	err = s.scan(ctx, "nodes", &span, func(r row) error { var e error; n, e = decodeNode(r.Raw); return e })
	return n, err
}
func (s *Segment) logicalAt(ctx context.Context, local uint64) (id string, err error) {
	if local < uint64(s.seal.Meta.Counts.Nodes) {
		n, e := s.nodeAt(ctx, local)
		return n.ID, e
	}
	span, found, err := tableRange(s.tables["terminals.ranges"], localKey(local))
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("missing terminal local ID")
	}
	err = s.scan(ctx, "terminals", &span, func(r row) error { id = string(r.Raw); return nil })
	return id, err
}
func (s *Segment) edgeAt(ctx context.Context, local uint64) (edge dataset.Edge, err error) {
	span := rowRange{First: int64(local), End: int64(local) + 1}
	err = s.scan(ctx, "edges", &span, func(r row) error {
		var e error
		edge, e = decodeEdge(r.Raw, func(id uint64) (string, error) { return s.logicalAt(ctx, id) })
		return e
	})
	return edge, err
}
func (s *Segment) findLocal(ctx context.Context, table, id string) (local uint64, err error) {
	if err = ctx.Err(); err != nil {
		return 0, err
	}
	reader := parquet.NewGenericReader[row](s.tables[table].parquet)
	defer func() { err = errors.Join(err, reader.Close()) }()
	low, high := int64(0), reader.NumRows()
	one := make([]row, 1)
	read := func(index int64) error {
		if e := reader.SeekToRow(index); e != nil {
			return e
		}
		n, e := reader.Read(one)
		if e != nil && e != io.EOF {
			return e
		}
		if n != 1 {
			return fmt.Errorf("truncated logical-ID lookup")
		}
		return nil
	}
	for low < high {
		middle := low + (high-low)/2
		if err = read(middle); err != nil {
			return 0, err
		}
		if one[0].Key < id {
			low = middle + 1
		} else {
			high = middle
		}
	}
	if low == reader.NumRows() {
		return 0, adapter.ErrNotFound
	}
	if err = read(low); err != nil {
		return 0, err
	}
	if one[0].Key != id {
		return 0, adapter.ErrNotFound
	}
	return one[0].Local, nil
}
func (s *Segment) localID(ctx context.Context, id string) (uint64, error) {
	return s.findLocal(ctx, "ids", id)
}
func (s *Segment) NodeByID(ctx context.Context, id string) (dataset.Node, error) {
	if err := ctx.Err(); err != nil {
		return dataset.Node{}, err
	}
	if s.side != nil {
		hash := sha256.Sum256([]byte(id))
		raw, closer, err := s.side.Get(append([]byte("N/"), hash[:]...))
		if errors.Is(err, pebble.ErrNotFound) {
			return dataset.Node{}, adapter.ErrNotFound
		}
		if err != nil {
			return dataset.Node{}, err
		}
		local, e := parseLocal(raw)
		err = errors.Join(e, closer.Close())
		if err != nil {
			return dataset.Node{}, err
		}
		n, err := s.nodeAt(ctx, local)
		if err == nil && n.ID != id {
			return dataset.Node{}, fmt.Errorf("side lookup identity mismatch")
		}
		return n, err
	}
	local, err := s.localID(ctx, id)
	if err != nil {
		return dataset.Node{}, err
	}
	return s.nodeAt(ctx, local)
}
func (s *Segment) NodesByName(ctx context.Context, name string, k int) ([]dataset.Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if k == 0 {
		return nil, nil
	}
	var nodes []dataset.Node
	var err error
	if s.side != nil {
		nodes, err = s.sideNodes(ctx, indexKey("A", name), k)
	} else {
		nodes, err = lookupRows[dataset.Node](ctx, s, "names", name, k)
	}
	if err != nil {
		return nil, err
	}
	slices.SortFunc(nodes, func(a, b dataset.Node) int { return strings.Compare(a.ID, b.ID) })
	if k >= 0 && len(nodes) > k {
		nodes = nodes[:k]
	}
	return nodes, nil
}
func (s *Segment) FileNodes(ctx context.Context, path string) ([]dataset.Node, error) {
	var nodes []dataset.Node
	var err error
	if s.side != nil {
		nodes, err = s.sideNodes(ctx, indexKey("P", path), -1)
	} else {
		nodes, err = lookupRows[dataset.Node](ctx, s, "paths", path, -1)
	}
	slices.SortFunc(nodes, func(a, b dataset.Node) int { return strings.Compare(a.ID, b.ID) })
	return nodes, err
}
func (s *Segment) edges(ctx context.Context, table, id, kind string) ([]dataset.Edge, error) {
	local, err := s.localID(ctx, id)
	if errors.Is(err, adapter.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	tags := make([]uint64, 0)
	if kind != "" && kind != "any" {
		tag, e := s.findLocal(ctx, "kinds", kind)
		if errors.Is(e, adapter.ErrNotFound) {
			return nil, nil
		}
		if e != nil {
			return nil, e
		}
		tags = append(tags, tag)
	} else {
		for tag := uint64(0); tag < uint64(s.seal.Verification["kinds"].Rows); tag++ {
			tags = append(tags, tag)
		}
	}
	var edges []dataset.Edge
	for _, tag := range tags {
		span, e := s.offsetSpan(table, local, tag)
		if e != nil {
			return nil, e
		}
		err = s.scan(ctx, table, &span, func(r row) error {
			edge, e := s.edgeAt(ctx, r.Local)
			if e != nil {
				return e
			}
			edges = append(edges, edge)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return edges, err
}
func (s *Segment) OutEdges(ctx context.Context, id, kind string) ([]dataset.Edge, error) {
	return s.edges(ctx, "out", id, kind)
}
func (s *Segment) InEdges(ctx context.Context, id, kind string) ([]dataset.Edge, error) {
	return s.edges(ctx, "in", id, kind)
}
func validVector(vector []float32) error {
	if len(vector) != dataset.Dimensions {
		return fmt.Errorf("vector dimensions: %d", len(vector))
	}
	for _, x := range vector {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return fmt.Errorf("nonfinite vector")
		}
	}
	return nil
}
func (s *Segment) VectorTopK(ctx context.Context, query []float32, k int) ([]adapter.Hit, error) {
	if err := validVector(query); err != nil {
		return nil, err
	}
	if k == 0 {
		return nil, nil
	}
	var norm float64
	for _, x := range query {
		norm += float64(x) * float64(x)
	}
	var hits []adapter.Hit
	err := s.scan(ctx, "vectors", nil, func(r row) error {
		var v dataset.Vector
		if err := json.Unmarshal(r.Raw, &v); err != nil {
			return err
		}
		if err := validVector(v.Values); err != nil {
			return err
		}
		var dot, vnorm float64
		for i, x := range v.Values {
			dot += float64(x) * float64(query[i])
			vnorm += float64(x) * float64(x)
		}
		score := 0.
		if norm > 0 && vnorm > 0 {
			score = dot / math.Sqrt(norm*vnorm)
		}
		hits = append(hits, adapter.Hit{ID: v.NodeID, Score: score})
		if k > 0 && len(hits) >= 2*k {
			hits = adapter.Top(hits, k)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return adapter.Top(hits, k), nil
}
func allRows[T any](ctx context.Context, s *Segment, table string) (values []T, err error) {
	err = s.scan(ctx, table, nil, func(r row) error {
		value, e := decodeRow[T](ctx, s, table, r)
		if e != nil {
			return e
		}
		values = append(values, value)
		return nil
	})
	return values, err
}
func (s *Segment) records(ctx context.Context) (dataset.Dataset, error) {
	d := dataset.Dataset{Repo: s.seal.Repo, Meta: s.seal.Meta}
	var err error
	if d.Nodes, err = allRows[dataset.Node](ctx, s, "nodes"); err != nil {
		return d, err
	}
	if d.Edges, err = allRows[dataset.Edge](ctx, s, "edges"); err != nil {
		return d, err
	}
	if d.Files, err = allRows[dataset.File](ctx, s, "files"); err != nil {
		return d, err
	}
	if d.Symbols, err = allRows[dataset.Symbol](ctx, s, "symbols"); err != nil {
		return d, err
	}
	if d.Content, err = allRows[dataset.Content](ctx, s, "content"); err != nil {
		return d, err
	}
	d.Vectors, err = allRows[dataset.Vector](ctx, s, "vectors")
	return d, err
}
func (b Backend) BuildDelta(ctx context.Context, old adapter.SegmentRef, change adapter.FileChange, path string) (adapter.SegmentRef, adapter.BuildStats, error) {
	opened, err := b.Open(ctx, old.Path, old)
	if err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, err
	}
	d, err := opened.(*Segment).records(ctx)
	err = errors.Join(err, opened.Close())
	if err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, err
	}
	ref, stats, err := b.Build(ctx, adapter.ApplyChange(d, change), path)
	stats.Notes = append(stats.Notes, "delta is a full rebuild and decodes existing primary records, matching reference B; not the Check 4 delta candidate")
	return ref, stats, err
}

// OpenVersion exposes the test-only immutable reader to the other checks.
func (b Backend) OpenVersion(ctx context.Context, ref adapter.SegmentRef) (*Segment, error) {
	opened, err := b.Open(ctx, ref.Path, ref)
	if err != nil {
		return nil, err
	}
	return opened.(*Segment), nil
}

var stopIteration = errors.New("consumer stopped row iteration")

func recordRows[T any](ctx context.Context, s *Segment, table string) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		err := s.scan(ctx, table, nil, func(r row) error {
			value, e := decodeRow[T](ctx, s, table, r)
			if e != nil {
				return e
			}
			if !yield(value, nil) {
				return stopIteration
			}
			return nil
		})
		if err != nil && !errors.Is(err, stopIteration) {
			var zero T
			yield(zero, err)
		}
	}
}
func (s *Segment) NodeRows(ctx context.Context) iter.Seq2[dataset.Node, error] {
	return recordRows[dataset.Node](ctx, s, "nodes")
}
func (s *Segment) EdgeRows(ctx context.Context) iter.Seq2[dataset.Edge, error] {
	return recordRows[dataset.Edge](ctx, s, "edges")
}
func (s *Segment) FileRows(ctx context.Context) iter.Seq2[dataset.File, error] {
	return recordRows[dataset.File](ctx, s, "files")
}
func (s *Segment) SymbolRows(ctx context.Context) iter.Seq2[dataset.Symbol, error] {
	return recordRows[dataset.Symbol](ctx, s, "symbols")
}
func (s *Segment) ContentRows(ctx context.Context) iter.Seq2[dataset.Content, error] {
	return recordRows[dataset.Content](ctx, s, "content")
}
func (s *Segment) VectorRows(ctx context.Context) iter.Seq2[dataset.Vector, error] {
	return recordRows[dataset.Vector](ctx, s, "vectors")
}

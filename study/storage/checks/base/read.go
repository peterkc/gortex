package base

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"math"
	"slices"
	"strings"

	"github.com/cockroachdb/pebble/v2"
	"gxstudy/adapter"
	"gxstudy/dataset"
)

func lookupRows[T any](ctx context.Context, s *Segment, table, key string) (values []T, err error) {
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	span, found, err := tableRange(s.tables[table+".ranges"], key)
	if err != nil || !found {
		return nil, err
	}
	err = s.scan(ctx, table, &span, func(r row) error {
		if r.Key != key {
			return fmt.Errorf("range directory key mismatch")
		}
		var value T
		if e := json.Unmarshal(r.Raw, &value); e != nil {
			return e
		}
		values = append(values, value)
		return nil
	})
	return values, err
}
func (s *Segment) NodeByID(ctx context.Context, id string) (dataset.Node, error) {
	if err := ctx.Err(); err != nil {
		return dataset.Node{}, err
	}
	if s.side != nil {
		raw, closer, err := s.side.Get(indexKey("N", id))
		if errors.Is(err, pebble.ErrNotFound) {
			return dataset.Node{}, adapter.ErrNotFound
		}
		if err != nil {
			return dataset.Node{}, err
		}
		var n dataset.Node
		err = json.Unmarshal(raw, &n)
		return n, errors.Join(err, closer.Close())
	}
	rows, err := lookupRows[dataset.Node](ctx, s, "nodes", id)
	if err != nil {
		return dataset.Node{}, err
	}
	if len(rows) == 0 {
		return dataset.Node{}, adapter.ErrNotFound
	}
	if len(rows) != 1 {
		return dataset.Node{}, fmt.Errorf("duplicate ID rows")
	}
	return rows[0], nil
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
		nodes, err = s.sideNodes(ctx, indexKey("A", name))
	} else {
		nodes, err = lookupRows[dataset.Node](ctx, s, "names", name)
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
	if s.side != nil {
		return s.sideNodes(ctx, indexKey("P", path))
	}
	return lookupRows[dataset.Node](ctx, s, "paths", path)
}
func (s *Segment) edges(ctx context.Context, table, id, kind string) ([]dataset.Edge, error) {
	edges, err := lookupRows[dataset.Edge](ctx, s, table, id)
	if err != nil {
		return nil, err
	}
	if kind == "" || kind == "any" {
		return edges, nil
	}
	var result []dataset.Edge
	for _, edge := range edges {
		if edge.Kind == kind {
			result = append(result, edge)
		}
	}
	return result, nil
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
		var value T
		if err := json.Unmarshal(r.Raw, &value); err != nil {
			return err
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
	if d.Edges, err = allRows[dataset.Edge](ctx, s, "out"); err != nil {
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
			var value T
			if err := json.Unmarshal(r.Raw, &value); err != nil {
				return err
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
	return recordRows[dataset.Edge](ctx, s, "out")
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

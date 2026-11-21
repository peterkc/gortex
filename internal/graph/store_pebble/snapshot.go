package store_pebble

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

// Snapshot is a request-scoped reader of one published head. The caller owns
// its lifetime and must finish reads before closing it.
type Snapshot struct {
	snap       *pebble.Snapshot
	profile    Profile
	mu         sync.Mutex
	first      error
	searchMemo []searchMemoEntry
}

var _ graph.Reader = (*Snapshot)(nil)

func (s *Snapshot) Close() error { return s.snap.Close() }

// ReadError returns the first storage failure from an error-free graph.Reader
// method. The request owner checks this before returning its reply.
func (s *Snapshot) ReadError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.first
}

func (s *Snapshot) retain(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.first == nil {
		s.first = err
	}
}

type decodeRow[T any] func([]byte) (T, error)

func point[T any](s *Snapshot, key []byte, decode decodeRow[T]) (T, error) {
	raw, closer, err := s.snap.Get(key)
	if err != nil {
		var zero T
		if errors.Is(err, pebble.ErrNotFound) {
			return zero, nil
		}
		return zero, fmt.Errorf("read Pebble %q family: %w", key[:1], err)
	}
	row, err := decode(raw)
	return row, errors.Join(err, closer.Close())
}

func scan(ctx context.Context, s *Snapshot, prefix []byte, yield func(key, value []byte) (bool, error)) error {
	return scanRange(ctx, s, prefix, prefixEnd(prefix), yield)
}

func scanRange(ctx context.Context, s *Snapshot, lower, upper []byte, yield func(key, value []byte) (bool, error)) (err error) {
	i, err := s.snap.NewIterWithContext(ctx, &pebble.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, i.Close()) }()
	for ok := i.First(); ok; ok = i.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		value, err := i.ValueAndErr()
		if err != nil {
			return err
		}
		more, err := yield(i.Key(), value)
		if err != nil || !more {
			return err
		}
	}
	return errors.Join(ctx.Err(), i.Error())
}

func decodeRows[T any](s *Snapshot, prefix []byte, decode decodeRow[T], yield func(T) bool) {
	s.retain(scan(context.Background(), s, prefix, func(_, raw []byte) (bool, error) {
		row, err := decode(raw)
		if err != nil {
			return false, err
		}
		return yield(row), nil
	}))
}

func listRows[T any](s *Snapshot, prefix []byte, decode decodeRow[T]) []T {
	var out []T
	decodeRows(s, prefix, decode, func(row T) bool {
		out = append(out, row)
		return true
	})
	return out
}

func (s *Snapshot) GetNode(id string) *graph.Node {
	n, err := s.GetNodeContext(context.Background(), id)
	s.retain(err)
	return n
}

func (s *Snapshot) GetNodeContext(ctx context.Context, id string) (*graph.Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return point(s, keyStrings([]byte{familyNode}, id), decodeNode)
}

func (s *Snapshot) GetNodesByIDs(ids []string) map[string]*graph.Node {
	nodes, err := s.GetNodesByIDsContext(context.Background(), ids)
	s.retain(err)
	return nodes
}

func (s *Snapshot) GetNodesByIDsContext(ctx context.Context, ids []string) (map[string]*graph.Node, error) {
	out := make(map[string]*graph.Node, len(ids))
	ordered := slices.Clone(ids)
	slices.Sort(ordered)
	for _, id := range slices.Compact(ordered) {
		n, err := s.GetNodeContext(ctx, id)
		if err != nil {
			return nil, err
		}
		if n != nil {
			out[id] = n
		}
	}
	return out, ctx.Err()
}

const (
	nameExact byte = iota + 1
	nameQualified
	nameGram
	nameLanguage
	nameRepoLanguage
	nameNonContent
)

func (s *Snapshot) indexedNodes(prefix []byte, yield func(*graph.Node) bool) {
	decodeRows(s, prefix, func(raw []byte) (*graph.Node, error) {
		n, err := point(s, keyStrings([]byte{familyNode}, string(raw)), decodeNode)
		if err == nil && n == nil {
			err = invalid("missing indexed node")
		}
		return n, err
	}, yield)
}

func (s *Snapshot) findIndexedNodes(prefix []byte) []*graph.Node {
	return slices.Collect(iter.Seq[*graph.Node](func(yield func(*graph.Node) bool) { s.indexedNodes(prefix, yield) }))
}

func (s *Snapshot) GetNodeByQualName(name string) *graph.Node {
	if name == "" {
		return nil
	}
	var out *graph.Node
	s.indexedNodes(keyStrings([]byte{familyName, nameQualified}, name), func(n *graph.Node) bool {
		out = n
		return false
	})
	return out
}

func (s *Snapshot) FindNodesByName(name string) []*graph.Node {
	return s.findIndexedNodes(keyStrings([]byte{familyName, nameExact}, name))
}

// nameGrams indexes each distinct 1, 2 and 3-byte substring of the folded
// name. Byte grams are valid even across a rune boundary; the full predicate
// remains the final check.
func nameGrams(name string) []string {
	grams := make(map[string]struct{})
	for width := 1; width <= min(3, len(name)); width++ {
		for i := 0; i+width <= len(name); i++ {
			grams[name[i:i+width]] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(grams))
}

func (s *Snapshot) FindNodesByNameContaining(substr string, limit int) []*graph.Node {
	substr = strings.ToLower(substr)
	if substr == "" {
		return nil
	}
	width := min(3, len(substr))
	prefix := keyStrings([]byte{familyName, nameGram}, substr[:width])
	var out []*graph.Node
	s.indexedNodes(prefix, func(n *graph.Node) bool {
		if strings.Contains(strings.ToLower(n.Name), substr) {
			out = append(out, n)
		}
		return limit <= 0 || len(out) < limit
	})
	return out
}

func (s *Snapshot) GetFileNodes(path string) []*graph.Node {
	var out []*graph.Node
	for _, repo := range s.repos() {
		out = append(out, s.findIndexedNodes(keyStrings([]byte{familyFile}, repo, path))...)
	}
	return out
}

func (s *Snapshot) GetRepoNodes(repo string) []*graph.Node {
	return s.findIndexedNodes(keyStrings([]byte{familyRepo}, repo))
}

func (s *Snapshot) GetOutEdges(id string) []*graph.Edge {
	return listRows(s, keyStrings([]byte{familyEdge, edgeOut}, id), decodeEdge)
}

func (s *Snapshot) GetInEdges(id string) []*graph.Edge {
	return listRows(s, keyStrings([]byte{familyEdge, edgeIn}, id), decodeEdge)
}

var _ graph.RepoScopedInEdgeReader = (*Snapshot)(nil)

func (s *Snapshot) GetInEdgesByNodeIDsFromRepos(ids, repos []string) (map[string][]*graph.Edge, bool, error) {
	if len(ids) == 0 || len(repos) == 0 {
		return nil, true, nil
	}
	out := make(map[string][]*graph.Edge)
	ordered := slices.Clone(repos)
	slices.Sort(ordered)
	ordered = slices.Compact(ordered)
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		for _, repo := range ordered {
			if err := scan(context.Background(), s, keyStrings([]byte{familyEdge, edgeInRepo}, id, repo), func(_, raw []byte) (bool, error) {
				e, err := decodeEdge(raw)
				if err != nil {
					return false, err
				}
				out[id] = append(out[id], e)
				return true, nil
			}); err != nil {
				return nil, false, err
			}
		}
	}
	return out, true, nil
}

func (s *Snapshot) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	return readAdjacencies(ids, s.GetOutEdges)
}

func (s *Snapshot) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	return readAdjacencies(ids, s.GetInEdges)
}

func readAdjacencies(ids []string, read func(string) []*graph.Edge) map[string][]*graph.Edge {
	var out map[string][]*graph.Edge
	for _, id := range ids {
		if id == "" {
			continue
		}
		if out == nil {
			out = make(map[string][]*graph.Edge, len(ids))
		}
		if _, ok := out[id]; !ok {
			out[id] = read(id)
		}
	}
	// Keep the sentinel entries only while deduplicating input IDs.
	for id, edges := range out {
		if len(edges) == 0 {
			delete(out, id)
		}
	}
	return out
}

func (s *Snapshot) AllNodes() []*graph.Node { return listRows(s, []byte{familyNode}, decodeNode) }
func (s *Snapshot) AllEdges() []*graph.Edge {
	return listRows(s, []byte{familyEdge, edgeOut}, decodeEdge)
}

func (s *Snapshot) NodesByKind(kind graph.NodeKind) iter.Seq[*graph.Node] {
	return func(yield func(*graph.Node) bool) {
		s.indexedNodes(keyStrings([]byte{familyNodeKind}, string(kind)), yield)
	}
}

func (s *Snapshot) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	return func(yield func(*graph.Edge) bool) {
		s.scanEdgeKind(kind, decodeEdge, yield)
	}
}

func (s *Snapshot) scanEdgeKind(kind graph.EdgeKind, decode decodeRow[*graph.Edge], yield func(*graph.Edge) bool) {
	decodeRows(s, keyStrings([]byte{familyEdge, edgeKind}, string(kind)), func(key []byte) (*graph.Edge, error) {
		e, err := point(s, key, decode)
		if err == nil && e == nil {
			err = invalid("missing indexed edge")
		}
		return e, err
	}, yield)
}

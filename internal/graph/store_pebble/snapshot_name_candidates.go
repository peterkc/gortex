package store_pebble

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

var (
	_ graph.BoundedExactNameReader       = (*Snapshot)(nil)
	_ graph.BoundedFileNodeReader        = (*Snapshot)(nil)
	_ graph.FilteredContainingNameReader = (*Snapshot)(nil)
	_ graph.FoldedNameCandidateReader    = (*Snapshot)(nil)
)

func (s *Snapshot) scanIndexedNodes(ctx context.Context, prefix []byte, decode decodeRow[*graph.Node], yield func(*graph.Node) (bool, error)) error {
	return scan(ctx, s, prefix, func(_, raw []byte) (bool, error) {
		n, err := point(s, keyStrings([]byte{familyNode}, string(raw)), decode)
		if err != nil {
			return false, err
		}
		if n == nil {
			return false, invalid("missing indexed node")
		}
		return yield(n)
	})
}

func asciiNameFold(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, s)
}

func nameCandidateHeader(n *graph.Node) *graph.Node {
	return &graph.Node{ID: n.ID, Kind: n.Kind, Name: n.Name, FilePath: n.FilePath, RepoPrefix: n.RepoPrefix, WorkspaceID: n.WorkspaceID, ProjectID: n.ProjectID}
}

func (s *Snapshot) nameCandidates(ctx context.Context, needle string, yield func(*graph.Node) (bool, error)) error {
	return s.scanNameGramNodes(ctx, strings.ToLower(needle), decodeNodeLight, yield)
}

// Intersect the distinct longest byte grams in ID order. All shorter grams
// are implied by these; the caller still checks the full name predicate.
func (s *Snapshot) scanNameGramNodes(ctx context.Context, folded string, decode decodeRow[*graph.Node], yield func(*graph.Node) (bool, error)) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if folded == "" {
		return nil
	}
	width := min(3, len(folded))
	var prefixes [][]byte
	seen := make(map[string]bool)
	for i := 0; i+width <= len(folded); i++ {
		gram := folded[i : i+width]
		if !seen[gram] {
			seen[gram] = true
			prefixes = append(prefixes, keyStrings([]byte{familyName, nameGram}, gram))
		}
	}
	if len(prefixes) == 1 {
		return s.scanIndexedNodes(ctx, prefixes[0], decode, yield)
	}
	cursors := make([]*pebble.Iterator, 0, len(prefixes))
	for _, prefix := range prefixes {
		i, openErr := s.snap.NewIterWithContext(ctx, &pebble.IterOptions{LowerBound: prefix, UpperBound: prefixEnd(prefix)})
		if openErr != nil {
			return openErr
		}
		defer func() { err = errors.Join(err, i.Close()) }()
		if !i.First() {
			return errors.Join(ctx.Err(), i.Error())
		}
		cursors = append(cursors, i)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Each key is the gram prefix followed by the same encoded node ID.
		// Seek lagging postings to the largest head, then repeat until equal.
		largest := 0
		for j := 1; j < len(cursors); j++ {
			if bytes.Compare(cursors[j].Key()[len(prefixes[j]):], cursors[largest].Key()[len(prefixes[largest]):]) > 0 {
				largest = j
			}
		}
		idKey := cursors[largest].Key()[len(prefixes[largest]):]
		aligned := true
		for j, i := range cursors {
			if bytes.Equal(i.Key()[len(prefixes[j]):], idKey) {
				continue
			}
			aligned = false
			if !i.SeekGE(append(bytes.Clone(prefixes[j]), idKey...)) {
				return errors.Join(ctx.Err(), i.Error())
			}
		}
		if !aligned {
			continue
		}
		id, tail, err := readKeyString(idKey)
		if err != nil || len(tail) != 0 {
			return invalid("name gram node key")
		}
		n, err := point(s, keyStrings([]byte{familyNode}, id), decode)
		if err != nil {
			return err
		}
		if n == nil {
			return invalid("missing indexed node")
		}
		more, err := yield(n)
		if err != nil || !more {
			return err
		}
		// Advancing just one cursor is enough; the others will seek forward.
		if !cursors[0].Next() {
			return errors.Join(ctx.Err(), cursors[0].Error())
		}
	}
}

func (s *Snapshot) FindNodesByNameContainingFilteredContext(ctx context.Context, needle string, limit int, filter graph.NameSearchFilter) ([]*graph.Node, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if needle == "" {
		return nil, nil
	}
	// SQLite's LIKE folds ASCII only. The broader Unicode gram index is a
	// candidate superset; the final predicate preserves the narrower match.
	needle, _, _ = strings.Cut(needle, "\x00")
	var out []*graph.Node
	accept := func(n *graph.Node) (bool, error) {
		head, _, _ := strings.Cut(n.Name, "\x00")
		if n.Name == "" || !strings.Contains(asciiNameFold(head), asciiNameFold(needle)) || !filter.Allows(nameCandidateHeader(n)) {
			return true, nil
		}
		full, err := s.GetNodeContext(ctx, n.ID)
		if err != nil {
			return false, err
		}
		out = append(out, full)
		return limit <= 0 || len(out) < limit, nil
	}
	var err error
	if needle == "" {
		err = scan(ctx, s, []byte{familyNode}, func(_, raw []byte) (bool, error) {
			n, e := decodeNodeLight(raw)
			if e != nil {
				return false, e
			}
			return accept(n)
		})
	} else {
		err = s.nameCandidates(ctx, needle, accept)
	}
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Snapshot) FindFoldedNameCandidatesContext(ctx context.Context, needle string, limit int, filter graph.NameSearchFilter) ([]*graph.Node, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if needle == "" || limit <= 0 {
		return nil, false, fmt.Errorf("store_pebble: invalid folded-name candidate head")
	}
	ranked := graph.NewFoldedNameCandidateSet(needle, limit)
	err := s.nameCandidates(ctx, needle, func(n *graph.Node) (bool, error) {
		if filter.Allows(nameCandidateHeader(n)) {
			ranked.Add(graph.NodeSearchKey{ID: n.ID, Kind: n.Kind, Name: n.Name, FilePath: n.FilePath, RepoPrefix: n.RepoPrefix, WorkspaceID: n.WorkspaceID, ProjectID: n.ProjectID})
		}
		return true, nil
	})
	if err != nil {
		return nil, false, err
	}
	keys := ranked.Keys()
	out := make([]*graph.Node, 0, len(keys))
	for _, key := range keys {
		n, err := s.GetNodeContext(ctx, key.ID)
		if err != nil {
			return nil, false, err
		}
		if n == nil {
			return nil, false, invalid("missing ranked node")
		}
		out = append(out, n)
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	return out, ranked.More(), nil
}

func (s *Snapshot) FindNodesByNameBounded(ctx context.Context, name string, scope graph.LocalizationNodeScope, limit int) (graph.BoundedNodeProjection, error) {
	if name == "" || limit <= 0 {
		return graph.BoundedNodeProjection{}, nil
	}
	return s.localizedNodes(ctx, [][]byte{keyStrings([]byte{familyName, nameExact}, name)}, scope, limit)
}

func (s *Snapshot) FindFileNodesBounded(ctx context.Context, path string, scope graph.LocalizationNodeScope, limit int) (graph.BoundedNodeProjection, error) {
	if path == "" || limit <= 0 {
		return graph.BoundedNodeProjection{}, nil
	}
	if ctx != nil && ctx.Err() != nil {
		return graph.BoundedNodeProjection{}, ctx.Err()
	}
	if scope.ExcludesFile(path) {
		return graph.BoundedNodeProjection{}, nil
	}
	var prefixes [][]byte
	for _, repo := range s.repos() {
		if len(scope.RepoAllow) == 0 || repo == "" || scope.RepoAllow[repo] {
			prefixes = append(prefixes, keyStrings([]byte{familyFile}, repo, path))
		}
	}
	if err := s.ReadError(); err != nil {
		return graph.BoundedNodeProjection{}, err
	}
	return s.localizedNodes(ctx, prefixes, scope, limit)
}

func (s *Snapshot) localizedNodes(ctx context.Context, prefixes [][]byte, scope graph.LocalizationNodeScope, limit int) (graph.BoundedNodeProjection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var out graph.BoundedNodeProjection
	decode := decodeNodeSummary
	if scope.ExcludeTests {
		decode = func(raw []byte) (*graph.Node, error) {
			full, err := decodeNode(raw)
			if err != nil {
				return nil, err
			}
			n := nodeSummary(full)
			if isTest, _ := full.Meta["is_test"].(bool); isTest {
				n.Meta = map[string]any{"is_test": true}
			}
			return n, nil
		}
	}
	inspected := 0
	// The SQL predicate applies structural scope before its inspection counter.
	structural := graph.LocalizationNodeScope{WorkspaceID: scope.WorkspaceID, ProjectID: scope.ProjectID, RepoAllow: scope.RepoAllow, Kinds: scope.Kinds, ExcludeKinds: scope.ExcludeKinds}
	for _, prefix := range prefixes {
		seen := 0
		err := s.scanIndexedNodes(ctx, prefix, decode, func(n *graph.Node) (bool, error) {
			if !structural.Allows(n) {
				return true, nil
			}
			inspected++
			if err := scope.CheckInspection(inspected); err != nil {
				return false, err
			}
			if !scope.Allows(n) {
				return true, nil
			}
			seen++
			i, _ := slices.BinarySearchFunc(out.Nodes, n, func(a, b *graph.Node) int { return cmp.Compare(a.ID, b.ID) })
			if len(out.Nodes) < limit {
				out.Nodes = append(out.Nodes, nil)
			} else {
				out.Truncated = true
				if i >= limit {
					return seen <= limit, nil
				}
			}
			copy(out.Nodes[i+1:], out.Nodes[i:])
			out.Nodes[i] = n
			return seen <= limit, nil
		})
		if err != nil {
			return graph.BoundedNodeProjection{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return graph.BoundedNodeProjection{}, err
	}
	if out.Nodes == nil {
		out.Nodes = []*graph.Node{}
	}
	out.Nodes = out.Nodes[:len(out.Nodes):len(out.Nodes)]
	out.Total = len(out.Nodes)
	if out.Truncated {
		out.Total++
	}
	return out, nil
}

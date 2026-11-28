package store_pebble

import (
	"bytes"
	"context"

	"github.com/zzet/gortex/internal/graph"
)

// goldSeal validates the selected key stream, never the destination DB. Its
// secondary references use their own bounded sort and a sequential join.
type goldSeal struct {
	refs sortedRuns
	// Resolve owners from private input keys, never from the serving DB.
	owner                              func(string) (sourceOwner, error)
	edgeOwner                          func(*graph.Edge) (sourceOwner, error)
	nodes, edges, nodeCount, edgeCount int
}

func (s *goldSeal) relation(key, value []byte, actual bool) error {
	// Key types include their complete secondary key; the final byte sorts
	// expected immediately before actual. Equal-count mismatches still fail.
	ref := appendKeyString(nil, string(key))
	return s.refs.add(appendBool(ref, actual), bytes.Clone(value))
}

func (s *goldSeal) visit(r row) error {
	if len(r.key) == 0 {
		return invalid("empty record key")
	}
	switch r.key[0] {
	case familyNode:
		n, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		owner, err := s.owner(n.ID)
		if err != nil {
			return err
		}
		if n.ID == "" || !owner.present || owner.repo != n.RepoPrefix || !bytes.Equal(r.key, keyStrings([]byte{familyNode}, n.ID)) {
			return invalid("node ID and owner projection do not match")
		}
		s.nodes++
		for _, key := range nodeIndexKeys(n) {
			if err := s.relation(key, []byte(n.ID), false); err != nil {
				return err
			}
		}
	case familyEdge:
		if len(r.key) < 2 {
			return invalid("missing edge index tag")
		}
		if r.key[1] != edgeOut {
			return s.relation(r.key, r.value, true)
		}
		e, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		id := edgeIdentity(e)
		if e.From == "" || e.To == "" || e.Kind == "" || !bytes.Equal(r.key, outEdgeKey(e, id)) {
			return invalid("outgoing edge identity mismatch")
		}
		s.edges++
		owner, err := s.edgeOwner(e)
		if err != nil {
			return err
		}
		for _, pair := range edgeIndexRows(e, id, owner, r.key, r.value) {
			if err := s.relation(pair.key, pair.value, false); err != nil {
				return err
			}
		}
	case familyFile, familyName, familyRepo, familyNodeKind:
		return s.relation(r.key, r.value, true)
	case familyIsolation:
		_, err := decodeIsolationScope(r.key, r.value)
		return err
	case familyCount:
		count, err := decodeCount(r.value)
		if err != nil {
			return err
		}
		switch {
		case bytes.Equal(r.key, countKey("nodes")):
			s.nodeCount = count
		case bytes.Equal(r.key, countKey("edges")):
			s.edgeCount = count
		}
	case familySymbol:
		return checkSymbolRow(r)
	case familyFileMeta:
		if len(r.key) > 1 && r.key[1] == fileMetaRows {
			return checkFileMetaRow(r)
		}
		return checkShingleRow(r)
	default:
		return invalid("unexpected graph family at seal")
	}
	return nil
}

func (s *goldSeal) finish(ctx context.Context) error {
	if s.nodes != s.nodeCount || s.edges != s.edgeCount {
		return invalid("stored counts do not match sealed graph")
	}
	var expected []byte
	var value []byte
	if err := s.refs.merge(ctx, func(r row) error {
		key, tail, err := readKeyString(r.key)
		if err != nil || len(tail) != 1 {
			return invalid("malformed seal relationship")
		}
		if tail[0] == 0 {
			if expected != nil {
				return invalid("missing secondary graph record")
			}
			expected = []byte(key)
			value = append(value[:0], r.value...)
			return nil
		}
		if !bytes.Equal(expected, []byte(key)) || !bytes.Equal(value, r.value) {
			return invalid("unexpected or mismatched secondary graph record")
		}
		expected = nil
		return nil
	}); err != nil {
		return err
	}
	if expected != nil {
		return invalid("missing final secondary graph record")
	}
	return nil
}

package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
	"math"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

var ErrRebuildCSharpPartial = errors.New("pebble rebuild C# partial merge failed")

var rebuildCSharpPartialPrefix = []byte{'g', 'q', 'U'}

// RebuildCSharpPartialRules supplies the native type grouping and payloads.
type RebuildCSharpPartialRules struct {
	Group    func(*graph.Node) (repo, ns, name string)
	Retarget func(*graph.Edge, string, string) (graph.EdgeReindex, bool)
	CopyFrom func(*graph.Edge, string) *graph.Edge
	Stamp    func(*graph.Node, string) bool
}

type rebuildCSharpPartial struct {
	b     *BuildStore
	rules RebuildCSharpPartialRules
	moves uint64
}

// RebuildCSharpPartial settles the final file-attribution turn. The scratch
// graph contains only edges incident to an active group. Plan and prove both
// writes before changing any saved node or edge.
func (b *BuildStore) RebuildCSharpPartial(ctx context.Context, receipts iter.Seq2[ParseFacts, error], rules RebuildCSharpPartialRules) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("%w: %w", ErrRebuildCSharpPartial, err)
		}
	}()
	if rules.Group == nil || rules.Retarget == nil || rules.CopyFrom == nil || rules.Stamp == nil {
		return invalid("C# partial merge inputs")
	}
	prefix := rebuildCSharpPartialPrefix
	busy, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return err
	}
	if busy {
		return invalid("C# partial scratch range occupied")
	}
	defer func() { err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync)) }()
	p := rebuildCSharpPartial{b: b, rules: rules}
	active, err := p.groups(ctx)
	if err != nil || !active {
		return err
	}
	if receipts == nil {
		return &RebuildUnknownProvenanceError{Count: 1}
	}
	if err := p.capture(ctx); err != nil {
		return err
	}
	if err := p.record(ctx, receipts); err != nil {
		return err
	}
	if err := p.seed(ctx); err != nil {
		return err
	}
	if err := p.fold(ctx); err != nil {
		return err
	}
	if err := p.reindex(ctx); err != nil {
		return err
	}
	return b.withWrites(func() error { return p.apply(ctx) })
}

func (*rebuildCSharpPartial) key(col byte, fields ...string) []byte {
	return keyStrings(append(bytes.Clone(rebuildCSharpPartialPrefix), col), fields...)
}

func (p *rebuildCSharpPartial) get(key []byte) ([]byte, error) {
	return pendingGet(p.b.pending, key, copyValue)
}

func (p *rebuildCSharpPartial) put(key, raw []byte) error {
	return p.b.putRebuildScratch(key, raw, 16<<20, "C# partial")
}

func (p *rebuildCSharpPartial) visit(ctx context.Context, key []byte, visit func(row) error) error {
	return pendingVisit(ctx, p.b.pending, key, visit)
}

// c holds a group's lowest ID. x marks groups with more than one node, and
// n maps every candidate ID to its group. Node keys already sort by ID.
func (p *rebuildCSharpPartial) groups(ctx context.Context) (bool, error) {
	active := false
	err := p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		n, err := checkedRebuildLightNode(r, "C# partial")
		if err != nil || n.Kind != graph.KindType || n.Language != "csharp" {
			return err
		}
		n, err = decodeNode(r.value)
		if err != nil {
			return err
		}
		repo, ns, name := p.rules.Group(n)
		if name == "" {
			return nil
		}
		group := keyStrings(nil, repo, ns, name)
		if err := p.put(p.key('n', n.ID), group); err != nil {
			return err
		}
		key := append(p.key('c'), group...)
		first, err := p.get(key)
		if err != nil {
			return err
		}
		if first == nil {
			return p.put(key, []byte(n.ID))
		}
		active = true
		return p.put(append(p.key('x'), group...), first)
	})
	return active, err
}

func (p *rebuildCSharpPartial) group(id string) (group, canonical []byte, err error) {
	group, err = p.get(p.key('n', id))
	if err != nil || group == nil {
		return group, nil, err
	}
	canonical, err = p.get(append(p.key('x'), group...))
	return group, canonical, err
}

// h is the immutable roster of affected live edges. r requests their raw
// keys; an inherited recorder is always used before considering a live key.
func (p *rebuildCSharpPartial) capture(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		e, err := decodeEdgeLight(r.value)
		if err != nil {
			return err
		}
		_, group, err := p.group(e.From)
		if err == nil && group == nil {
			_, group, err = p.group(e.To)
		}
		if err != nil || group == nil {
			return err
		}
		if !bytes.Equal(r.key, pendingEdgeKey(e)) {
			return invalid("C# partial incident edge identity")
		}
		raw, err := rebuildReceiptKey(snap, r.key)
		if err != nil && !errors.Is(err, ErrRebuildUnknownProvenance) {
			return err
		}
		if err == nil {
			if err := p.put(append(p.key('r'), raw...), []byte{0}); err != nil {
				return err
			}
		}
		return p.put(append(p.key('h'), r.key...), raw)
	})
}

func (p *rebuildCSharpPartial) record(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	var nodeOrdinal, edgeOrdinal uint64
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if facts.EffectiveLanguage == "csharp" && n.FilePath == file {
				if err := p.recordNode(n, nodeOrdinal); err != nil {
					return err
				}
			}
			nodeOrdinal++
		}
		for _, e := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			if facts.EffectiveLanguage != "" && e.FilePath == file {
				key := append(p.key('r'), pendingEdgeKey(e)...)
				order, err := p.get(key)
				if err != nil {
					return err
				}
				if len(order) == 1 {
					if err := p.put(key, binary.BigEndian.AppendUint64(nil, edgeOrdinal)); err != nil {
						return err
					}
				}
			}
			edgeOrdinal++
		}
	}
	return p.visit(ctx, p.key('n'), func(r row) error {
		id, _, err := readKeyString(r.key[len(p.key('n')):])
		if err != nil {
			return err
		}
		canonical, err := p.get(append(p.key('x'), r.value...))
		if err != nil || canonical == nil {
			return err
		}
		proved, err := p.get(p.key('p', id))
		if err != nil || proved != nil {
			return err
		}
		return &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentity{From: id, To: string(canonical)}}
	})
}

func (p *rebuildCSharpPartial) recordNode(n *graph.Node, ordinal uint64) error {
	repo, ns, name := p.rules.Group(n)
	if name == "" {
		return nil
	}
	group, canonical, err := p.group(n.ID)
	if err != nil || canonical == nil || !bytes.Equal(group, keyStrings(nil, repo, ns, name)) {
		return err
	}
	live, err := p.b.pendingNode(n.ID)
	if err != nil || live == nil || live.FilePath != n.FilePath {
		return err
	}
	proof := p.key('p', n.ID)
	seen, err := p.get(proof)
	if err != nil || seen != nil {
		return err
	}
	if err := p.put(proof, []byte{1}); err != nil {
		return err
	}
	order := append(p.key('f'), group...)
	first, err := p.get(order)
	if err != nil {
		return err
	}
	if first == nil {
		if err := p.put(order, []byte{1}); err != nil {
			return err
		}
		if err := p.put(binary.BigEndian.AppendUint64(p.key('g'), ordinal), appendKeyString(group, string(canonical))); err != nil {
			return err
		}
	}
	if n.ID == string(canonical) {
		return nil
	}
	// Native keeps the fragment order of SQLite's C# type scan, not
	// receipt path order, when it folds duplicate outgoing relations.
	return p.put(appendKeyString(order, n.ID), []byte(n.ID))
}

// The incident edge overlay has primary, outgoing and incoming indexes. The
// latter two use frozen receipt order, even for a previously retargeted row.
// The value carries the raw key in the existing custody representation.
type rebuildPartialEdge struct {
	edge  *graph.Edge
	raw   []byte
	order uint64
}

func (p *rebuildCSharpPartial) seed(ctx context.Context) error {
	prefix := p.key('h')
	return p.visit(ctx, prefix, func(r row) error {
		e, err := p.b.pendingEdge(r.key[len(prefix):])
		if err != nil || e == nil {
			return errors.Join(err, invalid("C# partial incident edge missing"))
		}
		item := rebuildPartialEdge{edge: e, raw: r.value, order: math.MaxUint64}
		if len(item.raw) != 0 {
			order, err := p.get(append(p.key('r'), item.raw...))
			if err != nil {
				return err
			}
			if len(order) == 8 {
				item.order = binary.BigEndian.Uint64(order)
			}
		}
		return p.putEdge(item)
	})
}

func (s rebuildPartialEdge) encode() ([]byte, error) {
	raw, err := encodeEdge(s.edge)
	if err != nil {
		return nil, err
	}
	return append(appendKeyString(binary.BigEndian.AppendUint64(nil, s.order), string(s.raw)), raw...), nil
}

func decodeRebuildPartialEdge(v []byte) (s rebuildPartialEdge, err error) {
	if len(v) < 8 {
		return s, invalid("C# partial scratch edge")
	}
	s.order = binary.BigEndian.Uint64(v)
	raw, val, err := readKeyString(v[8:])
	if err != nil {
		return s, err
	}
	s.raw = []byte(raw)
	s.edge, err = decodeEdge(val)
	return s, err
}

func (p *rebuildCSharpPartial) edge(key []byte) (rebuildPartialEdge, error) {
	return pendingGet(p.b.pending, append(p.key('e'), key...), decodeRebuildPartialEdge)
}

func (p *rebuildCSharpPartial) posting(col byte, id string, item rebuildPartialEdge, key []byte) []byte {
	return append(binary.BigEndian.AppendUint64(p.key(col, id), item.order), key...)
}

func (p *rebuildCSharpPartial) putEdge(item rebuildPartialEdge) error {
	key := pendingEdgeKey(item.edge)
	val, err := item.encode()
	if err != nil {
		return err
	}
	if err := p.put(append(p.key('e'), key...), val); err != nil {
		return err
	}
	if err := p.put(p.posting('o', item.edge.From, item, key), nil); err != nil {
		return err
	}
	return p.put(p.posting('i', item.edge.To, item, key), nil)
}

func (p *rebuildCSharpPartial) remove(item rebuildPartialEdge) error {
	key := pendingEdgeKey(item.edge)
	for _, scratch := range [...][]byte{append(p.key('e'), key...), p.posting('o', item.edge.From, item, key), p.posting('i', item.edge.To, item, key)} {
		if err := p.b.pending.Delete(scratch, pebble.NoSync); err != nil {
			return err
		}
	}
	return p.put(append(p.key('d'), key...), nil)
}

func (p *rebuildCSharpPartial) incident(ctx context.Context, col byte, id string, visit func(rebuildPartialEdge) error) error {
	prefix := p.key(col, id)
	return p.visit(ctx, prefix, func(r row) error {
		item, err := p.edge(r.key[len(prefix)+8:])
		if err != nil || item.edge == nil {
			return err
		}
		return visit(item)
	})
}

func (p *rebuildCSharpPartial) insert(item rebuildPartialEdge) error {
	key := pendingEdgeKey(item.edge)
	prior, err := p.edge(key)
	if err != nil || prior.edge != nil {
		return err
	}
	if item.order == math.MaxUint64 {
		return &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(item.edge)}
	}
	if err := p.putEdge(item); err != nil {
		return err
	}
	return p.put(append(p.key('d'), key...), nil)
}

// Native captures the entire group's incoming side before rewriting its
// outgoing rows. It defers the actual incoming moves until every group ends.
func (p *rebuildCSharpPartial) fold(ctx context.Context) error {
	return p.visit(ctx, p.key('g'), func(r row) error {
		fields := r.value
		for range 3 {
			var err error
			_, fields, err = readKeyString(fields)
			if err != nil {
				return err
			}
		}
		canonical, _, err := readKeyString(fields)
		if err != nil {
			return err
		}
		fragments := append(p.key('f'), r.value[:len(r.value)-len(fields)]...)
		pair := func(e *graph.Edge) []byte { return p.key('u', canonical, string(e.Kind), e.To) }
		if err := p.incident(ctx, 'o', canonical, func(item rebuildPartialEdge) error {
			return p.put(pair(item.edge), []byte{1})
		}); err != nil {
			return err
		}
		visitFragments := func(visit func(string) error) error {
			return p.visit(ctx, fragments, func(r row) error {
				if len(r.key) == len(fragments) { // group-order marker
					return nil
				}
				return visit(string(r.value))
			})
		}
		if err := visitFragments(func(id string) error {
			return p.incident(ctx, 'i', id, func(item rebuildPartialEdge) error {
				old := pendingEdgeKey(item.edge)
				if _, changed := p.rules.Retarget(item.edge, id, canonical); !changed {
					return nil
				}
				raw, err := item.encode()
				if err != nil {
					return err
				}
				key := binary.BigEndian.AppendUint64(p.key('m'), p.moves)
				p.moves++
				return p.put(key, append(appendKeyString(nil, string(old)), raw...))
			})
		}); err != nil {
			return err
		}
		if err := visitFragments(func(id string) error {
			if err := p.incident(ctx, 'o', id, func(item rebuildPartialEdge) error {
				key := pair(item.edge)
				seen, err := p.get(key)
				if err != nil {
					return err
				}
				if item.order == math.MaxUint64 {
					return &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(item.edge)}
				}
				if err := p.remove(item); err != nil || seen != nil {
					return err
				}
				item.edge = p.rules.CopyFrom(item.edge, canonical)
				if !graph.StructuralEdgeTargetInvalid(item.edge.Kind, item.edge.To) {
					if err := p.insert(item); err != nil {
						return err
					}
				}
				return p.put(key, []byte{1})
			}); err != nil {
				return err
			}
			return p.stamp(id, canonical)
		}); err != nil {
			return err
		}
		return p.stamp(canonical, canonical)
	})
}

func (p *rebuildCSharpPartial) stamp(id, canonical string) error {
	n, err := p.b.pendingNode(id)
	if err != nil || n == nil {
		return errors.Join(err, invalid("C# partial node missing"))
	}
	if !p.rules.Stamp(n, canonical) {
		return nil
	}
	raw, err := encodeNode(n)
	if err != nil {
		return err
	}
	return p.put(p.key('w', id), raw)
}

// Match SQLite's windows, including a move whose old key the outgoing fold
// already removed. SQLite still offers that move's captured payload.
func (p *rebuildCSharpPartial) reindex(ctx context.Context) error {
	move := func(index uint64) (string, rebuildPartialEdge, error) {
		val, err := p.get(binary.BigEndian.AppendUint64(p.key('m'), index))
		if err != nil {
			return "", rebuildPartialEdge{}, err
		}
		old, raw, err := readKeyString(val)
		if err != nil {
			return "", rebuildPartialEdge{}, err
		}
		item, err := decodeRebuildPartialEdge(raw)
		return old, item, err
	}
	for start := uint64(0); start < p.moves; start += graph.SQLiteReindexChunkSize {
		stop := min(start+graph.SQLiteReindexChunkSize, p.moves)
		var wasPresent [graph.SQLiteReindexChunkSize]bool
		for i := start; i < stop; i++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			old, _, err := move(i)
			if err != nil {
				return err
			}
			prior, err := p.edge([]byte(old))
			if err != nil {
				return err
			}
			wasPresent[i-start] = prior.edge != nil
		}
		for i := start; i < stop; i++ {
			old, item, err := move(i)
			if err != nil {
				return err
			}
			prior, err := p.edge([]byte(old))
			if err != nil {
				return err
			}
			if prior.edge != nil {
				if item.order == math.MaxUint64 {
					return &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(item.edge)}
				}
				if err := p.remove(prior); err != nil {
					return err
				}
			}
		}
		for i := start; i < stop; i++ {
			old, item, err := move(i)
			if err != nil {
				return err
			}
			if !wasPresent[i-start] {
				prior, err := p.edge([]byte(old))
				if err != nil {
					return err
				}
				if prior.edge != nil {
					if err := p.remove(prior); err != nil {
						return err
					}
				}
			}
			if !graph.StructuralEdgeTargetInvalid(item.edge.Kind, item.edge.To) {
				if err := p.insert(item); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (p *rebuildCSharpPartial) apply(ctx context.Context) error {
	prefix := p.key('d')
	if err := p.visit(ctx, prefix, func(r row) error {
		key := r.key[len(prefix):]
		prior, err := p.b.pendingEdge(key)
		if err != nil || prior == nil {
			return err
		}
		return p.b.deleteEdge(key, prior)
	}); err != nil {
		return err
	}
	if err := p.visit(ctx, prefix, func(r row) error {
		item, err := p.edge(r.key[len(prefix):])
		if err != nil || item.edge == nil {
			return err
		}
		return p.b.putEdgeWithCustody(item.edge, rebuildEdgeCustody{raw: item.raw})
	}); err != nil {
		return err
	}
	return p.visit(ctx, p.key('w'), func(r row) error {
		n, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		return p.b.writeNode(n)
	})
}

// csharpPartialReceiptSource preserves the invariant-site checks in Implements
// and Overrides for ordinary rows. A partial fold is the checked exception:
// use the raw source only when its live fragment stamp names this canonical.
func (b *BuildStore) csharpPartialReceiptSource(e *graph.Edge) (string, error) {
	canonical, err := pendingGet(b.pending, pendingNodeKey(e.From), decodeNodeLight)
	if err != nil || canonical == nil || canonical.Language != "csharp" || canonical.Kind != graph.KindType {
		return e.From, err
	}
	custody, err := readRebuildEdgeCustody(b.pending, pendingEdgeKey(e))
	if err != nil || custody.raw == nil {
		return e.From, err
	}
	raw, err := decodeLinkRecorder(custody.raw)
	if err != nil || raw.From == e.From || raw.Kind != e.Kind || raw.FilePath != e.FilePath || raw.Line != e.Line {
		return e.From, err
	}
	fragment, err := b.pendingNode(raw.From)
	if err != nil || fragment == nil || fragment.Meta["partial_merged_into"] != e.From {
		return e.From, err
	}
	return raw.From, nil
}

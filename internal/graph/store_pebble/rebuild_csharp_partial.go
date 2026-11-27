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

const rebuildPartialRowLimit = 16 << 20

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
	if err != nil {
		return err
	}
	if active {
		if err := p.capture(ctx); err != nil {
			return err
		}
	}
	// A receipt can restore the C# role of a known ID even when no live
	// node is a C# type. Check that cohort before treating the fold as inert.
	if err := p.record(ctx, receipts); err != nil {
		return err
	}
	if !active {
		return nil
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
	if err := p.proveEdges(ctx); err != nil {
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
	return p.b.putRebuildScratch(key, raw, rebuildPartialRowLimit, "C# partial")
}

func (p *rebuildCSharpPartial) visit(ctx context.Context, key []byte, visit func(row) error) error {
	return pendingVisit(ctx, p.b.pending, key, visit)
}

// c holds a group's lowest ID. x marks groups with more than one node, and
// n maps every candidate ID to its group. Node keys already sort by ID.
func (p *rebuildCSharpPartial) groups(ctx context.Context) (active bool, err error) {
	err = p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		n, err := checkedRebuildLightNode(r, "C# partial")
		if err != nil || n.Kind != graph.KindType || n.Language != "csharp" || n.Name == "" || n.FilePath == "" {
			return err
		}
		if len(r.key)+len(r.value) > rebuildPartialRowLimit {
			return fmt.Errorf("%w: csharp partial type node", ErrRebuildLinkInputSize)
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

// h is the immutable roster of affected live edges. Register claims before
// choosing a move or a stationary incumbent; two edges cannot prove one raw
// receipt. An edge incident to both sides still has just one live identity.
func (p *rebuildCSharpPartial) capture(ctx context.Context) error {
	return p.b.RebuildView().read(ctx, func(snap *pebble.Snapshot) error {
		members := p.key('n')
		return p.visit(ctx, members, func(member row) error {
			id, tail, err := readKeyString(member.key[len(members):])
			if err != nil || len(tail) != 0 {
				return errors.Join(err, invalid("csharp partial member key"))
			}
			canonical, err := p.get(append(p.key('x'), member.value...))
			if err != nil || canonical == nil {
				return err
			}
			out := keyStrings([]byte{familyEdge, edgeOut}, id)
			if err := pendingVisit(ctx, snap, out, func(r row) error {
				return p.captureEdge(snap, r)
			}); err != nil {
				return err
			}
			in := keyStrings([]byte{familyEdge, edgeIn}, id)
			return pendingVisit(ctx, snap, in, func(r row) error {
				if !bytes.Equal(r.key[len(in):], r.value) || !bytes.HasPrefix(r.value, []byte{familyEdge, edgeOut}) {
					return invalid("csharp partial reverse edge identity")
				}
				found, err := pendingGet(snap, r.value, func(raw []byte) (bool, error) {
					return true, p.captureEdge(snap, row{key: r.value, value: raw})
				})
				if err == nil && !found {
					return invalid("csharp partial reverse edge missing")
				}
				return err
			})
		})
	})
}

func (p *rebuildCSharpPartial) captureEdge(snap *pebble.Snapshot, r row) error {
	if len(r.key)+len(r.value) > rebuildPartialRowLimit {
		return fmt.Errorf("%w: csharp partial incident edge", ErrRebuildLinkInputSize)
	}
	h := append(p.key('h'), r.key...)
	seen, err := pendingGet(p.b.pending, h, fnPointerPresent)
	if err != nil || seen {
		return err
	}
	e, err := decodeEdgeLight(r.value)
	if err != nil {
		return err
	}
	if !bytes.Equal(r.key, pendingEdgeKey(e)) {
		return invalid("csharp partial incident edge identity")
	}
	raw, err := rebuildReceiptKey(snap, r.key)
	if err != nil && !errors.Is(err, ErrRebuildUnknownProvenance) {
		return err
	}
	if err == nil {
		key := append(p.key('r'), raw...)
		prior, err := p.get(key)
		if err != nil {
			return err
		}
		var state byte
		if prior != nil {
			state = 1 // ambiguous claim; do not attach a receipt ordinal
		}
		if err := p.put(key, []byte{state}); err != nil {
			return err
		}
	}
	return p.put(h, raw)
}

func (p *rebuildCSharpPartial) record(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	if receipts != nil {
		if err := p.recordReceipts(ctx, receipts); err != nil {
			return err
		}
	}
	// The receipt-side cohort includes a known ID even if it was not a type
	// or had a different namespace in the live graph. Its index sorts by ID.
	if err := p.visit(ctx, p.key('N'), func(r row) error {
		if len(r.value) <= 8 {
			return nil // last admitted receipt cleared its previous role
		}
		group := r.value[8:]
		key := append(p.key('C'), group...)
		first, err := p.get(key)
		if err != nil {
			return err
		}
		if first != nil {
			return p.put(append(p.key('X'), group...), first)
		}
		id, tail, err := readKeyString(r.key[len(p.key('N')):])
		if err != nil || len(tail) != 0 {
			return errors.Join(err, invalid("csharp partial receipt member key"))
		}
		return p.put(key, []byte(id))
	}); err != nil {
		return err
	}
	return p.planGroups(ctx)
}

func (p *rebuildCSharpPartial) recordReceipts(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	var nodeOrdinal, edgeOrdinal uint64
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		if facts.EffectiveLanguage == "" {
			continue
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if n != nil && n.FilePath == file {
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
			if e != nil && e.FilePath == file {
				key := append(p.key('r'), pendingEdgeKey(e)...)
				order, err := p.get(key)
				if err != nil {
					return err
				}
				if len(order) == 1 && order[0] == 0 {
					if err := p.put(key, binary.BigEndian.AppendUint64(nil, edgeOrdinal)); err != nil {
						return err
					}
				}
			}
			edgeOrdinal++
		}
	}
	return ctx.Err()
}

func (p *rebuildCSharpPartial) recordNode(n *graph.Node, ordinal uint64) error {
	repo, ns, name := p.rules.Group(n)
	key := p.key('N', n.ID)
	if name == "" {
		seen, err := pendingGet(p.b.pending, key, fnPointerPresent)
		if err != nil || !seen {
			return err
		}
		return p.put(key, nil)
	}
	known, err := pendingGet(p.b.pending, pendingNodeKey(n.ID), fnPointerPresent)
	if err != nil || !known {
		return err
	}
	return p.put(key, keyStrings(binary.BigEndian.AppendUint64(nil, ordinal), repo, ns, name))
}

func (p *rebuildCSharpPartial) planGroups(ctx context.Context) error {
	var refused rebuildScratchRefusals
	check := func(id string, group []byte) error {
		var live, proved []byte
		var err error
		if group != nil {
			live, err = p.get(append(p.key('x'), group...))
			if err != nil {
				return err
			}
		}
		raw, err := p.get(p.key('N', id))
		if err != nil {
			return err
		}
		if len(raw) > 8 {
			proved, err = p.get(append(p.key('X'), raw[8:]...))
			if err != nil {
				return err
			}
		}
		if !bytes.Equal(live, proved) {
			changes, err := p.groupChange(ctx, id, live, proved)
			if err != nil {
				return err
			}
			if changes {
				refused.add(graph.EdgeIdentity{From: id, Kind: "node"}, pendingNodeKey(id))
				return nil
			}
		}
		if live == nil {
			return nil
		}
		// First receipt in a group fixes only its cross-group position.
		// SQLite has no fixed map-order result for linked groups (U5).
		ordinal := uint64(math.MaxUint64)
		if len(raw) > 8 && bytes.Equal(group, raw[8:]) {
			ordinal = binary.BigEndian.Uint64(raw)
		}
		order := append(p.key('f'), group...)
		first, err := p.get(order)
		if err != nil {
			return err
		}
		orderKey := func(o uint64) []byte { return append(binary.BigEndian.AppendUint64(p.key('g'), o), group...) }
		if first == nil || ordinal < binary.BigEndian.Uint64(first) {
			if first != nil {
				if err := p.b.pending.Delete(orderKey(binary.BigEndian.Uint64(first)), pebble.NoSync); err != nil {
					return err
				}
			}
			if err := p.put(order, binary.BigEndian.AppendUint64(nil, ordinal)); err != nil {
				return err
			}
			if err := p.put(orderKey(ordinal), appendKeyString(bytes.Clone(group), string(live))); err != nil {
				return err
			}
		}
		if id == string(live) {
			return nil
		}
		// Native's C# type scan orders fragments by ID within the group.
		return p.put(appendKeyString(order, id), []byte(id))
	}
	idAt := func(r row) (string, error) {
		id, tail, err := readKeyString(r.key[len(p.key('n')):])
		if err == nil && len(tail) != 0 {
			err = invalid("csharp partial member key")
		}
		return id, err
	}
	if err := p.visit(ctx, p.key('n'), func(r row) error {
		id, err := idAt(r)
		if err != nil {
			return err
		}
		return check(id, r.value)
	}); err != nil {
		return err
	}
	if err := p.visit(ctx, p.key('N'), func(r row) error {
		if len(r.value) <= 8 {
			return nil
		}
		id, err := idAt(r)
		if err != nil {
			return err
		}
		group, err := p.get(p.key('n', id))
		if err != nil || group != nil {
			return err
		}
		return check(id, nil)
	}); err != nil {
		return err
	}
	return refused.err()
}

func (p *rebuildCSharpPartial) groupChange(ctx context.Context, id string, canonicals ...[]byte) (bool, error) {
	for _, canonical := range canonicals {
		if canonical == nil {
			continue
		}
		n, err := p.b.pendingNode(id)
		if err != nil || n == nil {
			return false, errors.Join(err, invalid("csharp partial member missing"))
		}
		if p.rules.Stamp(n, string(canonical)) {
			return true, nil
		}
		if id != string(canonical) {
			for _, dir := range [...]byte{edgeOut, edgeIn} {
				found, err := rebuildGuardHasPrefix(ctx, p.b.pending, keyStrings([]byte{familyEdge, dir}, id))
				if err != nil || found {
					return found, err
				}
			}
		}
	}
	return false, nil
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
	if err != nil {
		return err
	}
	if prior.edge != nil {
		return p.proveSuppressor(prior)
	}
	if err := p.putEdge(item); err != nil {
		return err
	}
	return p.put(append(p.key('d'), key...), nil)
}

func (p *rebuildCSharpPartial) proveSuppressor(item rebuildPartialEdge) error {
	if item.order == math.MaxUint64 {
		return &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(item.edge)}
	}
	return nil
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
			key := pair(item.edge)
			if item.order == math.MaxUint64 {
				seen, err := p.get(key)
				if err != nil || seen != nil {
					return err
				}
				return p.put(key, pendingEdgeKey(item.edge))
			}
			return p.put(key, []byte{1})
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
				if err := p.remove(item); err != nil {
					return err
				}
				item.edge = p.rules.CopyFrom(item.edge, canonical)
				if graph.StructuralEdgeTargetInvalid(item.edge.Kind, item.edge.To) {
					return p.put(key, []byte{1})
				}
				if len(seen) != 0 && seen[0] == 1 {
					// A canonical incumbent or an earlier fragment wins by
					// role/order. Only siblings in this fragment need an ordinal.
					if item.order == math.MaxUint64 && string(seen[1:]) == id {
						return &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(item.edge)}
					}
					return nil
				}
				if len(seen) != 0 {
					id, err := decodeLinkRecorder(seen)
					if err != nil {
						return err
					}
					return &RebuildUnknownProvenanceError{Count: 1, First: id}
				}
				if err := p.insert(item); err != nil {
					return err
				}
				if item.order == math.MaxUint64 {
					return p.put(key, pendingEdgeKey(item.edge))
				}
				return p.put(key, append([]byte{1}, id...))
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

// The native attribution buffer flushes 2,048 moves at a time. A new target
// is canonical, never an old fragment target, so adjacent windows cannot
// change a collision. Preserve the buffered offer order without a window.
func (p *rebuildCSharpPartial) reindex(ctx context.Context) error {
	return p.visit(ctx, p.key('m'), func(r row) error {
		old, raw, err := readKeyString(r.value)
		if err != nil {
			return err
		}
		item, err := decodeRebuildPartialEdge(raw)
		if err != nil {
			return err
		}
		prior, err := p.edge([]byte(old))
		if err != nil {
			return err
		}
		if prior.edge != nil {
			if err := p.remove(prior); err != nil {
				return err
			}
		}
		if graph.StructuralEdgeTargetInvalid(item.edge.Kind, item.edge.To) {
			return nil
		}
		return p.insert(item)
	})
}

func (p *rebuildCSharpPartial) proveEdges(ctx context.Context) error {
	var missing rebuildScratchRefusals
	prefix := p.key('d')
	if err := p.visit(ctx, prefix, func(r row) error {
		key := r.key[len(prefix):]
		item, err := p.edge(key)
		if err == nil && item.edge != nil && item.order == math.MaxUint64 {
			missing.add(graph.EdgeIdentityFor(item.edge), bytes.Clone(key))
		}
		return err
	}); err != nil {
		return err
	}
	return missing.err()
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

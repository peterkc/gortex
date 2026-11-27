package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

// RebuildOverrides is the private cold override pass immediately after the
// checked implementation pass. No graph row is written until proof succeeds.
func (b *BuildStore) RebuildOverrides(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	origin func(string) string, emit func(*graph.Node, string, string) *graph.Edge,
	inferred func(*graph.Node, string) *graph.Edge,
) (err error) {
	const rowLimit = 16 << 20
	const supported, deferred, mismatch = byte(1), byte(2), byte(4)
	prefix := []byte{'g', 'o'}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return err
	}
	if occupied {
		return invalid("override scratch range occupied")
	}
	defer func() { err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync)) }()
	key := func(column byte, parts ...string) []byte { return keyStrings([]byte{'g', 'o', column}, parts...) }
	get := func(k []byte) ([]byte, error) { return pendingGet(b.pending, k, copyValue) }
	put := func(k, v []byte) error {
		if len(k)+len(v) > rowLimit {
			return fmt.Errorf("%w: override scratch row", ErrRebuildLinkInputSize)
		}
		return b.pending.Set(k, v, pebble.NoSync)
	}
	mark := func(k []byte, bit byte) error {
		v, err := get(k)
		if err != nil {
			return err
		}
		if len(v) == 1 {
			if v[0]&bit != 0 {
				return nil
			}
			bit |= v[0]
		}
		return put(k, []byte{bit})
	}
	owner := func(n *graph.Node) bool {
		return n != nil && (n.Kind == graph.KindType || n.Kind == graph.KindInterface)
	}
	parentKind := func(k graph.EdgeKind) bool {
		return k == graph.EdgeExtends || k == graph.EdgeImplements || k == graph.EdgeComposes
	}
	edgeProof := func(e *graph.Edge, from string) []byte {
		return appendKeyLine(key('e', string(e.Kind), from, e.FilePath), e.Line)
	}
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file := pathkey.Normalize(facts.File.FilePath)
		producer := deferred
		switch facts.EffectiveLanguage {
		case "go", "javascript", "typescript":
			producer = supported
		}
		proof := func(path string) byte {
			if path != file {
				return mismatch
			}
			return producer
		}
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if owner(n) || n.Kind == graph.KindMethod {
				if err := mark(key('n', n.ID), proof(n.FilePath)); err != nil {
					return err
				}
			}
		}
		for _, e := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			if e.Kind == graph.EdgeMemberOf || e.Kind == graph.EdgeOverrides || parentKind(e.Kind) {
				if err := mark(edgeProof(e, e.From), proof(e.FilePath)); err != nil {
					return err
				}
			}
		}
	}
	node := func(id string) (*graph.Node, error) {
		return pendingGet(b.pending, pendingNodeKey(id), func(raw []byte) (*graph.Node, error) {
			if len(raw) > rowLimit {
				return nil, fmt.Errorf("%w: override node", ErrRebuildLinkInputSize)
			}
			n, err := decodeNodeLight(raw)
			if err == nil && n.ID != id {
				return nil, invalid("override node identity")
			}
			return n, err
		})
	}
	view := b.RebuildView()
	if err := b.visitRebuildDataflowKind(ctx, graph.EdgeMemberOf, decodeEdgeLight, func(e *graph.Edge) error {
		n, err := node(e.From)
		if err != nil || n == nil || n.Kind != graph.KindMethod {
			return err
		}
		np, err := get(key('n', n.ID))
		if err != nil {
			return err
		}
		ep, err := get(edgeProof(e, e.From))
		if err != nil {
			return err
		}
		state := mismatch
		if bytes.Equal(np, []byte{supported}) && bytes.Equal(ep, []byte{supported}) {
			state = supported
		} else if bytes.Equal(np, []byte{deferred}) {
			state = deferred
		}
		if state == supported {
			if err := put(key('s', e.To, n.Name), []byte(n.ID)); err != nil {
				return err
			}
		}
		k := key('m', e.To, n.Name)
		prior, err := get(k)
		if err != nil {
			return err
		}
		// Complete member key starts with method ID. For the same ID,
		// any independent proved membership is enough for the native set.
		if len(prior) > 0 {
			cmp := bytes.Compare(prior[1:], []byte(n.ID))
			if cmp > 0 || (cmp == 0 && prior[0] == supported) {
				return nil
			}
		}
		return put(k, append([]byte{state}, n.ID...))
	}); err != nil {
		return err
	}
	pairKey := func(e *graph.Edge) []byte { return key('c', e.From+"\x00"+e.To) }
	var refused RebuildUnknownProvenanceError
	var firstRefusal []byte
	refuse := func(e *graph.Edge) {
		k := pendingEdgeKey(e)
		if firstRefusal == nil || bytes.Compare(k, firstRefusal) < 0 {
			refused.First, firstRefusal = graph.EdgeIdentityFor(e), k
		}
		refused.Count++
	}
	if err := view.visit(ctx, []byte{familyEdge, edgeOut}, func(_ *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || !parentKind(kind) {
			return err
		}
		if len(r.key)+len(r.value) > rowLimit {
			return fmt.Errorf("%w: override parent", ErrRebuildLinkInputSize)
		}
		e, err := decodeEdge(r.value)
		if err != nil || !bytes.Equal(r.key, pendingEdgeKey(e)) {
			return errors.Join(err, invalid("override parent identity"))
		}
		if e.From == e.To {
			return nil
		}
		childType, err := node(e.From)
		if err != nil || !owner(childType) {
			return err
		}
		parentType, err := node(e.To)
		if err != nil || !owner(parentType) {
			return err
		}
		cp, err := get(key('n', e.From))
		if err != nil || bytes.Equal(cp, []byte{deferred}) {
			return err
		}
		pp, err := get(key('n', e.To))
		if err != nil || bytes.Equal(pp, []byte{deferred}) {
			return err
		}
		from, err := b.csharpPartialReceiptSource(e)
		if err != nil {
			return err
		}
		ep, err := get(edgeProof(e, from))
		if err != nil {
			return err
		}
		proved := bytes.Equal(ep, []byte{supported})
		if len(ep) == 0 && kind == graph.EdgeImplements {
			// No raw producer at the invariant site. Only the already
			// checked implementation pass can have inserted this shape.
			want, err := encodeEdge(inferred(childType, e.To))
			if err != nil {
				return err
			}
			proved = bytes.Equal(want, r.value)
		}
		proved = proved && bytes.Equal(cp, []byte{supported}) && bytes.Equal(pp, []byte{supported})
		members := key('m', e.From)
		return pendingVisit(ctx, b.pending, members, func(m row) error {
			name, _, err := readKeyString(m.key[len(members):])
			if err != nil {
				return err
			}
			parent, err := get(key('m', e.To, name))
			if err != nil || len(parent) == 0 {
				return err
			}
			if len(m.value) < 2 || len(parent) < 2 {
				return invalid("override member slot shape")
			}
			from, to := string(m.value[1:]), string(parent[1:])
			alternate := false
			for _, slot := range [...]struct {
				state         byte
				owner, method string
			}{{m.value[0], e.From, from}, {parent[0], e.To, to}} {
				if slot.state == supported {
					continue
				}
				other, err := get(key('s', slot.owner, name))
				if err != nil || (slot.state == deferred && len(other) == 0) {
					return err // no potential supported method can substitute here
				}
				alternate = alternate || (len(other) != 0 && string(other) != slot.method)
			}
			if from == to && !alternate {
				return nil
			}
			child, err := node(from)
			if err != nil || child == nil {
				return errors.Join(err, invalid("override method disappeared"))
			}
			candidate := emit(child, to, origin(e.Origin))
			if alternate {
				// The retained winner can become a different pair. An
				// existing row at the full-graph pair cannot prove no work.
				raw, err := encodeEdge(candidate)
				if err != nil {
					return err
				}
				return put(append(key('u'), pendingEdgeKey(candidate)...), raw)
			}
			k := pairKey(candidate)
			if err := stageOverrideCandidate(get, put, k, candidate); err != nil {
				return err
			}
			if proved && m.value[0] == supported && parent[0] == supported {
				k[2] = 'p'
				return stageOverrideCandidate(get, put, k, candidate)
			}
			return nil
		})
	}); err != nil {
		return err
	}
	if err := pendingVisit(ctx, b.pending, key('u'), func(r row) error {
		e, err := decodeEdgeLight(r.value)
		if err != nil || !bytes.Equal(r.key[3:], pendingEdgeKey(e)) {
			return errors.Join(err, invalid("override refusal identity"))
		}
		refuse(e)
		return nil
	}); err != nil {
		return err
	}
	if err := pendingVisit(ctx, b.pending, key('c'), func(r row) error {
		candidate, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		alternate, err := get(append(key('u'), pendingEdgeKey(candidate)...))
		if err != nil || alternate != nil {
			return err // already counted an ambiguous method at this key
		}
		existing, err := firstRebuildOverride(ctx, b.pending, candidate.From, candidate.To)
		if err != nil {
			return err
		}
		good := true
		if existing != nil {
			p, err := get(edgeProof(existing, existing.From))
			if err != nil {
				return err
			}
			good = bytes.Equal(p, []byte{supported})
			if good && graph.OriginRank(existing.Origin) >= graph.OriginRank(candidate.Origin) {
				return nil
			}
		}
		k := bytes.Clone(r.key)
		k[2] = 'p'
		best, err := pendingGet(b.pending, k, decodeEdge)
		if err != nil {
			return err
		}
		if !good || best == nil || graph.OriginRank(best.Origin) < graph.OriginRank(candidate.Origin) {
			refuse(candidate)
			return nil
		}
		upgrade := byte(0)
		if existing != nil {
			existing.Origin = candidate.Origin
			candidate = existing
			upgrade = 1
		}
		raw, err := encodeEdge(candidate)
		if err != nil {
			return err
		}
		return put(append(key('w'), pendingEdgeKey(candidate)...), append([]byte{upgrade}, raw...))
	}); err != nil {
		return err
	}
	if refused.Count != 0 {
		return &refused
	}
	return b.withWrites(func() error {
		return pendingVisit(ctx, b.pending, key('w'), func(r row) error {
			if len(r.value) < 2 {
				return invalid("override write plan shape")
			}
			e, err := decodeEdge(r.value[1:])
			if err != nil || !bytes.Equal(r.key[3:], pendingEdgeKey(e)) {
				return errors.Join(err, invalid("override write identity"))
			}
			old, err := b.pendingEdge(r.key[3:])
			if err != nil {
				return err
			}
			if r.value[0] == 1 && old != nil && graph.OriginRank(old.Origin) < graph.OriginRank(e.Origin) {
				_, err := b.setEdgeProvenance(e, e.Origin)
				return err
			}
			if old != nil || r.value[0] != 0 {
				return invalid("override destination changed after planning")
			}
			return b.putEdge(e)
		})
	})
}

func stageOverrideCandidate(get func([]byte) ([]byte, error), put func([]byte, []byte) error, key []byte, e *graph.Edge) error {
	prior, err := get(key)
	if err != nil {
		return err
	}
	if len(prior) != 0 {
		old, err := decodeEdgeLight(prior)
		if err != nil {
			return err
		}
		if old.From != e.From || old.To != e.To {
			return invalid("override native pair key aliases two endpoints")
		}
		if graph.OriginRank(old.Origin) >= graph.OriginRank(e.Origin) {
			return nil
		}
	}
	raw, err := encodeEdge(e)
	if err != nil {
		return err
	}
	return put(key, raw)
}

func firstRebuildOverride(ctx context.Context, db *pebble.DB, from, to string) (_ *graph.Edge, err error) {
	prefix := keyStrings(guardOutgoing(from, graph.EdgeOverrides), to)
	it, err := db.NewIterWithContext(ctx, &pebble.IterOptions{LowerBound: prefix, UpperBound: prefixEnd(prefix)})
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, it.Close()) }()
	if !it.First() {
		return nil, errors.Join(ctx.Err(), it.Error())
	}
	v, err := it.ValueAndErr()
	if err != nil {
		return nil, err
	}
	if len(it.Key())+len(v) > 16<<20 {
		return nil, fmt.Errorf("%w: existing override", ErrRebuildLinkInputSize)
	}
	e, err := decodeEdge(v)
	if err != nil || !bytes.Equal(it.Key(), pendingEdgeKey(e)) {
		return nil, errors.Join(err, invalid("existing override identity"))
	}
	return e, ctx.Err()
}

package store_pebble

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"iter"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

// RebuildImplements runs the cold method-set join on all settled candidates.
// It writes only proved supported matches. Deferred publication and the
// no-effect proof are separate; this method neither projects nor seals rows.
func (b *BuildStore) RebuildImplements(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	methods func(map[string]any) []string, synthesize func(*graph.Node, string) *graph.Edge,
) (err error) {
	const maxRow = 16 << 20
	const supported, deferred, mismatched = byte(1), byte(2), byte(4)
	prefix := []byte{'g', 'i'}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return err
	}
	if occupied {
		return invalid("implements scratch range occupied")
	}
	defer func() { err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync)) }()
	key := func(column byte, parts ...string) []byte { return keyStrings([]byte{'g', 'i', column}, parts...) }
	put := func(k, v []byte) error {
		if len(k)+len(v) > maxRow {
			return fmt.Errorf("%w: implements scratch row", ErrRebuildLinkInputSize)
		}
		return b.pending.Set(k, v, pebble.NoSync)
	}
	read := func(k []byte) (byte, error) {
		return pendingGet(b.pending, k, func(v []byte) (byte, error) {
			if len(v) != 1 || v[0] == 0 {
				return 0, invalid("implements proof or membership value")
			}
			return v[0], nil
		})
	}
	mark := func(k []byte, flag byte) error {
		prior, err := read(k)
		if err != nil || prior&flag == flag {
			return err
		}
		return put(k, []byte{prior | flag})
	}
	isOwner := func(n *graph.Node) bool {
		return n != nil && (n.Kind == graph.KindType || n.Kind == graph.KindInterface)
	}
	haveInterfaces := false
	view := b.RebuildView()
	if err := view.visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		if len(r.key)+len(r.value) > maxRow {
			return fmt.Errorf("%w: implements interface node", ErrRebuildLinkInputSize)
		}
		n, err := decodeNodeLight(r.value)
		if err != nil || n.Kind != graph.KindInterface {
			return err
		}
		if !bytes.Equal(r.key, pendingNodeKey(n.ID)) {
			return invalid("implements interface node identity")
		}
		haveInterfaces = true
		return put(key('I', n.ID), []byte{1})
	}); err != nil || !haveInterfaces {
		return err
	}
	node := func(id string) (*graph.Node, error) {
		return pendingGet(b.pending, pendingNodeKey(id), func(raw []byte) (*graph.Node, error) {
			if len(raw) > maxRow {
				return nil, fmt.Errorf("%w: implements node", ErrRebuildLinkInputSize)
			}
			n, err := decodeNode(raw)
			if err == nil && n.ID != id {
				return nil, invalid("implements node identity")
			}
			return n, err
		})
	}
	// A membership or explicit implements edge can be retargeted in main.
	// Its complete site and source are invariant, not a winning receipt ID.
	edgeProof := func(e *graph.Edge) []byte {
		return appendKeyLine(key('e', string(e.Kind), e.From, e.FilePath), e.Line)
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
			if file != path {
				return mismatched
			}
			return producer
		}
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if isOwner(n) || n.Kind == graph.KindMethod {
				if err := mark(key('n', n.ID), proof(n.FilePath)); err != nil {
					return err
				}
			}
		}
		for _, e := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			if e.Kind == graph.EdgeMemberOf || e.Kind == graph.EdgeImplements {
				if err := mark(edgeProof(e), proof(e.FilePath)); err != nil {
					return err
				}
			}
		}
	}
	const present = byte(1)
	const proved = byte(2)
	if err := b.visitRebuildDataflowKind(ctx, graph.EdgeMemberOf, decodeEdgeLight, func(e *graph.Edge) error {
		owner, err := node(e.To)
		if err != nil || !isOwner(owner) {
			return err
		}
		method, err := node(e.From)
		if err != nil || method == nil || method.Kind != graph.KindMethod {
			return err
		}
		k := key('m', owner.RepoPrefix, owner.ID, method.Name)
		prior, err := read(k)
		if err != nil || prior&proved != 0 {
			return err
		}
		if prior == 0 {
			if err := addPendingCount(b.pending, key('f', owner.RepoPrefix, method.Name), 1); err != nil {
				return err
			}
		}
		m, err := read(key('n', method.ID))
		if err != nil {
			return err
		}
		p, err := read(edgeProof(e))
		if err != nil {
			return err
		}
		flag := present
		if m == supported && p == supported {
			flag |= proved // independent evidence for this name is sufficient
		}
		return mark(k, flag)
	}); err != nil {
		return err
	}
	// Preserve native pair suppression, including its historical pair key.
	pair := func(from, to string) []byte { return key('x', from+"\x00"+to) }
	if err := b.visitRebuildDataflowKind(ctx, graph.EdgeImplements, decodeEdgeLight, func(e *graph.Edge) error {
		p, err := read(edgeProof(e))
		if err != nil {
			return err
		}
		flag := present
		if p == supported {
			flag |= proved
		}
		return mark(pair(e.From, e.To), flag)
	}); err != nil {
		return err
	}
	if err := pendingVisit(ctx, b.pending, key('I'), func(r row) error {
		id, _, err := readKeyString(r.key[3:])
		if err != nil {
			return err
		}
		n, err := node(id)
		if err != nil || n == nil {
			return errors.Join(err, invalid("implements interface disappeared"))
		}
		required := methods(n.Meta)
		if len(required) == 0 {
			return nil
		}
		anchor, minimum := "", 0
		for _, name := range required {
			if err := ctx.Err(); err != nil {
				return err
			}
			count, err := pendingGet(b.pending, key('f', n.RepoPrefix, name), decodeCount)
			if err != nil || count == 0 {
				return err
			}
			if minimum == 0 || count < minimum {
				anchor, minimum = name, count // sorted names give native first tie
			}
		}
		return put(key('a', n.RepoPrefix, anchor, n.ID), []byte{1})
	}); err != nil {
		return err
	}
	var refused RebuildUnknownProvenanceError
	if err := pendingVisit(ctx, b.pending, key('m'), func(r row) error {
		repo, tail, err := readKeyString(r.key[3:])
		if err != nil {
			return err
		}
		ownerID, tail, err := readKeyString(tail)
		if err != nil {
			return err
		}
		name, _, err := readKeyString(tail)
		if err != nil {
			return err
		}
		posting := key('a', repo, name)
		return pendingVisit(ctx, b.pending, posting, func(p row) error {
			ifaceID, _, err := readKeyString(p.key[len(posting):])
			if err != nil || ifaceID == ownerID {
				return err
			}
			existing, err := read(pair(ownerID, ifaceID))
			if err != nil || existing&proved != 0 {
				return err // a proved suppressor already makes this no work
			}
			ownerProof, err := read(key('n', ownerID))
			if err != nil || ownerProof == deferred {
				return err
			}
			ifaceProof, err := read(key('n', ifaceID))
			if err != nil || ifaceProof == deferred {
				return err // leave clearly deferred output to projection
			}
			iface, err := node(ifaceID)
			if err != nil || iface == nil {
				return errors.Join(err, invalid("implements posting interface disappeared"))
			}
			good := ownerProof == supported && ifaceProof == supported && existing == 0
			for _, required := range methods(iface.Meta) {
				member, err := read(key('m', repo, ownerID, required))
				if err != nil || member == 0 {
					return err // native miss, including with unproved neighbors
				}
				good = good && member&proved != 0
			}
			owner, err := node(ownerID)
			if err != nil || owner == nil {
				return errors.Join(err, invalid("implements owner disappeared"))
			}
			e := synthesize(owner, ifaceID)
			if !good {
				if refused.Count == 0 {
					refused.First = graph.EdgeIdentityFor(e)
				}
				refused.Count++
				return nil
			}
			raw, err := encodeEdge(e)
			if err != nil {
				return err
			}
			return put(append(key('w'), pendingEdgeKey(e)...), raw)
		})
	}); err != nil {
		return err
	}
	if refused.Count > 0 {
		return &refused
	}
	return b.withWrites(func() error {
		return pendingVisit(ctx, b.pending, key('w'), func(r row) error {
			e, err := decodeEdge(r.value)
			if err != nil || !bytes.Equal(r.key[3:], pendingEdgeKey(e)) {
				return errors.Join(err, invalid("implements write identity"))
			}
			prior, err := b.pendingEdge(r.key[3:])
			if err != nil {
				return err
			}
			if prior != nil {
				return invalid("implements insert would overwrite a saved row")
			}
			if err := b.putEdge(e); err != nil {
				return err
			}
			digest := sha256.Sum256(r.value)
			return b.pending.Set(rebuildImplementsStampKey(r.key[3:]), digest[:], pebble.NoSync)
		})
	})
}

// This producer witness is pending-only. It is not edge custody and does not
// change the saved edge encoding or any stage's rows.
func rebuildImplementsStampKey(live []byte) []byte {
	return append([]byte{'g', 'q', 'I'}, live...)
}

// A later same-key replacement must not borrow a different payload's proof.
func rebuildImplementsStamped(reader pebble.Reader, live []byte) (bool, error) {
	return pendingGet(reader, rebuildImplementsStampKey(live), func(stamp []byte) (bool, error) {
		if len(stamp) != sha256.Size {
			return false, invalid("implements producer stamp")
		}
		return pendingGet(reader, live, func(raw []byte) (bool, error) {
			digest := sha256.Sum256(raw)
			return bytes.Equal(stamp, digest[:]), nil
		})
	})
}

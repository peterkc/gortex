package store_pebble

import (
	"bytes"
	"encoding/binary"

	"github.com/zzet/gortex/internal/graph"
)

type rebuildMemberOwners struct {
	class, movedClass                                      string
	seen, different, exact, unproved, moved, movedConflict bool
}

func (o *rebuildMemberOwners) observe(class string) {
	if !o.seen {
		o.class, o.seen = class, true
	}
	o.different = o.different || o.class != class
}

func (o *rebuildMemberOwners) prove(p *rebuildCallMoves, raw, live []byte, class string) error {
	if raw == nil {
		o.unproved = true
		return nil
	}
	claim, err := p.marked(append(p.column('u'), raw...))
	if err != nil {
		return err
	}
	ok := len(claim) > 1 && claim[0] == rebuildScratchExact && bytes.Equal(claim[1:], live)
	o.exact, o.unproved = o.exact || ok, o.unproved || !ok
	if !bytes.Equal(raw, live) {
		o.movedConflict = o.movedConflict || o.moved && o.movedClass != class
		o.moved, o.movedClass = true, class
	}
	return nil
}

func (o *rebuildMemberOwners) elect(p *rebuildCallMoves, id string, lastCol byte, err error) (class string, exact, uncertain bool, electErr error) {
	if err != nil || !o.different {
		return o.class, o.exact, false, err
	}
	// The SQLite resolved-to-resolved MemberOf rewrite appends after
	// retained rows. Raw receipts don't order different rewritten owners.
	if o.unproved || o.movedConflict {
		return "", false, true, nil
	}
	if o.moved {
		return o.movedClass, true, false, nil
	}
	last, err := p.get(p.key(lastCol, id))
	if err != nil {
		return "", false, false, err
	}
	class, rest, err := readKeyString(last)
	if err != nil || len(rest) != 0 {
		return "", false, false, invalid(p.label + " last owner")
	}
	return class, true, false, nil
}

func (p *rebuildCallMoves) recordOwnerEdge(e *graph.Edge, admitted bool, order uint64, liveQuery func(*graph.Edge) bool,
	owner func(*graph.Edge) (string, bool), lastCol byte, missing func(error) error,
) error {
	col := byte('r')
	if e.Kind == graph.EdgeMemberOf {
		col = 'u'
	}
	raw := pendingEdgeKey(e)
	key := append(p.column(col), raw...)
	claim, err := p.marked(key)
	if err != nil || len(claim) <= 1 {
		return err
	}
	live, err := p.edge(claim[1:])
	if err != nil || live == nil {
		return missing(err)
	}
	if col == 'r' {
		admitted = liveQuery(live) && admitted
	}
	if admitted && claim[0] == rebuildCapabilityWanted {
		if col == 'r' {
			if err := p.put(binary.BigEndian.AppendUint64(p.column('z'), order), raw); err != nil {
				return err
			}
		} else if bytes.Equal(raw, claim[1:]) {
			class, ok := owner(live)
			if !ok {
				return invalid(p.label + " member role changed")
			}
			if err := p.put(p.key(lastCol, live.From), appendKeyString(nil, class)); err != nil {
				return err
			}
		}
	}
	return noteRebuildScratchPayload(p.put, key, claim, rebuildScratchProofBit(admitted))
}

func (p *rebuildCallMoves) orderCallReceipt(raw []byte) ([]byte, error) {
	key := append(p.column('r'), raw...)
	claim, err := p.marked(key)
	if err != nil || len(claim) <= 1 || claim[0] != rebuildScratchExact {
		return nil, err
	}
	// Expose this first exact receipt to the shared move-order writer.
	claim[0] = rebuildCapabilityWanted
	return claim[1:], p.noteReceipt(key, claim, true)
}

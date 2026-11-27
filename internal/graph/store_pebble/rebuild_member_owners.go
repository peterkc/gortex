package store_pebble

import (
	"bytes"
	"encoding/binary"

	"github.com/zzet/gortex/internal/graph"
)

type rebuildMemberOwners struct {
	class, unorderedClass                        string
	seen, different, exact, unordered, disagrees bool
}

func (o *rebuildMemberOwners) observe(class string) {
	if !o.seen {
		o.class, o.seen = class, true
	}
	o.different = o.different || o.class != class
}

func (p *rebuildCallMoves) memberOrderRetained(raw, live []byte) (bool, error) {
	if bytes.Equal(raw, live) {
		return true, nil
	}
	witness, err := p.get(rebuildReceiverOrderKey(live))
	return bytes.Equal(witness, raw), err
}

func (o *rebuildMemberOwners) prove(p *rebuildCallMoves, raw, live []byte, class string) error {
	if raw != nil {
		claim, err := p.marked(append(p.column('u'), raw...))
		if err != nil {
			return err
		}
		if len(claim) > 1 && claim[0] == rebuildScratchExact && bytes.Equal(claim[1:], live) {
			o.exact = true
			retained, err := p.memberOrderRetained(raw, live)
			if err != nil || retained {
				return err
			}
		}
	}
	o.disagrees = o.disagrees || o.unordered && o.unorderedClass != class
	o.unordered, o.unorderedClass = true, class
	return nil
}

func (o *rebuildMemberOwners) elect(p *rebuildCallMoves, id string, lastCol byte, err error) (class string, exact, uncertain bool, electErr error) {
	if err != nil || !o.different {
		return o.class, o.exact, false, err
	}
	// Known in-place repairs keep their receipt position. Other rewrites
	// can append or retain their position; don't guess when the winner changes.
	last, err := p.get(p.key(lastCol, id))
	if err != nil {
		return "", false, false, err
	}
	if last == nil || o.disagrees {
		return "", false, true, nil
	}
	class, rest, err := readKeyString(last)
	if err != nil || len(rest) != 0 {
		return "", false, false, invalid(p.label + " last owner")
	}
	if o.unordered && o.unorderedClass != class {
		return "", false, true, nil
	}
	return class, true, false, nil
}

func (p *rebuildCallMoves) recordOwnerEdge(e *graph.Edge, admitted bool, order uint64, liveQuery func(*graph.Edge) bool,
	owner func(*graph.Edge) (string, bool), lastCol byte, missing func(error) error, edge func([]byte) (*graph.Edge, error),
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
	live, err := edge(claim[1:])
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
		} else {
			retained, err := p.memberOrderRetained(raw, claim[1:])
			if err != nil {
				return err
			}
			if !retained {
				return noteRebuildScratchPayload(p.put, key, claim, rebuildCapabilityProved)
			}
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

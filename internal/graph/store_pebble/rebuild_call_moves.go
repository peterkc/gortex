package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
)

// rebuildCallMoves plans ordered Call rebinds under one stage's scratch
// prefix. The caller owns scratch cleanup, receipt proof, and error wrapping.
type rebuildCallMoves struct {
	b        *BuildStore
	prefix   []byte
	rowBytes int
	label    string
	inputRow string
	ordered  uint64
}

func (p *rebuildCallMoves) column(col byte) []byte {
	return append(bytes.Clone(p.prefix), col)
}

func (p *rebuildCallMoves) put(key, value []byte) error {
	return p.b.putRebuildScratch(key, value, p.rowBytes, p.label)
}

func (p *rebuildCallMoves) present(key []byte) (bool, error) {
	return pendingGet(p.b.pending, key, func([]byte) (bool, error) { return true, nil })
}

func (p *rebuildCallMoves) stage(old []byte, e *graph.Edge) error {
	val, err := encodeEdge(e)
	if err != nil {
		return err
	}
	return p.put(append(p.column('m'), old...), val)
}

func (p *rebuildCallMoves) moves(ctx context.Context, yield func([]byte, *graph.Edge) error) error {
	prefix := p.column('m')
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		e, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		return yield(r.key[len(prefix):], e)
	})
}

// noteReceipt updates a raw-key claim and indexes the first exact receipt for an
// actual mover. Frozen receipt order matches SQLite rowid order only for
// rows inserted and retained in that order. Native UPDATE keeps an earlier
// move's rowid, delete+insert appends it, and the in-memory shadow sorts
// batches. Stationary calls take no slot.
func (p *rebuildCallMoves) noteReceipt(key, claim []byte, exact bool) error {
	if exact && claim[0] == rebuildCapabilityWanted {
		moving, err := p.present(append(p.column('m'), claim[1:]...))
		if err != nil {
			return err
		}
		if moving {
			order := binary.BigEndian.AppendUint64(p.window(p.ordered), p.ordered)
			if err := p.put(order, key[len(p.prefix)+1:]); err != nil {
				return err
			}
			p.ordered++
		}
	}
	return noteRebuildScratchPayload(p.put, key, claim, rebuildScratchProofBit(exact))
}

func (p *rebuildCallMoves) window(ordinal uint64) []byte {
	return binary.BigEndian.AppendUint64(p.column('o'), ordinal/graph.SQLiteReindexChunkSize)
}

func (p *rebuildCallMoves) visitWindow(ctx context.Context, ordinal uint64, visit func(old, raw []byte) error) error {
	return pendingVisit(ctx, p.b.pending, p.window(ordinal), func(r row) error {
		key := append(p.column('r'), r.value...)
		old, err := pendingGet(p.b.pending, key, func(claim []byte) ([]byte, error) {
			if len(claim) < 2 || claim[0] != rebuildScratchExact {
				return nil, invalid(p.label + " ordered move has no exact receipt")
			}
			return bytes.Clone(claim[1:]), nil
		})
		if err != nil {
			return err
		}
		if old == nil {
			return invalid(p.label + " ordered move has no receipt claim")
		}
		return visit(old, r.value)
	})
}

func (p *rebuildCallMoves) input(key, val []byte) error {
	if len(key)+len(val) > p.rowBytes {
		return fmt.Errorf("%w: %s %s", ErrRebuildLinkInputSize, p.label, p.inputRow)
	}
	return nil
}

func (p *rebuildCallMoves) decodeKeyedEdge(key, val []byte) (*graph.Edge, error) {
	e, err := decodeEdge(val)
	if err == nil && !bytes.Equal(key, pendingEdgeKey(e)) {
		return nil, invalid(p.label + " edge does not match key")
	}
	return e, err
}

func (p *rebuildCallMoves) edge(key []byte) (*graph.Edge, error) {
	return pendingGet(p.b.pending, key, func(val []byte) (*graph.Edge, error) {
		if err := p.input(key, val); err != nil {
			return nil, err
		}
		return p.decodeKeyedEdge(key, val)
	})
}

// apply runs inside the caller's write scope, after every move has proved.
// Each window first frees all old keys, then admits claims in receipt order.
// Read the planned payload and raw key from scratch, not from an earlier window.
func (p *rebuildCallMoves) apply(ctx context.Context, planned uint64) error {
	if p.ordered != planned {
		return invalid(p.label + " ordered move count does not match plan")
	}
	for ordinal := uint64(0); ordinal < p.ordered; ordinal += graph.SQLiteReindexChunkSize {
		if err := p.visitWindow(ctx, ordinal, func(old, _ []byte) error {
			prior, err := p.edge(old)
			if err != nil {
				return err
			}
			if prior == nil {
				return invalid(p.label + " move has no old edge")
			}
			return p.b.deleteEdge(old, prior)
		}); err != nil {
			return err
		}
		if err := p.visitWindow(ctx, ordinal, func(old, raw []byte) error {
			e, err := pendingGet(p.b.pending, append(p.column('m'), old...), decodeEdge)
			if err != nil {
				return err
			}
			if e == nil {
				return invalid(p.label + " ordered move has no planned edge")
			}
			occupied, err := p.present(pendingEdgeKey(e))
			if err != nil || occupied {
				return err
			}
			return p.b.addEdgeWithCustody(e, rebuildEdgeCustody{raw: raw})
		}); err != nil {
			return err
		}
	}
	return nil
}

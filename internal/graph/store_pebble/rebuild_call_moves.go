package store_pebble

import (
	"bytes"
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
)

// rebuildCallMoves plans noninteracting Call rebinds under one stage's scratch
// prefix. The caller owns scratch cleanup, receipt proof, and error wrapping.
type rebuildCallMoves struct {
	b        *BuildStore
	prefix   []byte
	rowBytes int
	label    string
	edgeRow  string
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
	dest := pendingEdgeKey(e)
	key := append(p.column('d'), dest...)
	seen, err := p.present(key)
	if err != nil {
		return err
	}
	if seen {
		key = append(p.column('x'), dest...)
	}
	if err := p.put(key, nil); err != nil {
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

func (p *rebuildCallMoves) checkOrder(ctx context.Context, refuse func(uint64, graph.EdgeIdentity) error) error {
	// With unique destinations and no destination among the moving old keys,
	// every incumbent stays put. No mover can affect another's SQLite result,
	// even across a 5,000-move window boundary.
	if err := p.moves(ctx, func(_ []byte, e *graph.Edge) error {
		dest := pendingEdgeKey(e)
		occupiedByMover, err := p.present(append(p.column('m'), dest...))
		if err != nil || !occupiedByMover {
			return err
		}
		return p.put(append(p.column('x'), dest...), nil)
	}); err != nil {
		return err
	}
	prefix := p.column('x')
	var count uint64
	var first graph.EdgeIdentity
	if err := pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		if count == 0 {
			var err error
			first, err = decodeLinkRecorder(r.key[len(prefix):])
			if err != nil {
				return err
			}
		}
		count++
		return nil
	}); err != nil {
		return err
	}
	if count != 0 {
		return refuse(count, first)
	}
	return nil
}

func (p *rebuildCallMoves) edgeInput(key, val []byte) error {
	if len(key)+len(val) > p.rowBytes {
		return fmt.Errorf("%w: %s %s", ErrRebuildLinkInputSize, p.label, p.edgeRow)
	}
	return nil
}

func (p *rebuildCallMoves) decodeEdge(key, val []byte) (*graph.Edge, error) {
	e, err := decodeEdge(val)
	if err == nil && !bytes.Equal(key, pendingEdgeKey(e)) {
		return nil, invalid(p.label + " edge does not match key")
	}
	return e, err
}

func (p *rebuildCallMoves) edge(key []byte) (*graph.Edge, error) {
	return pendingGet(p.b.pending, key, func(val []byte) (*graph.Edge, error) {
		if err := p.edgeInput(key, val); err != nil {
			return nil, err
		}
		return p.decodeEdge(key, val)
	})
}

func (p *rebuildCallMoves) validatedPresent(key []byte) (bool, error) {
	e, err := p.edge(key)
	return e != nil, err
}

// apply must run inside the caller's write scope after order and custody checks.
func (p *rebuildCallMoves) apply(ctx context.Context, hasIncumbent func([]byte) (bool, error)) error {
	return p.moves(ctx, func(old []byte, e *graph.Edge) error {
		prior, err := p.edge(old)
		if err != nil {
			return err
		}
		if prior == nil {
			return invalid(p.label + " move has no old edge")
		}
		incumbent, err := hasIncumbent(pendingEdgeKey(e))
		if err != nil {
			return err
		}
		raw, err := rebuildReceiptKey(p.b.pending, old)
		if err != nil {
			return err
		}
		if err := p.b.deleteEdge(old, prior); err != nil || incumbent {
			return err
		}
		return p.b.addEdgeWithCustody(e, rebuildEdgeCustody{raw: raw})
	})
}

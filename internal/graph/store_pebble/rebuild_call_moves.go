package store_pebble

import (
	"bytes"
	"context"

	"github.com/zzet/gortex/internal/graph"
)

func (p *rebuildMyBatisPass) present(key []byte) (bool, error) {
	return pendingGet(p.b.pending, key, func([]byte) (bool, error) { return true, nil })
}

func (p *rebuildMyBatisPass) stage(old []byte, e *graph.Edge) error {
	val, err := encodeEdge(e)
	if err != nil {
		return err
	}
	dest := pendingEdgeKey(e)
	key := append(rebuildMyBatisKey('d'), dest...)
	seen, err := p.present(key)
	if err != nil {
		return err
	}
	if seen {
		key = append(rebuildMyBatisKey('x'), dest...)
	}
	if err := p.put(key, nil); err != nil {
		return err
	}
	p.stats.Moves++
	return p.put(append(rebuildMyBatisKey('m'), old...), val)
}

func (p *rebuildMyBatisPass) moves(ctx context.Context, yield func([]byte, *graph.Edge) error) error {
	prefix := rebuildMyBatisKey('m')
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		e, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		return yield(r.key[len(prefix):], e)
	})
}

func (p *rebuildMyBatisPass) checkOrder(ctx context.Context) error {
	// With unique destinations and no destination among the moving old keys,
	// every incumbent stays put. No mover can affect another's SQLite result,
	// even across a 5,000-move window boundary.
	if err := p.moves(ctx, func(_ []byte, e *graph.Edge) error {
		dest := pendingEdgeKey(e)
		occupiedByMover, err := p.present(append(rebuildMyBatisKey('m'), dest...))
		if err != nil || !occupiedByMover {
			return err
		}
		return p.put(append(rebuildMyBatisKey('x'), dest...), nil)
	}); err != nil {
		return err
	}
	prefix := rebuildMyBatisKey('x')
	var order RebuildMyBatisOrderError
	if err := pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		if order.Count == 0 {
			var err error
			order.First, err = decodeLinkRecorder(r.key[len(prefix):])
			if err != nil {
				return err
			}
		}
		order.Count++
		return nil
	}); err != nil {
		return err
	}
	if order.Count != 0 {
		return &order
	}
	return nil
}

func (p *rebuildMyBatisPass) edge(key []byte) (*graph.Edge, error) {
	return pendingGet(p.b.pending, key, func(val []byte) (*graph.Edge, error) {
		if err := rebuildMyBatisInput(key, val); err != nil {
			return nil, err
		}
		e, err := decodeEdge(val)
		if err == nil && !bytes.Equal(key, pendingEdgeKey(e)) {
			return nil, invalid("MyBatis edge does not match key")
		}
		return e, err
	})
}

func (p *rebuildMyBatisPass) apply(ctx context.Context) error {
	return p.moves(ctx, func(old []byte, e *graph.Edge) error {
		prior, err := p.edge(old)
		if err != nil {
			return err
		}
		if prior == nil {
			return invalid("MyBatis move has no old edge")
		}
		incumbent, err := p.edge(pendingEdgeKey(e))
		if err != nil {
			return err
		}
		raw, err := rebuildReceiptKey(p.b.pending, old)
		if err != nil {
			return err
		}
		if err := p.b.deleteEdge(old, prior); err != nil || incumbent != nil {
			return err
		}
		return p.b.addEdgeWithCustody(e, rebuildEdgeCustody{raw: raw})
	})
}

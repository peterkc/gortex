package store_pebble

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

var ErrRebuildGinMiddleware = errors.New("pebble rebuild Gin middleware failed")

var rebuildGinPrefix = []byte{'g', 'q', 'G'}

// RebuildGinRules supplies the native dispatcher, registration and edge rules.
type RebuildGinRules struct {
	Dispatcher func(*graph.Node) bool
	Names      func(*graph.Node) []string
	Target     func(*graph.Node, *graph.Node) bool
	Edge       func(*graph.Node, *graph.Node) *graph.Edge
	Threshold  uint64
}

type rebuildGinPass struct {
	rebuildCallMoves
	rules RebuildGinRules
}

// RebuildGinMiddleware adds speculative calls in the private build. Like
// native, its count includes offers that lose to an existing row.
func (b *BuildStore) RebuildGinMiddleware(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	rules RebuildGinRules,
) (offers uint64, err error) {
	defer func() {
		if err != nil {
			offers = 0
			err = fmt.Errorf("%w: %w", ErrRebuildGinMiddleware, err)
		}
	}()
	if receipts == nil || rules.Dispatcher == nil || rules.Names == nil || rules.Target == nil || rules.Edge == nil || rules.Threshold == 0 {
		return 0, invalid("gin middleware inputs")
	}
	prefix := rebuildGinPrefix
	busy, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return 0, err
	}
	if busy {
		return 0, invalid("gin middleware scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
	}()
	p := rebuildGinPass{
		rebuildCallMoves: rebuildCallMoves{b: b, prefix: prefix, rowBytes: 16 << 20, label: "Gin middleware"},
		rules:            rules,
	}
	active, err := p.capture(ctx)
	if err != nil || !active {
		return 0, err
	}
	offers, err = p.plan(ctx, ginLive)
	if err != nil || offers == 0 {
		return 0, err
	}
	if err := p.record(ctx, receipts); err != nil {
		return 0, err
	}
	if _, err := p.plan(ctx, ginProved); err != nil {
		return 0, err
	}
	if err := p.prove(ctx); err != nil {
		return 0, err
	}
	return offers, b.withWrites(func() error {
		return p.outputs(ctx, ginLive.output, func(e *graph.Edge) error {
			return b.addEdgeWithCustody(e, rebuildEdgeCustody{proof: rebuildProofUntrackedMove})
		})
	})
}

// Independent sorted indexes let the proof replay count offers even when the
// corresponding edge already exists. The only retained join state is a row.
type rebuildGinColumns struct{ dispatcher, name, target, seen, output byte }

var (
	ginLive   = rebuildGinColumns{'d', 'n', 'c', 's', 'w'}
	ginProved = rebuildGinColumns{'D', 'N', 'C', 'S', 'W'}
)

func rebuildGinCallable(n *graph.Node) bool {
	return n != nil && (n.Kind == graph.KindMethod || n.Kind == graph.KindFunction)
}

func (p *rebuildGinPass) capture(ctx context.Context) (bool, error) {
	var dispatchers, names bool
	if err := p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		n, err := checkedRebuildLightNode(r, p.label)
		if err != nil || !rebuildGinCallable(n) {
			return err
		}
		n, err = decodeNode(r.value)
		if err != nil {
			return err
		}
		if p.rules.Dispatcher(n) {
			dispatchers = true
			if err := p.put(p.key(ginLive.dispatcher, n.ID), nil); err != nil {
				return err
			}
		}
		for _, name := range p.rules.Names(n) {
			names = true
			if err := p.put(p.key(ginLive.name, name), nil); err != nil {
				return err
			}
		}
		return nil
	}); err != nil || !dispatchers || !names {
		return false, err
	}
	return true, p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		n, err := checkedRebuildLightNode(r, p.label)
		if err != nil || !rebuildGinCallable(n) {
			return err
		}
		named, err := p.present(p.key(ginLive.name, n.Name))
		if err != nil || !named {
			return err
		}
		return p.put(p.key(ginLive.target, n.WorkspaceID, n.Name, n.ID), nil)
	})
}

func (p *rebuildGinPass) record(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, raw := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !rebuildGinCallable(raw) || facts.EffectiveLanguage == "" || raw.FilePath != file {
				continue
			}
			dispatcher, names := p.rules.Dispatcher(raw), p.rules.Names(raw)
			target, err := p.present(p.key(ginLive.target, raw.WorkspaceID, raw.Name, raw.ID))
			if err != nil {
				return err
			}
			if !dispatcher && len(names) == 0 && !target {
				continue
			}
			live, err := p.b.pendingNode(raw.ID)
			if err != nil {
				return err
			}
			if !rebuildGinCallable(live) || !sameRebuildLanguageMarker(raw, live, file) {
				continue
			}
			if dispatcher && p.rules.Dispatcher(live) && raw.StartLine == live.StartLine && raw.WorkspaceID == live.WorkspaceID {
				if err := p.put(p.key(ginProved.dispatcher, raw.ID), nil); err != nil {
					return err
				}
			}
			if target {
				if err := p.put(p.key(ginProved.target, raw.WorkspaceID, raw.Name, raw.ID), nil); err != nil {
					return err
				}
			}
			if len(names) != 0 {
				liveNames := p.rules.Names(live)
				for _, name := range names {
					if slices.Contains(liveNames, name) {
						if err := p.put(p.key(ginProved.name, name), nil); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	return ctx.Err()
}

func (p *rebuildGinPass) visitStrings(ctx context.Context, prefix []byte, yield func(string) error) error {
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		s, tail, err := readKeyString(r.key[len(prefix):])
		if err != nil || len(tail) != 0 {
			return errors.Join(err, invalid("gin middleware index key"))
		}
		return yield(s)
	})
}

func (p *rebuildGinPass) plan(ctx context.Context, cols rebuildGinColumns) (uint64, error) {
	var offers uint64
	err := p.visitStrings(ctx, p.column(cols.dispatcher), func(id string) error {
		d, err := p.b.pendingNode(id)
		if err != nil || d == nil {
			return errors.Join(err, invalid("gin middleware dispatcher missing"))
		}
		return p.visitStrings(ctx, p.column(cols.name), func(name string) error {
			return p.offer(ctx, cols, d, name, &offers)
		})
	})
	return offers, err
}

func (p *rebuildGinPass) offer(ctx context.Context, cols rebuildGinColumns, d *graph.Node, name string, offers *uint64) (err error) {
	prefix := p.key(cols.target, d.WorkspaceID, name)
	it, err := p.b.pending.NewIterWithContext(ctx, &pebble.IterOptions{LowerBound: prefix, UpperBound: prefixEnd(prefix)})
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, it.Close())
	}()
	for ok := it.First(); ok; ok = it.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		id, tail, err := readKeyString(it.Key()[len(prefix):])
		if err != nil || len(tail) != 0 {
			return errors.Join(err, invalid("gin middleware candidate key"))
		}
		h := &graph.Node{ID: id, WorkspaceID: d.WorkspaceID}
		if !p.rules.Target(d, h) {
			continue
		}
		// Match native's seen identity, including its separator, across names
		// and dispatchers. Count new offers before checking for an incumbent.
		seenKey := p.key(cols.seen, d.ID+"\x00"+h.ID)
		seen, err := p.present(seenKey)
		if err != nil {
			return err
		}
		if seen {
			continue
		}
		if err := p.put(seenKey, nil); err != nil {
			return err
		}
		*offers++
		e := p.rules.Edge(d, h)
		key := pendingEdgeKey(e)
		occupied, err := p.present(key)
		if err != nil {
			return err
		}
		if !occupied {
			val, err := encodeEdge(e)
			if err != nil {
				return err
			}
			if err := p.put(append(p.column(cols.output), key...), val); err != nil {
				return err
			}
		}
		// Native breaks only this loop, even after reaching the threshold.
		if *offers >= p.rules.Threshold {
			break
		}
	}
	return errors.Join(ctx.Err(), it.Error())
}

func (p *rebuildGinPass) outputs(ctx context.Context, col byte, yield func(*graph.Edge) error) error {
	return visitRebuildScratchKeyedEdges(ctx, p.b.pending, p.column(col), 0, "gin middleware output key", yield)
}

func (p *rebuildGinPass) prove(ctx context.Context) error {
	var refused rebuildScratchRefusals
	for _, pair := range [...][2]byte{{ginLive.output, ginProved.output}, {ginProved.output, ginLive.output}} {
		if err := p.outputs(ctx, pair[0], func(e *graph.Edge) error {
			key := pendingEdgeKey(e)
			present, err := p.present(append(p.column(pair[1]), key...))
			// The constructor depends only on fields fixed by this key.
			if err == nil && !present {
				refused.add(graph.EdgeIdentityFor(e), key)
			}
			return err
		}); err != nil {
			return err
		}
	}
	return refused.err()
}

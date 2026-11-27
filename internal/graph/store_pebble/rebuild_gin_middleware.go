package store_pebble

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"

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
	SeenKey    func(string, string) string
	GoLanguage func(string) bool
	Threshold  uint64
}

type rebuildGinPass struct {
	rebuildCallMoves
	rules     RebuildGinRules
	goWitness bool
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
	if receipts == nil || rules.Dispatcher == nil || rules.Names == nil || rules.Target == nil || rules.Edge == nil || rules.SeenKey == nil || rules.GoLanguage == nil || rules.Threshold == 0 {
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
	if err := p.record(ctx, receipts); err != nil {
		return 0, err
	}
	if err := p.capture(ctx); err != nil {
		return 0, err
	}
	offers, err = p.plan(ctx)
	if err != nil {
		return 0, err
	}
	if err := p.prove(ctx); err != nil {
		return 0, err
	}
	if offers == 0 {
		return 0, nil
	}
	return offers, b.withWrites(func() error {
		return p.outputs(ctx, func(e *graph.Edge) error {
			return b.addEdgeWithCustody(e, rebuildEdgeCustody{proof: rebuildProofUntrackedMove})
		})
	})
}

// The live plan and the reference join each use disk indexes sorted like native:
// dispatcher ID, registered name, then target ID within a workspace.
type rebuildGinColumns struct{ dispatcher, name, target byte }

var (
	ginLive    = rebuildGinColumns{'d', 'n', 'c'}
	ginReceipt = rebuildGinColumns{'D', 'N', 'C'}
)

const (
	ginCertain = byte(1)
	ginOutput  = byte('w')
)

func rebuildGinCallable(n *graph.Node) bool {
	return n != nil && (n.Kind == graph.KindMethod || n.Kind == graph.KindFunction)
}

func (p *rebuildGinPass) record(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		if facts.EffectiveLanguage == "" {
			continue
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, raw := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if raw == nil || raw.FilePath != file {
				continue
			}
			known, err := p.present(pendingNodeKey(raw.ID))
			if err != nil {
				return err
			}
			if !known {
				continue
			}
			// Retain the last admitted row for an ID, even if it removes a
			// role. SQLite upserts the declaration; Option B fixes receipt order.
			n := graph.Node{ID: raw.ID, Kind: raw.Kind, Language: raw.Language, FilePath: file}
			if rebuildGinCallable(raw) {
				n.Name, n.StartLine, n.WorkspaceID = raw.Name, raw.StartLine, raw.WorkspaceID
				n.Meta = map[string]any{"gin_dispatcher": p.rules.Dispatcher(raw), "gin_handlers": p.rules.Names(raw)}
			}
			val, err := encodeNode(&n)
			if err != nil {
				return err
			}
			if err := p.put(p.key('r', n.ID), val); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

func (p *rebuildGinPass) capture(ctx context.Context) error {
	view := p.b.RebuildView()
	if err := view.visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		n, err := checkedRebuildLightNode(r, p.label)
		if err != nil {
			return err
		}
		raw, err := p.receipt(n.ID)
		if err != nil {
			return err
		}
		if raw != nil && p.rules.GoLanguage(n.Language) && sameRebuildLanguageMarker(raw, n, raw.FilePath) {
			p.goWitness = true
		}
		if rebuildGinCallable(n) {
			// Only callable rows need full metadata for the dispatcher and
			// registration probes. Bound those before decoding them.
			if len(r.key)+len(r.value) > p.rowBytes {
				return fmt.Errorf("%w: gin callable node", ErrRebuildLinkInputSize)
			}
			n, err = decodeNode(r.value)
			if err != nil {
				return err
			}
			if err := p.indexRoles(ginLive, n, ginCertain); err != nil {
				return err
			}
		}
		if raw != nil {
			return p.indexRoles(ginReceipt, raw, ginCertain)
		}
		return p.indexRoles(ginReceipt, n, 0)
	}); err != nil {
		return err
	}
	return view.visit(ctx, []byte{familyNode}, func(snap *pebble.Snapshot, r row) error {
		n, err := checkedRebuildLightNode(r, p.label)
		if err != nil {
			return err
		}
		if err := p.indexTarget(ginLive, n, ginCertain); err != nil {
			return err
		}
		raw, err := p.receipt(n.ID)
		if err != nil {
			return err
		}
		if raw != nil {
			return p.indexTarget(ginReceipt, raw, ginCertain)
		}
		if !rebuildGinCallable(n) {
			return nil
		}
		named, err := p.present(p.key(ginReceipt.name, n.Name))
		if err != nil || !named {
			return err
		}
		// Producer evidence applies only to the callable target. It cannot
		// satisfy the separate requirement for a receipt-backed Go node.
		stamped, err := rebuildGoExternalSymbolStamped(snap, r.key)
		if err != nil {
			return err
		}
		var state byte
		if stamped {
			state = ginCertain
		}
		return p.indexTarget(ginReceipt, n, state)
	})
}

func (p *rebuildGinPass) receipt(id string) (*graph.Node, error) {
	return pendingGet(p.b.pending, p.key('r', id), decodeNode)
}

func (p *rebuildGinPass) indexRoles(cols rebuildGinColumns, n *graph.Node, state byte) error {
	if !rebuildGinCallable(n) {
		return nil
	}
	if p.rules.Dispatcher(n) {
		// The edge constructor uses ID, file and line. Workspace is used
		// for the join. Neither role reads declaration language or name.
		d := &graph.Node{ID: n.ID, WorkspaceID: n.WorkspaceID, FilePath: n.FilePath, StartLine: n.StartLine}
		val, err := encodeNode(d)
		if err != nil {
			return err
		}
		if err := p.put(p.key(cols.dispatcher, n.ID), append([]byte{state}, val...)); err != nil {
			return err
		}
	}
	for _, name := range p.rules.Names(n) {
		key := p.key(cols.name, name)
		if state == 0 {
			old, err := p.present(key)
			if err != nil {
				return err
			}
			if old {
				continue // An optional registrar cannot remove a known name.
			}
		}
		if err := p.put(key, []byte{state}); err != nil {
			return err
		}
	}
	return nil
}

func (p *rebuildGinPass) indexTarget(cols rebuildGinColumns, n *graph.Node, state byte) error {
	if !rebuildGinCallable(n) {
		return nil
	}
	named, err := p.present(p.key(cols.name, n.Name))
	if err != nil || !named {
		return err
	}
	return p.put(p.key(cols.target, n.WorkspaceID, n.Name, n.ID), []byte{state})
}

func (p *rebuildGinPass) groups(ctx context.Context, cols rebuildGinColumns, visit func(*graph.Node, string, bool) error) error {
	dispatchers, names := p.column(cols.dispatcher), p.column(cols.name)
	return pendingVisit(ctx, p.b.pending, dispatchers, func(r row) error {
		if len(r.value) < 2 {
			return invalid("gin dispatcher index row")
		}
		d, err := decodeNode(r.value[1:])
		if err != nil {
			return err
		}
		certain := r.value[0] == ginCertain
		return pendingVisit(ctx, p.b.pending, names, func(n row) error {
			name, tail, err := readKeyString(n.key[len(names):])
			if err != nil || len(tail) != 0 || len(n.value) != 1 {
				return errors.Join(err, invalid("gin registration index row"))
			}
			return visit(d, name, certain && n.value[0] == ginCertain)
		})
	})
}

func (p *rebuildGinPass) targets(ctx context.Context, cols rebuildGinColumns, d *graph.Node, name string,
	visit func(*graph.Node, bool) (bool, error),
) (err error) {
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
			return errors.Join(err, invalid("gin candidate index key"))
		}
		h := &graph.Node{ID: id, WorkspaceID: d.WorkspaceID}
		if !p.rules.Target(d, h) {
			continue
		}
		state, err := it.ValueAndErr()
		if err != nil {
			return err
		}
		if len(state) != 1 {
			return invalid("gin candidate index row")
		}
		stop, err := visit(h, state[0] == ginCertain)
		if err != nil || stop {
			return err
		}
	}
	return errors.Join(ctx.Err(), it.Error())
}

func (p *rebuildGinPass) plan(ctx context.Context) (uint64, error) {
	var offers uint64
	err := p.groups(ctx, ginLive, func(d *graph.Node, name string, _ bool) error {
		return p.targets(ctx, ginLive, d, name, func(h *graph.Node, _ bool) (bool, error) {
			seenKey := p.key('s', p.rules.SeenKey(d.ID, h.ID))
			seen, err := p.present(seenKey)
			if err != nil || seen {
				return false, err
			}
			if err := p.put(seenKey, nil); err != nil {
				return false, err
			}
			offers++
			e := p.rules.Edge(d, h)
			key := pendingEdgeKey(e)
			occupied, err := p.present(key)
			if err != nil {
				return false, err
			}
			if !occupied {
				val, err := encodeEdge(e)
				if err != nil {
					return false, err
				}
				if err := p.put(append(p.column(ginOutput), key...), val); err != nil {
					return false, err
				}
			}
			// Native breaks the current group, not the next dispatcher/name.
			return offers >= p.rules.Threshold, nil
		})
	})
	return offers, err
}

func (p *rebuildGinPass) outputs(ctx context.Context, yield func(*graph.Edge) error) error {
	return visitRebuildScratchKeyedEdges(ctx, p.b.pending, p.column(ginOutput), 0, "gin middleware output key", yield)
}

func (p *rebuildGinPass) prove(ctx context.Context) error {
	var refused rebuildScratchRefusals
	cap := p.rules.Threshold
	var least, most uint64
	if err := p.groups(ctx, ginReceipt, func(d *graph.Node, name string, groupCertain bool) error {
		// Count occupied offers too. Only free keys participate in the result.
		// In each group the first available pair is offered even after cap.
		// Subsequent pairs need room in the global budget. Independent low
		// and high bounds cannot cancel uncertainty as a single replay can.
		var earlierCertain, earlierPossible bool
		leastInGroup := least
		err := p.targets(ctx, ginReceipt, d, name, func(h *graph.Node, targetCertain bool) (bool, error) {
			if earlierCertain && leastInGroup >= cap {
				return true, nil
			}
			// For a possible offer, this group is present. Its known targets
			// still count even if the dispatcher or name is optional globally.
			guaranteedTarget := targetCertain
			if strings.ContainsRune(d.ID, '\x00') || strings.ContainsRune(h.ID, '\x00') {
				// A NUL in either ID can collide under native's seen key.
				seen := p.key('S', p.rules.SeenKey(d.ID, h.ID))
				state, err := p.get(seen)
				if err != nil {
					return false, err
				}
				if len(state) != 0 && state[0] == ginCertain {
					return false, nil
				}
				guaranteedTarget = guaranteedTarget && state == nil
				var bit byte
				if groupCertain && guaranteedTarget && (!earlierPossible || most < cap) {
					bit = ginCertain
				}
				if err := p.put(seen, []byte{bit}); err != nil {
					return false, err
				}
			}
			certain := groupCertain && guaranteedTarget && (!earlierPossible || most < cap)
			e := p.rules.Edge(d, h)
			key := pendingEdgeKey(e)
			occupied, err := p.present(key)
			if err != nil {
				return false, err
			}
			if !occupied {
				live, err := p.present(append(p.column(ginOutput), key...))
				if err != nil {
					return false, err
				}
				if !live {
					refused.add(graph.EdgeIdentityFor(e), key)
				} else if certain {
					if err := p.put(append(p.column('W'), key...), nil); err != nil {
						return false, err
					}
				}
			}
			earlierPossible = true
			if most < cap {
				most++
			}
			if guaranteedTarget {
				earlierCertain = true
				if leastInGroup < cap {
					leastInGroup++
				}
			}
			return false, nil
		})
		if groupCertain {
			least = leastInGroup
		}
		return err
	}); err != nil {
		return err
	}
	if err := p.outputs(ctx, func(e *graph.Edge) error {
		key := pendingEdgeKey(e)
		proved, err := p.present(append(p.column('W'), key...))
		if err == nil && (!proved || !p.goWitness) {
			refused.add(graph.EdgeIdentityFor(e), key)
		}
		return err
	}); err != nil {
		return err
	}
	return refused.err()
}

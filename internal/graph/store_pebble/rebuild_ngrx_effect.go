package store_pebble

import (
	"context"
	"errors"
	"fmt"
	"iter"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

var ErrRebuildNgRxEffect = errors.New("pebble rebuild NgRx Effect stage failed")

var rebuildNgRxEffectPrefix = []byte{'g', 'R'}

const rebuildNgRxEffectRowBytes = 16 << 20
const rebuildNgRxActionGroup = "ngrx"

// RebuildNgRxEffectStats describes the live census and planned moves.
type RebuildNgRxEffectStats = RebuildStoreFactoryStats

// RebuildNgRxEffectRules supplies the native marker and payload decisions.
type RebuildNgRxEffectRules struct {
	Stub   func(*graph.Edge) string
	Getter func(*graph.Node, string) bool
	Rebind func(*graph.Edge, string, string, bool) bool
}

type rebuildNgRxEffectPass struct{ rebuildStoreFactoryPass }

// RebuildNgRxEffect runs on the quiet private graph after Redux Thunk.
// The worker must discard the build if applying the moves fails.
func (b *BuildStore) RebuildNgRxEffect(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	rules RebuildNgRxEffectRules,
) (stats RebuildNgRxEffectStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildNgRxEffectStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildNgRxEffect, err)
		}
	}()
	if receipts == nil || rules.Stub == nil || rules.Getter == nil || rules.Rebind == nil {
		return stats, fmt.Errorf("%w: invalid NgRx Effect inputs", ErrRebuildLinkInputSize)
	}
	prefix := rebuildNgRxEffectPrefix
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return stats, err
	}
	if occupied {
		return stats, invalid("NgRx Effect scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
	}()
	read := func(e *graph.Edge) rebuildStoreDispatchCall {
		action := rules.Stub(e)
		binding, _ := e.Meta["store_binding"].(string)
		return rebuildStoreDispatchCall{group: rebuildNgRxActionGroup, member: action, getter: binding, reset: action}
	}
	p := rebuildNgRxEffectPass{rebuildStoreFactoryPass: rebuildStoreFactoryPass{
		rebuildCallMoves: rebuildCallMoves{
			b: b, prefix: prefix, rowBytes: rebuildNgRxEffectRowBytes, label: "NgRx Effect", inputRow: "edge row",
		},
		rules: rebuildStoreDispatchRules{
			kind:   rebuildNgRxActionKind,
			action: func(n *graph.Node) (string, string) { return rebuildNgRxActionGroup, n.Name },
			call:   read,
			getter: rules.Getter,
			rebind: rules.Rebind,
		},
	}}
	if err := p.captureCalls(ctx); err != nil || p.stats.Stubs == 0 {
		return p.stats, err
	}
	if err := p.collectActions(ctx); err != nil {
		return stats, err
	}
	if err := p.planCalls(ctx); err != nil || p.stats.Moves == 0 {
		return p.stats, err
	}
	if err := p.proveCalls(ctx, receipts); err != nil {
		return stats, err
	}
	err = b.withWrites(func() error { return p.apply(ctx, p.stats.Moves) })
	return p.stats, err
}

func rebuildNgRxActionKind(kind graph.NodeKind) bool {
	return kind == graph.KindVariable || kind == graph.KindConstant || kind == graph.KindFunction || kind == graph.KindType
}

// h records the owned Call keys; q records their requested action names.
func (p *rebuildNgRxEffectPass) captureCalls(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || kind != graph.EdgeCalls {
			return err
		}
		e, err := p.decodeKeyedEdge(r.key, r.value)
		if err != nil {
			return err
		}
		call := p.rules.call(e)
		if call.member == "" {
			return nil
		}
		if err := p.input(r.key, r.value); err != nil {
			return err
		}
		p.stats.Stubs++
		custody, err := readRebuildEdgeCustody(snap, r.key)
		if err != nil && !errors.Is(err, ErrRebuildUnknownProvenance) {
			return err
		}
		// A valid stationary Call competes for raw custody too.
		if err == nil && custody.proof == 0 {
			if err := p.claim(custody.raw, r.key); err != nil {
				return err
			}
		}
		if err := p.want(p.key('q', call.member)); err != nil {
			return err
		}
		return p.put(append(p.key('h'), r.key...), nil)
	})
}

func (p *rebuildNgRxEffectPass) collectActions(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		n, err := checkedRebuildLightNode(r, p.label)
		if err != nil || !p.rules.kind(n.Kind) || n.Name == "" {
			return err
		}
		wanted, err := p.present(p.key('q', n.Name))
		if err != nil || !wanted {
			return err
		}
		if n.ID == "" {
			return invalid(p.label + " action has no ID")
		}
		if err := p.nodeInput(r.key, r.value); err != nil {
			return err
		}
		cohort := p.key('u', rebuildNgRxActionGroup, n.Name)
		if err := p.put(p.key('c', n.ID), append([]byte{rebuildCapabilityWanted}, cohort...)); err != nil {
			return err
		}
		for _, key := range rebuildStoreScopeKeys(cohort, len(p.prefix), 'a', n) {
			if err := p.addUnique(key, n.ID); err != nil {
				return err
			}
		}
		return nil
	})
}

func (p *rebuildNgRxEffectPass) planCalls(ctx context.Context) error {
	deferred := false
	plan := func(prefix []byte, resolveGetter bool) error {
		return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
			old := r.key[len(prefix):]
			e, err := p.edge(old)
			if err != nil {
				return err
			}
			if e == nil {
				return invalid(p.label + " planned Call missing")
			}
			call := p.rules.call(e)
			pick, err := p.elect(e.From, call, resolveGetter)
			if err != nil {
				return err
			}
			if !resolveGetter && pick.getter {
				deferred = true
				if err := p.want(p.key('b', call.getter)); err != nil {
					return err
				}
				return p.put(append(p.key('l'), old...), nil)
			}
			return p.planScopedCall(old, e, call, pick)
		})
	}
	if err := plan(p.key('h'), false); err != nil || !deferred {
		return err
	}
	if err := p.collectGetters(ctx); err != nil {
		return err
	}
	return plan(p.key('l'), true)
}

func (p *rebuildNgRxEffectPass) proveCalls(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	// An exact owned Call proves activation. Unlike Store Factory, native
	// NgRx still checks Calls when there are no action nodes at all.
	p.needsAnchor = false
	if err := p.prepareReceiptCallerProof(ctx, receipts); err != nil {
		return err
	}
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		if err := p.record(ctx, facts); err != nil {
			return err
		}
	}
	if _, err := p.proveActions(ctx); err != nil {
		return err
	}
	if err := p.foldGetterFiles(ctx, p.key('g'), p.node, p.addUnique); err != nil {
		return err
	}
	var refused rebuildScratchRefusals
	if err := p.moves(ctx, func(old []byte, e *graph.Edge) error {
		ok, err := p.proved(old, e, true) // proved checks the exact owned Call first.
		if err == nil && !ok {
			refused.add(graph.EdgeIdentityFor(e), pendingEdgeKey(e))
		}
		return err
	}); err != nil {
		return err
	}
	return refused.err()
}

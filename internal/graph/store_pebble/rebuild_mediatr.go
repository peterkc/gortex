package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

var ErrRebuildMediatR = errors.New("pebble rebuild MediatR stage failed")

var rebuildMediatRPrefix = []byte{'g', 'q', 'M'}

const rebuildMediatRNotification = "notification"

// RebuildMediatRStats counts Calls, native matches, moves and attempted fanout adds.
type RebuildMediatRStats struct {
	Stubs, Resolved, Moves, Adds uint64
}

// RebuildMediatRRules supplies the native marker and payload decisions.
type RebuildMediatRRules struct {
	Handler func(*graph.Node) (string, string)
	Call    func(*graph.Edge) (string, string)
	Getter  func(*graph.Node, string) bool
	Rebind  func(*graph.Edge, string, bool) bool
	Fanout  func(*graph.Edge, string, string) *graph.Edge
}

type rebuildMediatRPass struct {
	rebuildStoreFactoryPass
	fanout         func(*graph.Edge, string, string) *graph.Edge
	adds           uint64
	orderedSources uint64
}

// RebuildMediatR runs once on a fresh private graph, after Spring events.
func (b *BuildStore) RebuildMediatR(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	rules RebuildMediatRRules,
) (stats RebuildMediatRStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildMediatRStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildMediatR, err)
		}
	}()
	if receipts == nil || rules.Handler == nil || rules.Call == nil || rules.Getter == nil || rules.Rebind == nil || rules.Fanout == nil {
		return stats, fmt.Errorf("%w: invalid MediatR inputs", ErrRebuildLinkInputSize)
	}
	prefix := rebuildMediatRPrefix
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return stats, err
	}
	if occupied {
		return stats, invalid("MediatR scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
	}()
	read := func(e *graph.Edge) rebuildStoreDispatchCall {
		kind, reqType := rules.Call(e)
		var binding string
		if kind == "request" {
			binding, _ = e.Meta["store_binding"].(string)
		}
		return rebuildStoreDispatchCall{group: kind, member: reqType, getter: binding}
	}
	p := rebuildMediatRPass{
		rebuildStoreFactoryPass: rebuildStoreFactoryPass{
			rebuildCallMoves: rebuildCallMoves{b: b, prefix: prefix, rowBytes: 16 << 20, label: "MediatR", inputRow: "edge row"},
			needsAnchor:      true,
			rules: rebuildStoreDispatchRules{
				kind:   rebuildMediatRHandlerKind,
				action: rules.Handler,
				call:   read,
				getter: rules.Getter,
				rebind: func(e *graph.Edge, _, id string, matched bool) bool { return rules.Rebind(e, id, matched) },
			},
		},
		fanout: rules.Fanout,
	}
	active, err := p.collectHandlers(ctx)
	if err != nil || !active {
		return stats, err
	}
	if err := p.planCalls(ctx); err != nil {
		return stats, err
	}
	stats = RebuildMediatRStats{Stubs: p.stats.Stubs, Resolved: p.stats.Resolved, Moves: p.stats.Moves, Adds: p.adds}
	if stats.Moves == 0 && stats.Adds == 0 {
		return stats, nil
	}
	if err := p.proveCalls(ctx, receipts); err != nil {
		return stats, err
	}
	var run string
	if stats.Adds != 0 {
		dir, mkdirErr := os.MkdirTemp(filepath.Join(b.scratchParent, "pending"), "mediatr-fanout-")
		if mkdirErr != nil {
			return stats, mkdirErr
		}
		defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
		run = filepath.Join(dir, "fanout.sst")
		if err := p.prepareFanout(ctx, run); err != nil {
			return stats, err
		}
	}
	err = b.withWrites(func() error {
		if err := p.apply(ctx, stats.Moves); err != nil {
			return err
		}
		if run == "" {
			return nil
		}
		return p.applyFanout(ctx, run)
	})
	return stats, err
}

func rebuildMediatRHandlerKind(kind graph.NodeKind) bool {
	return kind == graph.KindMethod || kind == graph.KindFunction
}

// y and j retain notification members in handler ID order, globally and by
// workspace. c is the shared per-handler receipt claim.
func (p *rebuildMediatRPass) collectHandlers(ctx context.Context) (bool, error) {
	active := false
	err := p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		light, err := checkedRebuildLightNode(r, p.label)
		if err != nil || !rebuildMediatRHandlerKind(light.Kind) {
			return err
		}
		n, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		kind, reqType := p.rules.action(n)
		if kind == "" || reqType == "" {
			return nil
		}
		if n.ID == "" {
			return invalid("MediatR handler has no ID")
		}
		if err := p.nodeInput(r.key, r.value); err != nil {
			return err
		}
		active = true
		cohort := p.key('u', kind, reqType)
		if err := p.put(p.key('c', n.ID), append([]byte{rebuildCapabilityWanted}, cohort...)); err != nil {
			return err
		}
		if kind == rebuildMediatRNotification {
			if err := p.put(p.key('y', reqType, n.ID), nil); err != nil {
				return err
			}
			return p.put(p.key('j', reqType, n.WorkspaceID, n.ID), nil)
		}
		for _, key := range rebuildStoreScopeKeys(cohort, len(p.prefix), 'a', n) {
			if err := p.addUnique(key, n.ID); err != nil {
				return err
			}
		}
		return nil
	})
	return active, err
}

func (p *rebuildMediatRPass) notification(from, reqType string) (prefix, bucket []byte, caller *graph.Node, err error) {
	caller, err = p.node(from)
	if err != nil {
		return nil, nil, nil, err
	}
	if caller != nil {
		return p.key('j', reqType, caller.WorkspaceID), p.key('w', rebuildMediatRNotification, reqType, caller.WorkspaceID), caller, nil
	}
	return p.key('y', reqType), p.key('a', rebuildMediatRNotification, reqType), nil, nil
}

func (p *rebuildMediatRPass) handlers(ctx context.Context, prefix []byte, visit func(string) error) error {
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		id, rest, err := readKeyString(r.key[len(prefix):])
		if err != nil || id == "" || len(rest) != 0 {
			return invalid("MediatR notification handler key")
		}
		return visit(id)
	})
}

func (p *rebuildMediatRPass) planCalls(ctx context.Context) error {
	deferred := false
	if err := p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || kind != graph.EdgeCalls {
			return err
		}
		e, err := p.decodeKeyedEdge(r.key, r.value)
		if err != nil {
			return err
		}
		call := p.rules.call(e)
		if call.group == "" || call.member == "" {
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
		if err == nil && custody.proof == 0 {
			if err := p.claim(custody.raw, r.key); err != nil {
				return err
			}
		}
		if call.group == rebuildMediatRNotification {
			return p.planNotification(ctx, r, e, call)
		}
		pick, err := p.elect(e.From, call, false)
		if err != nil {
			return err
		}
		if !pick.getter {
			return p.planCall(r.key, e, call, pick)
		}
		deferred = true
		if err := p.want(p.key('b', call.getter)); err != nil {
			return err
		}
		return p.put(append(p.key('l'), r.key...), nil)
	}); err != nil || !deferred {
		return err
	}
	if err := p.collectGetters(ctx); err != nil {
		return err
	}
	prefix := p.key('l')
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		old := r.key[len(prefix):]
		e, err := p.edge(old)
		if err != nil {
			return err
		}
		if e == nil {
			return invalid("MediatR deferred Call missing")
		}
		call := p.rules.call(e)
		pick, err := p.elect(e.From, call, true)
		if err != nil {
			return err
		}
		return p.planCall(old, e, call, pick)
	})
}

func (p *rebuildMediatRPass) planNotification(ctx context.Context, r row, e *graph.Edge, call rebuildStoreDispatchCall) error {
	prefix, bucket, caller, err := p.notification(e.From, call.member)
	if err != nil {
		return err
	}
	var first string
	var count uint64
	if err := p.handlers(ctx, prefix, func(id string) error {
		if count == 0 {
			first = id
		}
		count++
		return nil
	}); err != nil {
		return err
	}
	p.stats.Resolved += count
	moving := p.rules.rebind(e, "", first, count != 0)
	if count <= 1 && !moving {
		return nil
	}
	if count > 1 {
		p.adds += count - 1
	}
	if moving {
		if err := p.stage(r.key, e); err != nil {
			return err
		}
		p.stats.Moves++
	}
	if err := p.want(p.key('u', call.group, call.member)); err != nil {
		return err
	}
	if err := p.wantCandidateBuckets([]rebuildStoreCandidateWitness{{bucket: bucket}}); err != nil {
		return err
	}
	if caller != nil {
		if err := p.want(p.key('n', caller.ID)); err != nil {
			return err
		}
	}
	return p.put(append(p.key('h'), r.key...), r.value)
}

func (p *rebuildMediatRPass) proveCalls(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		if err := p.record(ctx, facts); err != nil {
			return err
		}
		if p.adds != 0 {
			if err := p.orderNotifications(ctx, facts); err != nil {
				return err
			}
		}
	}
	anchor, err := p.proveActions(ctx)
	if err != nil {
		return err
	}
	if err := p.foldGetterFiles(ctx, p.key('g'), p.node, p.addUnique); err != nil {
		return err
	}
	var refused rebuildScratchRefusals
	if err := p.moves(ctx, func(old []byte, e *graph.Edge) error {
		if p.rules.call(e).group == rebuildMediatRNotification {
			return nil // notification moves and adds share one source proof below
		}
		ok, err := p.proved(old, e, anchor)
		if err == nil && !ok {
			refused.add(graph.EdgeIdentityFor(e), pendingEdgeKey(e))
		}
		return err
	}); err != nil {
		return err
	}
	prefix := p.key('h')
	if err := pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		old := r.key[len(prefix):]
		e, err := p.decodeKeyedEdge(old, r.value)
		if err != nil {
			return err
		}
		ok, err := p.notificationProved(ctx, old, e, anchor)
		if err != nil || ok {
			return err
		}
		planned, err := pendingGet(p.b.pending, append(p.column('m'), old...), decodeEdge)
		if err != nil {
			return err
		}
		if planned != nil {
			e = planned
		}
		refused.add(graph.EdgeIdentityFor(e), pendingEdgeKey(e))
		return nil
	}); err != nil {
		return err
	}
	return refused.err()
}

// A producer proof certifies the installed payload, not the raw source
// ordinal. Repeating this stage on its own output must not invent that order.
func (p *rebuildMediatRPass) notificationProved(ctx context.Context, old []byte, e *graph.Edge, anchor bool) (bool, error) {
	raw, err := rebuildReceiptKey(p.b.pending, old)
	if errors.Is(err, ErrRebuildUnknownProvenance) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	claim, err := p.marked(append(p.column('r'), raw...))
	if err != nil || len(claim) <= 1 || claim[0] != rebuildScratchExact || !bytes.Equal(claim[1:], old) || !anchor {
		return false, err
	}
	call := p.rules.call(e)
	prefix, bucket, caller, err := p.notification(e.From, call.member)
	if err != nil {
		return false, err
	}
	state, err := p.state(p.candidateProofKey(bucket))
	if err != nil || state == 0 || state&rebuildStoreCandidateExtra != 0 {
		return false, err
	}
	if caller != nil {
		state, err := p.state(p.key('n', caller.ID))
		if err != nil || state != rebuildScratchExact {
			return false, err
		}
	}
	ok := true
	err = p.handlers(ctx, prefix, func(id string) error {
		claim, err := p.marked(p.key('c', id))
		if err != nil {
			return err
		}
		ok = ok && len(claim) > 1 && rebuildStoreCandidateExact(claim[0]) &&
			(caller == nil || claim[0]&rebuildStoreOtherWorkspace == 0)
		return nil
	})
	return ok, err
}

// A and z track the first exact raw receipt for each notification source.
// Only those receipts can schedule fanout. Like reindexes, this witnesses
// native order when the original SQLite insertion order is retained.
func (p *rebuildMediatRPass) orderNotifications(ctx context.Context, facts ParseFacts) error {
	for _, raw := range facts.Edges {
		if err := ctx.Err(); err != nil {
			return err
		}
		if raw.Kind != graph.EdgeCalls || p.rules.call(raw).group != rebuildMediatRNotification {
			continue
		}
		claim, err := p.marked(append(p.column('r'), pendingEdgeKey(raw)...))
		if err != nil {
			return err
		}
		if len(claim) <= 1 || claim[0] != rebuildScratchExact {
			continue
		}
		old := claim[1:]
		key := append(p.key('A'), old...)
		seen, err := p.present(key)
		if err != nil {
			return err
		}
		if seen {
			continue
		}
		val, err := pendingGet(p.b.pending, append(p.key('h'), old...), copyValue)
		if err != nil {
			return err
		}
		if val == nil {
			continue
		}
		if err := p.put(key, nil); err != nil {
			return err
		}
		if err := p.put(binary.BigEndian.AppendUint64(p.key('z'), p.orderedSources), old); err != nil {
			return err
		}
		p.orderedSources++
	}
	return nil
}

// The fanout run is sorted by receipt, then handler ID. Prepare it before
// graph writes so an invalid proposal cannot follow an installed move.
func (p *rebuildMediatRPass) prepareFanout(ctx context.Context, path string) (err error) {
	w, err := newLinkTable(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, w.Close()) }()
	var adds uint64
	if err := pendingVisit(ctx, p.b.pending, p.key('z'), func(r row) error {
		old := r.value
		val, err := pendingGet(p.b.pending, append(p.key('h'), old...), copyValue)
		if err != nil {
			return err
		}
		if val == nil {
			return invalid("MediatR fanout source missing")
		}
		e, err := p.decodeKeyedEdge(old, val)
		if err != nil {
			return err
		}
		call := p.rules.call(e)
		prefix, _, _, err := p.notification(e.From, call.member)
		if err != nil {
			return err
		}
		first := true
		return p.handlers(ctx, prefix, func(id string) error {
			if first {
				first = false
				return nil
			}
			add := p.fanout(e, id, call.member)
			val, err := encodeEdge(add)
			if err != nil {
				return err
			}
			if err := p.input(pendingEdgeKey(add), val); err != nil {
				return err
			}
			key := binary.BigEndian.AppendUint64(nil, adds)
			adds++
			return w.Set(key, val)
		})
	}); err != nil {
		return err
	}
	if adds != p.adds {
		return invalid("MediatR ordered fanout count does not match plan")
	}
	return nil
}

func (p *rebuildMediatRPass) applyFanout(ctx context.Context, path string) error {
	return visitLinkRun(ctx, path, nil, func(r row) error {
		e, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		occupied, err := p.present(pendingEdgeKey(e))
		if err != nil || occupied {
			return err
		}
		return p.b.addEdgeWithCustody(e, rebuildEdgeCustody{proof: rebuildProofMediatRFanout})
	})
}

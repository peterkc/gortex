package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
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
	fanout func(*graph.Edge, string, string) *graph.Edge
	adds   uint64
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
	if stats.Adds != 0 {
		if err := p.prepareFanout(ctx); err != nil {
			return stats, err
		}
	}
	err = b.withWrites(func() error {
		if err := p.apply(ctx, stats.Moves); err != nil {
			return err
		}
		return p.applyFanout(ctx)
	})
	return stats, err
}

func rebuildMediatRHandlerKind(kind graph.NodeKind) bool {
	return kind == graph.KindMethod || kind == graph.KindFunction
}

// y and j retain notification members in handler ID order, globally and by
// workspace. W records whether all handlers of a type share one workspace.
// c is the shared per-handler receipt claim.
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
			if err := p.addUnique(p.key('W', reqType), "="+n.WorkspaceID); err != nil {
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
			return p.planRequest(r.key, e, call, pick)
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
		return p.planRequest(old, e, call, pick)
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
		// Native may see no caller in the receipts and use the global cohort.
		if err := p.wantCandidateBuckets([]rebuildStoreCandidateWitness{{bucket: p.key('a', call.group, call.member)}}); err != nil {
			return err
		}
	}
	if err := p.want(p.key('N', e.From, call.member)); err != nil {
		return err
	}
	return p.put(append(p.key('h'), r.key...), r.value)
}

// Only moving requests with a nonempty global cohort can change their
// election when receipts introduce an otherwise missing caller.
func (p *rebuildMediatRPass) planRequest(old []byte, e *graph.Edge, call rebuildStoreDispatchCall, pick rebuildStoreFactoryElection) error {
	moves := p.stats.Moves
	if err := p.planCall(old, e, call, pick); err != nil {
		return err
	}
	if p.stats.Moves != moves && pick.nonempty && pick.caller == nil {
		return p.put(p.key('R', e.From), nil)
	}
	return nil
}

// Mark the deciding receipt-only caller buckets before recording handler
// receipts. A scoped decision does not need proof of an unrelated global row.
func (p *rebuildMediatRPass) prepareRequestCallerProof(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	wanted, err := rebuildGuardHasPrefix(ctx, p.b.pending, p.key('R'))
	if err != nil || !wanted {
		return err
	}
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		if facts.EffectiveLanguage == "" {
			continue
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if n == nil || n.FilePath != file {
				continue
			}
			key := p.key('R', n.ID)
			wanted, err := p.present(key)
			if err != nil {
				return err
			}
			if wanted {
				// Like the native node upsert, keep the last admitted row.
				if err := p.put(key, keyStrings(nil, n.WorkspaceID, file)); err != nil {
					return err
				}
			}
		}
	}
	return p.moves(ctx, func(_ []byte, e *graph.Edge) error {
		call := p.rules.call(e)
		if call.group == rebuildMediatRNotification {
			return nil
		}
		pick, err := p.receiptRequestPick(e.From, call)
		if err != nil || pick.caller == nil {
			return err
		}
		return p.wantCandidateBuckets(pick.buckets)
	})
}

func (p *rebuildMediatRPass) receiptRequestPick(from string, call rebuildStoreDispatchCall) (rebuildStoreFactoryElection, error) {
	val, err := p.get(p.key('R', from))
	if err != nil || len(val) == 0 {
		return rebuildStoreFactoryElection{}, err
	}
	return p.electWithCaller(call, true, func() (*graph.Node, error) {
		workspace, tail, err := readKeyString(val)
		if err != nil {
			return nil, err
		}
		file, _, err := readKeyString(tail)
		if err != nil {
			return nil, err
		}
		return &graph.Node{WorkspaceID: workspace, FilePath: file}, nil
	})
}

func (p *rebuildMediatRPass) requestProved(old []byte, e *graph.Edge, anchor bool) (bool, error) {
	call := p.rules.call(e)
	pick, err := p.receiptRequestPick(e.From, call)
	if err != nil {
		return false, err
	}
	if pick.caller == nil {
		return p.proved(old, e, anchor)
	}
	ok, err := p.callProved(old)
	if err != nil || !ok {
		return ok, err
	}
	live, err := p.elect(e.From, call, true)
	if err != nil || live.target != pick.target {
		return false, err
	}
	return p.electionProved(call, pick, anchor)
}

func (p *rebuildMediatRPass) proveCalls(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	if err := p.prepareRequestCallerProof(ctx, receipts); err != nil {
		return err
	}
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		if err := p.record(ctx, facts); err != nil {
			return err
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, raw := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := p.recordNotificationCaller(ctx, raw, file, facts.EffectiveLanguage); err != nil {
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
		ok, err := p.requestProved(old, e, anchor)
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

func (p *rebuildMediatRPass) recordNotificationCaller(ctx context.Context, raw *graph.Node, file, lang string) error {
	prefix := p.key('N', raw.ID)
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		reqType, _, err := readKeyString(r.key[len(prefix):])
		if err != nil {
			return err
		}
		live, err := p.node(raw.ID)
		if err != nil {
			return err
		}
		exact := lang != "" && raw.FilePath == file
		if live != nil {
			exact = exact && live.ID == raw.ID && live.WorkspaceID == raw.WorkspaceID
		} else if exact {
			workspace, _, err := p.unique(p.key('W', reqType))
			if err != nil {
				return err
			}
			exact = workspace == "="+raw.WorkspaceID
		}
		return noteRebuildScratchState(p.put, r.key, r.value[0], rebuildScratchProofBit(exact))
	})
}

// A producer proof certifies the installed payload, not an original source.
// Repeating this stage on its own output still needs the raw source proof.
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
	callerState, err := p.state(p.key('N', e.From, call.member))
	if err != nil {
		return false, err
	}
	switch callerState {
	case rebuildCapabilityWanted:
		if caller != nil {
			workspace, _, err := p.unique(p.key('W', call.member))
			if err != nil || workspace != "="+caller.WorkspaceID {
				return false, err
			}
			global, err := p.state(p.candidateProofKey(p.key('a', call.group, call.member)))
			if err != nil || global == 0 || global&rebuildStoreCandidateExtra != 0 {
				return false, err
			}
		}
	case rebuildScratchExact:
	default:
		return false, nil
	}
	scoped := caller != nil || callerState == rebuildScratchExact
	ok := true
	err = p.handlers(ctx, prefix, func(id string) error {
		claim, err := p.marked(p.key('c', id))
		if err != nil {
			return err
		}
		ok = ok && len(claim) > 1 && rebuildStoreCandidateExact(claim[0]) &&
			(!scoped || claim[0]&rebuildStoreOtherWorkspace == 0)
		return nil
	})
	return ok, err
}

// Encode all proposals before moves. A handler belongs to one message type,
// so offers sharing a complete edge key have identical fanout payloads.
func (p *rebuildMediatRPass) prepareFanout(ctx context.Context) error {
	prefix := p.key('h')
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		e, err := p.decodeKeyedEdge(r.key[len(prefix):], r.value)
		if err != nil {
			return err
		}
		call := p.rules.call(e)
		posting, _, _, err := p.notification(e.From, call.member)
		if err != nil {
			return err
		}
		first := true
		return p.handlers(ctx, posting, func(id string) error {
			if first {
				first = false
				return nil
			}
			add := p.fanout(e, id, call.member)
			val, err := encodeEdge(add)
			if err != nil {
				return err
			}
			key := pendingEdgeKey(add)
			if err := p.input(key, val); err != nil {
				return err
			}
			return p.b.pending.Set(append(p.key('z'), key...), val, pebble.NoSync)
		})
	})
}

func (p *rebuildMediatRPass) applyFanout(ctx context.Context) error {
	return visitRebuildScratchKeyedEdges(ctx, p.b.pending, p.key('z'), 0, "MediatR fanout key", func(e *graph.Edge) error {
		return p.addFanoutIfAbsent(e, rebuildProofMediatRFanout)
	})
}

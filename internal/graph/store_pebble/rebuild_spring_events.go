package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"iter"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

var ErrRebuildSpringEvents = errors.New("pebble rebuild Spring events stage failed")

var rebuildSpringEventsPrefix = []byte{'g', 'q', 'S'}

// RebuildSpringEventRules supplies native metadata, type and payload decisions.
type RebuildSpringEventRules struct {
	Listener func(*graph.Node) string
	Event    func(*graph.Edge) string
	Simple   func(string) string
	Rebind   func(*graph.Edge, string, string, bool) bool
	Fanout   func(*graph.Edge, string, string) *graph.Edge
}

// RebuildSpringEventStats counts tagged Calls, native matches, moves and inserts.
type RebuildSpringEventStats struct{ Stubs, Resolved, Moves, Adds uint64 }

type rebuildSpringEventPass struct {
	rebuildCallMoves
	rules      RebuildSpringEventRules
	stats      RebuildSpringEventStats
	refused    rebuildScratchRefusals
	guard      bool
	guardProof bool
	entrants   bool
	hasAdds    bool
	calls      uint64
}

// RebuildSpringEvents runs once on the settled private graph. It proves move
// and add effects before writing either, then uses native move-before-add order.
func (b *BuildStore) RebuildSpringEvents(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	rules RebuildSpringEventRules,
) (stats RebuildSpringEventStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildSpringEventStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildSpringEvents, err)
		}
	}()
	if receipts == nil || rules.Listener == nil || rules.Event == nil || rules.Simple == nil || rules.Rebind == nil || rules.Fanout == nil {
		return stats, fmt.Errorf("%w: invalid Spring event inputs", ErrRebuildLinkInputSize)
	}
	prefix := rebuildSpringEventsPrefix
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return stats, err
	}
	if occupied {
		return stats, invalid("Spring event scratch range occupied")
	}
	defer func() { err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync)) }()
	p := rebuildSpringEventPass{
		rebuildCallMoves: rebuildCallMoves{
			b: b, prefix: prefix, rowBytes: 16 << 20, label: "Spring events", inputRow: "row",
		},
		rules: rules,
	}
	if err := p.captureListeners(ctx); err != nil {
		return p.stats, err
	}
	if err := p.captureEdges(ctx); err != nil || p.stats.Stubs == 0 {
		return p.stats, err
	}
	if err := p.planMoves(ctx); err != nil {
		return stats, err
	}
	if err := p.record(ctx, receipts); err != nil {
		return stats, err
	}
	if !p.guard {
		if p.entrants {
			return stats, p.proveListenerEntrants(ctx)
		}
		return stats, nil
	}
	if p.stats.Moves == 0 && !p.hasAdds && !p.entrants {
		return p.stats, nil
	}
	if err := p.proveMoves(ctx); err != nil {
		return stats, err
	}
	if err := p.planAdds(ctx); err != nil {
		return stats, err
	}
	err = b.withWrites(func() error {
		if err := p.apply(ctx, p.stats.Moves); err != nil {
			return err
		}
		return visitRebuildScratchKeyedEdges(ctx, b.pending, p.column('w'), 8, "Spring event fanout key", func(e *graph.Edge) error {
			// A fanout is not an original Call and must not borrow a receipt.
			return b.addEdgeWithCustody(e, rebuildEdgeCustody{proof: rebuildProofUntrackedMove})
		})
	})
	return p.stats, err
}

func (p *rebuildSpringEventPass) key(col byte, fields ...string) []byte {
	return keyStrings(p.column(col), fields...)
}

func (p *rebuildSpringEventPass) state(key []byte) (byte, error) {
	return rebuildScratchState(p.b.pending, key, "Spring event proof state")
}

func (p *rebuildSpringEventPass) note(key []byte, bit byte) error {
	state, err := p.state(key)
	if err != nil {
		return err
	}
	return noteRebuildScratchState(p.put, key, state, bit)
}

func springEventListenerKind(kind graph.NodeKind) bool {
	return kind == graph.KindMethod || kind == graph.KindFunction
}

// l indexes live listener types by ID. n records their receipt evidence.
// j indexes receipt-only types that could add listeners to a live cohort.
func (p *rebuildSpringEventPass) captureListeners(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		n, err := checkedRebuildLightNode(r, p.label)
		if err != nil || !springEventListenerKind(n.Kind) {
			return err
		}
		n, err = decodeNode(r.value)
		if err != nil {
			return err
		}
		t := p.rules.Listener(n)
		if t == "" {
			return nil
		}
		if n.ID == "" {
			return invalid("Spring event listener has no ID")
		}
		if err := p.input(r.key, r.value); err != nil {
			return err
		}
		p.guard = true
		if err := p.put(p.key('n', n.ID), []byte{rebuildCapabilityWanted}); err != nil {
			return err
		}
		return p.put(p.key('l', p.rules.Simple(t), n.ID), nil)
	})
}

// h indexes tagged Calls, k their raw keys. a holds the global simple-name
// arcs; e requests exact receipts and E maps raw keys to their live arcs.
func (p *rebuildSpringEventPass) captureEdges(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil {
			return err
		}
		if kind == graph.EdgeCalls {
			e, err := p.decodeKeyedEdge(r.key, r.value)
			if err != nil || p.rules.Event(e) == "" {
				return err
			}
			if err := p.input(r.key, r.value); err != nil {
				return err
			}
			p.stats.Stubs++
			if err := p.put(append(p.column('h'), r.key...), nil); err != nil {
				return err
			}
			raw, err := rebuildReceiptKey(snap, r.key)
			if errors.Is(err, ErrRebuildUnknownProvenance) {
				return nil // Report only if this Call can change an output.
			}
			if err != nil {
				return err
			}
			if err := p.put(append(p.column('k'), r.key...), raw); err != nil {
				return err
			}
			return p.claim(raw, r.key)
		}
		if kind != graph.EdgeExtends && kind != graph.EdgeImplements {
			return nil
		}
		e, err := decodeEdgeLight(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key, pendingEdgeKey(e)) {
			return invalid("Spring event hierarchy key")
		}
		child, parent := p.rules.Simple(e.From), p.rules.Simple(e.To)
		if child == "" || parent == "" || child == parent {
			return nil
		}
		arc := p.key('a', child, parent)
		if err := p.put(arc, nil); err != nil {
			return err
		}
		return p.captureArcProof(snap, r.key, arc[len(p.prefix)+1:], kind)
	})
}

func (p *rebuildSpringEventPass) captureArcProof(snap *pebble.Snapshot, key, arc []byte, kind graph.EdgeKind) error {
	if kind == graph.EdgeImplements {
		proved, err := rebuildImplementsStamped(snap, key)
		if err != nil {
			return err
		}
		if proved {
			return p.put(append(p.column('A'), arc...), nil)
		}
	}
	custody, err := readRebuildEdgeCustody(snap, key)
	if errors.Is(err, ErrRebuildUnknownProvenance) {
		return nil
	}
	if err != nil {
		return err
	}
	if custody.proof != 0 {
		if kind == graph.EdgeImplements && custody.proof == rebuildProofKMPExpectActual {
			return p.put(append(p.column('A'), arc...), nil)
		}
		return nil
	}
	raw := custody.raw
	if raw == nil {
		raw = key
	}
	id, err := decodeLinkRecorder(raw)
	if err != nil || id.Kind != kind {
		return err
	}
	if err := p.put(append(p.key('E', string(raw)), arc...), nil); err != nil {
		return err
	}
	return p.put(append(p.column('e'), raw...), []byte{rebuildCapabilityWanted})
}

func (p *rebuildSpringEventPass) record(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := p.recordListener(n, file, facts.EffectiveLanguage); err != nil {
				return err
			}
		}
		for _, raw := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			valid := facts.EffectiveLanguage != "" && raw.FilePath == file
			switch raw.Kind {
			case graph.EdgeCalls:
				if err := p.recordCall(raw, valid); err != nil {
					return err
				}
			case graph.EdgeExtends, graph.EdgeImplements:
				key := append(p.column('e'), pendingEdgeKey(raw)...)
				state, err := p.state(key)
				if err != nil {
					return err
				}
				if state != 0 {
					if err := p.note(key, rebuildScratchProofBit(valid)); err != nil {
						return err
					}
				}
			}
		}
	}
	return pendingVisit(ctx, p.b.pending, p.column('E'), func(r row) error {
		raw, arc, err := readKeyString(r.key[len(p.prefix)+1:])
		if err != nil {
			return err
		}
		state, err := p.state(append(p.column('e'), raw...))
		if err != nil || state != rebuildScratchExact {
			return err
		}
		return p.put(append(p.column('A'), arc...), nil)
	})
}

func (p *rebuildSpringEventPass) recordListener(raw *graph.Node, file, lang string) error {
	t := p.rules.Listener(raw)
	role := raw.ID != "" && t != "" && springEventListenerKind(raw.Kind)
	cohort := p.rules.Simple(t)
	key := p.key('n', raw.ID)
	state, err := p.state(key)
	if err != nil || state == 0 && !role {
		return err
	}
	present, err := p.present(p.key('l', cohort, raw.ID))
	if err != nil {
		return err
	}
	if role && !present {
		// An entrant is not an exact proof, even if this is an admitted receipt.
		p.entrants = true
		if err := p.put(p.key('j', cohort, raw.ID), nil); err != nil {
			return err
		}
	}
	if state == 0 {
		return nil
	}
	live, err := pendingGet(p.b.pending, pendingNodeKey(raw.ID), decodeNodeLight)
	if err != nil {
		return err
	}
	valid := role && present && lang != "" &&
		sameRebuildLanguageMarker(raw, live, file)
	return p.note(key, rebuildScratchProofBit(valid))
}

func (p *rebuildSpringEventPass) recordCall(raw *graph.Edge, valid bool) error {
	key := append(p.column('r'), pendingEdgeKey(raw)...)
	claim, err := p.marked(key)
	if err != nil || len(claim) <= 1 {
		return err
	}
	live, err := p.call(claim[1:])
	if err != nil {
		return err
	}
	exact := valid && p.rules.Event(raw) == p.rules.Event(live)
	if exact && claim[0] == rebuildCapabilityWanted {
		if err := p.put(binary.BigEndian.AppendUint64(p.column('f'), p.calls), claim[1:]); err != nil {
			return err
		}
		p.calls++
	}
	return p.noteReceipt(key, claim, exact)
}

func (p *rebuildSpringEventPass) call(old []byte) (*graph.Edge, error) {
	e, err := p.edge(old)
	if err == nil && e == nil {
		err = invalid("Spring event Call missing")
	}
	return e, err
}

func (p *rebuildSpringEventPass) callProved(old []byte) (bool, error) {
	raw, err := pendingGet(p.b.pending, append(p.column('k'), old...), copyValue)
	if err != nil || raw == nil {
		return false, err
	}
	claim, err := p.marked(append(p.column('r'), raw...))
	return len(claim) > 1 && claim[0] == rebuildScratchExact && bytes.Equal(claim[1:], old), err
}

// Each requested event type is walked once per evidence set. The queue and
// visited set stay in Pebble, as do the final listener postings sorted by ID.
// A proved path need not use the same arc as another path to that listener.
func (p *rebuildSpringEventPass) listeners(ctx context.Context, root string, result byte) ([]byte, string, error) {
	arcs, listeners := byte('a'), byte('l')
	switch result {
	case 'T':
		arcs = 'A'
	case 'J':
		listeners = 'j'
	}
	out := p.key(result, root)
	done := p.key('c', string(result), root)
	cached, err := pendingGet(p.b.pending, done, copyValue)
	if err != nil {
		return nil, "", err
	}
	if cached != nil {
		return out, string(cached[1:]), nil
	}
	queue, visited := p.column('q'), p.column('v')
	var first string
	var count uint64
	enqueue := func(t string) error {
		key := p.key('v', t)
		seen, err := p.present(key)
		if err != nil || seen {
			return err
		}
		if err := p.put(key, nil); err != nil {
			return err
		}
		if err := p.put(binary.BigEndian.AppendUint64(p.column('q'), count), []byte(t)); err != nil {
			return err
		}
		count++
		return nil
	}
	if err := enqueue(root); err != nil {
		return nil, "", err
	}
	for i := uint64(0); i < count; i++ {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		t, err := pendingGet(p.b.pending, binary.BigEndian.AppendUint64(p.column('q'), i), copyValue)
		if err != nil {
			return nil, "", err
		}
		parents := p.key(arcs, string(t))
		if err := pendingVisit(ctx, p.b.pending, parents, func(r row) error {
			parent, _, err := readKeyString(r.key[len(parents):])
			if err != nil {
				return err
			}
			return enqueue(parent)
		}); err != nil {
			return nil, "", err
		}
		posting := p.key(listeners, string(t))
		if err := pendingVisit(ctx, p.b.pending, posting, func(r row) error {
			id, _, err := readKeyString(r.key[len(posting):])
			if err != nil {
				return err
			}
			if result == 'T' {
				state, err := p.state(p.key('n', id))
				if err != nil || state != rebuildScratchExact {
					return err
				}
			}
			if first == "" || id < first {
				first = id
			}
			return p.put(append(bytes.Clone(out), r.key[len(posting):]...), nil)
		}); err != nil {
			return nil, "", err
		}
	}
	for _, prefix := range [...][]byte{queue, visited} {
		if err := p.b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync); err != nil {
			return nil, "", err
		}
	}
	return out, first, p.put(done, append([]byte{1}, first...))
}

func (p *rebuildSpringEventPass) visitCalls(ctx context.Context, visit func([]byte, *graph.Edge) error) error {
	prefix := p.column('h')
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		old := r.key[len(prefix):]
		e, err := p.call(old)
		if err != nil {
			return err
		}
		return visit(old, e)
	})
}

func (p *rebuildSpringEventPass) planMoves(ctx context.Context) error {
	return p.visitCalls(ctx, func(old []byte, e *graph.Edge) error {
		t := p.rules.Event(e)
		posting, first, err := p.listeners(ctx, p.rules.Simple(t), 't')
		if err != nil {
			return err
		}
		var count uint64
		if err := pendingVisit(ctx, p.b.pending, posting, func(row) error {
			count++
			return nil
		}); err != nil {
			return err
		}
		p.stats.Resolved += count
		p.hasAdds = p.hasAdds || count > 1
		if !p.guard || !p.rules.Rebind(e, t, first, count > 0) {
			return nil
		}
		p.stats.Moves++
		return p.stage(old, e)
	})
}

func (p *rebuildSpringEventPass) proveMoves(ctx context.Context) error {
	if err := pendingVisit(ctx, p.b.pending, p.column('n'), func(r row) error {
		p.guardProof = p.guardProof || len(r.value) == 1 && r.value[0] == rebuildScratchExact
		return nil
	}); err != nil {
		return err
	}
	if err := p.moves(ctx, func(old []byte, e *graph.Edge) error {
		ok, err := p.callProved(old)
		if err != nil {
			return err
		}
		if ok && graph.IsUnresolvedTarget(e.To) {
			// No live listener is reachable. Only the native global guard
			// needs a witness; a chain that leads nowhere changes no output.
			ok = p.guardProof
		} else if ok {
			_, first, err := p.listeners(ctx, p.rules.Simple(p.rules.Event(e)), 'T')
			if err != nil {
				return err
			}
			ok = first == e.To
		}
		if !ok {
			p.refused.add(graph.EdgeIdentityFor(e), pendingEdgeKey(e))
		}
		return nil
	}); err != nil {
		return err
	}
	return p.refused.err()
}

// Simulate only the post-move occupancy, using the same windows as apply.
// An occupied key suppresses every add at that key, even unproved offers.
func (p *rebuildSpringEventPass) planAdds(ctx context.Context) error {
	if !p.hasAdds && !p.entrants {
		return nil
	}
	for i := uint64(0); i < p.ordered; i += graph.SQLiteReindexChunkSize {
		if err := p.visitWindow(ctx, i, func(old, _ []byte) error {
			return p.put(append(p.column('z'), old...), []byte{1})
		}); err != nil {
			return err
		}
		if err := p.visitWindow(ctx, i, func(old, _ []byte) error {
			e, err := pendingGet(p.b.pending, append(p.column('m'), old...), decodeEdge)
			if err != nil {
				return err
			}
			if e == nil {
				return invalid("Spring event planned move missing")
			}
			if graph.StructuralEdgeTargetInvalid(e.Kind, e.To) {
				return nil
			}
			return p.put(append(p.column('z'), pendingEdgeKey(e)...), []byte{2})
		}); err != nil {
			return err
		}
	}
	if p.entrants {
		if err := p.proveListenerEntrants(ctx); err != nil {
			return err
		}
	}
	if !p.hasAdds {
		return p.refused.err()
	}
	if err := pendingVisit(ctx, p.b.pending, p.column('f'), func(r row) error {
		proved, err := p.callProved(r.value)
		if err != nil || !proved {
			return err
		}
		e, err := p.call(r.value)
		if err != nil {
			return err
		}
		return p.offerAdds(ctx, e, true)
	}); err != nil {
		return err
	}
	if err := p.visitCalls(ctx, func(old []byte, e *graph.Edge) error {
		proved, err := p.callProved(old)
		if err != nil || proved {
			return err
		}
		return p.offerAdds(ctx, e, false)
	}); err != nil {
		return err
	}
	return p.refused.err()
}

func (p *rebuildSpringEventPass) proveListenerEntrants(ctx context.Context) error {
	if err := p.visitCalls(ctx, func(_ []byte, e *graph.Edge) error {
		return p.checkListenerEntrants(ctx, e)
	}); err != nil {
		return err
	}
	return p.refused.err()
}

func (p *rebuildSpringEventPass) checkListenerEntrants(ctx context.Context, e *graph.Edge) error {
	t := p.rules.Event(e)
	root := p.rules.Simple(t)
	live, first, err := p.listeners(ctx, root, 't')
	if err != nil {
		return err
	}
	entrants, earlier, err := p.listeners(ctx, root, 'J')
	if err != nil {
		return err
	}
	if !p.guard && earlier == "" {
		// A receipt-only listener can activate resets even for unrelated events.
		reset := *e
		if p.rules.Rebind(&reset, t, "", false) {
			p.refused.add(graph.EdgeIdentityFor(&reset), pendingEdgeKey(&reset))
		}
		return nil
	}
	return pendingVisit(ctx, p.b.pending, entrants, func(r row) error {
		suffix := r.key[len(entrants):]
		id, _, err := readKeyString(suffix)
		if err != nil {
			return err
		}
		known, err := p.present(append(bytes.Clone(live), suffix...))
		if err != nil || known {
			return err
		}
		if first == "" || id < first {
			changed := *e
			if p.rules.Rebind(&changed, t, id, true) || p.guard {
				p.refused.add(graph.EdgeIdentityFor(&changed), pendingEdgeKey(&changed))
			}
			return nil
		}
		add := p.rules.Fanout(e, t, id)
		key := pendingEdgeKey(add)
		occupied, err := p.addKeyOccupied(key)
		if err != nil || occupied {
			return err
		}
		p.refused.add(graph.EdgeIdentityFor(add), key)
		return nil
	})
}

func (p *rebuildSpringEventPass) addKeyOccupied(key []byte) (bool, error) {
	state, err := p.state(append(p.column('z'), key...))
	if err != nil || state != 0 {
		return state == 2, err
	}
	return p.present(key)
}

func (p *rebuildSpringEventPass) offerAdds(ctx context.Context, e *graph.Edge, proved bool) error {
	t := p.rules.Event(e)
	root := p.rules.Simple(t)
	posting, first, err := p.listeners(ctx, root, 't')
	if err != nil {
		return err
	}
	return pendingVisit(ctx, p.b.pending, posting, func(r row) error {
		id, _, err := readKeyString(r.key[len(posting):])
		if err != nil {
			return err
		}
		if id == first {
			return nil
		}
		add := p.rules.Fanout(e, t, id)
		key := pendingEdgeKey(add)
		occupied, err := p.addKeyOccupied(key)
		if err != nil || occupied {
			return err
		}
		winnerKey := append(p.column('d'), key...)
		winner, err := pendingGet(p.b.pending, winnerKey, copyValue)
		if err != nil {
			return err
		}
		if winner != nil && (proved || bytes.Equal(winner, []byte(t))) {
			return nil
		}
		valid := proved
		if valid {
			proof, earlier, err := p.listeners(ctx, root, 'T')
			if err != nil {
				return err
			}
			// Any proved preceding listener is enough to make this an add.
			valid, err = p.present(appendKeyString(proof, id))
			if err != nil {
				return err
			}
			valid = valid && earlier != "" && earlier < id
		}
		if !valid {
			p.refused.add(graph.EdgeIdentityFor(add), key)
			return nil
		}
		raw, err := encodeEdge(add)
		if err != nil {
			return err
		}
		if err := p.put(winnerKey, []byte(t)); err != nil {
			return err
		}
		order := binary.BigEndian.AppendUint64(p.column('w'), p.stats.Adds)
		p.stats.Adds++
		return p.put(append(order, key...), raw)
	})
}

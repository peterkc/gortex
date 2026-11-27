package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"iter"
	"slices"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

// Only different receipt roles need stored rows. Exact surviving roles use the
// ordinary claim proof. Discover alternate names before electing registrations.
func (p *rebuildFnPointerStage) captureReceiptRoles(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		if facts.EffectiveLanguage == "" {
			continue
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, e := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			if e.FilePath != file || e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeReferences {
				continue
			}
			if err := p.captureReceiptRole(e); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *rebuildFnPointerStage) captureReceiptRole(raw *graph.Edge) error {
	moves := &p.rebuildCallMoves
	if raw.Kind == graph.EdgeCalls {
		moves = &p.calls
	}
	key := pendingEdgeKey(raw)
	claim, err := moves.marked(append(moves.column('r'), key...))
	if err != nil || len(claim) == 1 {
		return err // ambiguous raw lineage is not a receipt-only role
	}
	old := key
	if len(claim) > 1 {
		old = claim[1:]
	} else if raw.Kind == graph.EdgeReferences {
		if _, source, name := p.decide.Registration(raw); source == "" && name == "" {
			return nil
		}
	} else if _, _, ok := p.decide.Dispatch(raw); !ok {
		return nil
	}
	live, err := moves.edge(old)
	if err != nil || live == nil {
		return err // a missing frozen key is stale, not an entrant
	}
	if len(claim) == 0 {
		custody, err := rebuildReceiptKey(p.b.pending, old)
		if errors.Is(err, ErrRebuildUnknownProvenance) {
			return nil
		}
		if err != nil || !bytes.Equal(custody, key) {
			return err
		}
	}
	if raw.Kind == graph.EdgeReferences {
		slot, source, name := p.decide.Registration(raw)
		liveSlot, liveSource, liveName := p.decide.Registration(live)
		if source == "" && name == "" && liveSource == "" && liveName == "" ||
			slot == liveSlot && source == liveSource && name == liveName &&
			(name == "" || fnPointerRegistrationCall(raw, name) == fnPointerRegistrationCall(live, liveName)) {
			return nil
		}
		if name != "" {
			if err := p.put(p.key('q', name), nil); err != nil {
				return err
			}
		}
	} else {
		st, field, ok := p.decide.Dispatch(raw)
		liveST, liveField, liveOK := p.decide.Dispatch(live)
		if !ok && !liveOK || ok == liveOK && st == liveST && field == liveField {
			return nil
		}
	}
	// A paired receipt explains a role at the surviving exact key. Its old
	// raw destination is not the current destination or copy tuple ordinal.
	e := *raw
	e.To = live.To
	value, err := encodeEdge(&e)
	if err != nil {
		return err
	}
	p.alternate = true
	return moves.put(append(moves.column('A'), old...), value)
}

// Replay the same ordered copy reducer for the receipt roles, without adding
// receipt-only rows to the live plan. Unknown input is not a receipt seed.
func (p *rebuildFnPointerStage) planReceiptSlots(ctx context.Context) error {
	prefix := p.column('h')
	if err := p.visit(ctx, prefix, func(r row) error {
		old := r.key[len(prefix):]
		alternate, err := p.present(append(p.column('A'), old...))
		if err != nil || alternate {
			return err
		}
		e, err := p.edge(old)
		if err != nil || e == nil {
			return errors.Join(err, invalid("function pointer registration missing"))
		}
		proved, err := fnPointerInputProved(&p.rebuildCallMoves, old, r.value)
		if err != nil {
			return err
		}
		return p.receiptRegistration(old, e, proved, false)
	}); err != nil {
		return err
	}
	if err := p.visitRegistrations(ctx, 'A', func(old []byte, e *graph.Edge) error {
		return p.receiptRegistration(old, e, true, true)
	}); err != nil {
		return err
	}
	var err error
	p.rawSeeded, err = rebuildGuardHasPrefix(ctx, p.b.pending, p.column('Y'))
	if err != nil {
		return err
	}
	return p.propagate(ctx, 'Z', 'Y', 'V')
}

func (p *rebuildFnPointerStage) receiptRegistration(old []byte, e *graph.Edge, proved, alternate bool) error {
	slot, source, name := p.decide.Registration(e)
	switch {
	case name != "":
		call := fnPointerRegistrationCall(e, name)
		pick, err := p.elect(e.From, call, true)
		if err != nil {
			return err
		}
		if proved {
			proved, err = p.registrationProved(e, name)
			if err != nil {
				return err
			}
		}
		if alternate && pick.target != "" && p.decide.BindRef(e, pick.target) {
			planned, err := pendingGet(p.b.pending, append(p.column('m'), old...), decodeEdge)
			if err != nil {
				return err
			}
			raw, err := p.get(append(p.column('h'), old...))
			if err != nil {
				return err
			}
			exact, err := fnPointerInputProved(&p.rebuildCallMoves, old, raw)
			if err != nil {
				return err
			}
			// A singleton never reads store_binding. Its exact claim and
			// matching live move already prove the alternate destination.
			if !proved || !exact || planned == nil || planned.To != e.To {
				if err := p.refuseOutput(e); err != nil {
					return err
				}
			}
		}
		if proved {
			if pick.target == "" {
				return nil
			}
			return p.put(p.key('Y', slot, pick.target), nil)
		}
		p.rawMaySeed = true
	case source != "" && source != slot:
		// Even an unproved live copy carries the uncertainty of its
		// source. Do not propagate its members as exact receipt facts.
		var exact byte
		if proved {
			exact = 1
		}
		tuple := appendKeyLine(p.key('Z', e.From, string(e.Kind), e.To, e.FilePath), e.Line)
		return p.put(tuple, keyStrings([]byte{exact}, slot, source))
	default:
		return nil
	}
	_, err := p.noteDependency('V', slot)
	return err
}

func (p *rebuildFnPointerStage) checkReceiptDispatch(ctx context.Context) error {
	for _, col := range [...]byte{'h', 'A'} {
		prefix := p.calls.column(col)
		if err := p.visit(ctx, prefix, func(r row) error {
			old := r.key[len(prefix):]
			live, err := p.calls.edge(old)
			if err != nil || live == nil {
				return errors.Join(err, invalid("function pointer call missing"))
			}
			raw := live
			if col == 'A' {
				admitted, err := p.calls.present(append(p.calls.column('h'), old...))
				if err != nil || admitted {
					return err // compare each surviving Call once
				}
				raw, err = decodeEdge(r.value)
				if err != nil {
					return err
				}
			} else {
				rawAlt, err := pendingGet(p.b.pending, append(p.calls.column('A'), old...), decodeEdge)
				if err != nil {
					return err
				}
				if rawAlt != nil {
					raw = rawAlt
				}
			}
			return p.compareDispatch(ctx, live, raw)
		}); err != nil {
			return err
		}
	}
	return nil
}

type rebuildFnPointerDispatch struct {
	primary *graph.Edge
	ids     []string
}

func (p *rebuildFnPointerStage) receiptDispatch(ctx context.Context, e *graph.Edge, col byte, seeded, possible bool) (rebuildFnPointerDispatch, error) {
	st, field, ok := p.decide.Dispatch(e)
	if !ok || !seeded {
		return rebuildFnPointerDispatch{}, nil
	}
	ids, err := p.dispatchTargets(ctx, col, st, field)
	if err != nil {
		return rebuildFnPointerDispatch{}, err
	}
	if possible {
		maybe, err := p.dispatchTargets(ctx, 'T', st, field)
		if err != nil {
			return rebuildFnPointerDispatch{}, err
		}
		if len(maybe) != 0 {
			ids = append(ids, maybe...)
			slices.Sort(ids)
			ids = slices.Compact(ids)
			if len(ids) > p.decide.Cap {
				ids = ids[:p.decide.Cap]
			}
		}
	}
	var first string
	if len(ids) != 0 {
		first = ids[0]
	}
	moved := *e
	var primary *graph.Edge
	if p.decide.BindCall(&moved, first, len(ids) != 0) {
		primary = &moved
	}
	return rebuildFnPointerDispatch{primary, ids}, nil
}

func (p *rebuildFnPointerStage) compareDispatch(ctx context.Context, live, raw *graph.Edge) error {
	before, err := p.receiptDispatch(ctx, live, 'y', p.liveSeeded, false)
	if err != nil {
		return err
	}
	after, err := p.receiptDispatch(ctx, raw, 'Y', p.rawSeeded, false)
	if err != nil {
		return err
	}
	if err := p.compareDispatchEffects(live, raw, before, after); err != nil {
		return err
	}
	st, field, ok := p.decide.Dispatch(raw)
	if !ok {
		return nil
	}
	unknown, err := p.present(p.key('V', st+"\x00"+field))
	if err != nil {
		return err
	}
	reset := *raw
	if unknown || !p.rawSeeded && p.rawMaySeed && p.decide.BindCall(&reset, "", false) {
		primary := before.primary
		if primary == nil {
			primary = after.primary
		}
		if primary == nil {
			primary = live
		}
		return p.refuseOutput(primary)
	}
	possible, err := p.receiptDispatch(ctx, raw, 'Y', p.rawSeeded, true)
	if err != nil {
		return err
	}
	return p.compareDispatchEffects(live, raw, before, possible)
}

func (p *rebuildFnPointerStage) compareDispatchEffects(live, raw *graph.Edge, before, after rebuildFnPointerDispatch) error {
	switch {
	case before.primary == nil && after.primary == nil:
	case before.primary == nil || after.primary == nil || before.primary.To != after.primary.To:
		primary := before.primary
		if primary == nil {
			primary = after.primary
		}
		if err := p.refuseOutput(primary); err != nil {
			return err
		}
	}
	beforeST, beforeField, _ := p.decide.Dispatch(live)
	afterST, afterField, _ := p.decide.Dispatch(raw)
	for _, row := range [...]struct {
		e         *graph.Edge
		st, field string
		from, to  []string
	}{
		{live, beforeST, beforeField, before.ids, after.ids},
		{raw, afterST, afterField, after.ids, before.ids},
	} {
		if len(row.from) <= 1 {
			continue
		}
		for _, target := range row.from[1:] {
			if len(row.to) > 1 && slices.Contains(row.to[1:], target) && beforeST == afterST && beforeField == afterField {
				continue
			}
			e := *row.e
			e.To = target
			if err := p.refuseFanout(&e); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *rebuildFnPointerStage) refuseFanout(e *graph.Edge) error {
	key := pendingEdgeKey(e)
	occupied, err := p.calls.present(key)
	if err != nil {
		return err
	}
	if occupied {
		vacated, err := p.calls.present(append(p.calls.column('m'), key...))
		if err != nil || !vacated {
			return err
		}
	}
	return p.refuseOutput(e)
}

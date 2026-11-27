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
	"github.com/zzet/gortex/internal/pathkey"
)

var ErrRebuildFnPointer = errors.New("pebble rebuild function pointer stage failed")

var rebuildFnPointerStagePrefix = []byte{'g', 'q', 'P'}

const rebuildFnPointerRowBytes = 16 << 20

// RebuildFnPointerStageRules supplies the native registration and dispatch decisions.
type RebuildFnPointerStageRules struct {
	Registration func(*graph.Edge) (slot, source, name string)
	Dispatch     func(*graph.Edge) (st, field string, ok bool)
	Callable     func(*graph.Node) bool
	Getter       func(*graph.Node, string) bool
	BindRef      func(*graph.Edge, string) bool
	BindCall     func(*graph.Edge, string, bool) bool
	Fanout       func(*graph.Edge, string, string, string) *graph.Edge
	Sweeps, Cap  int
}

// RebuildFnPointerStageStats describes the ordered reduction, not saved row counts.
type RebuildFnPointerStageStats struct {
	RefMoves, CallMoves, Adds, Resolved uint64
	Sweeps                              int
}

type rebuildFnPointerStage struct {
	rebuildStoreFactoryPass
	calls        rebuildCallMoves
	decide       RebuildFnPointerStageRules
	result       RebuildFnPointerStageStats
	refused      rebuildScratchRefusals
	dependencies rebuildScratchRefusals
	adds         uint64
}

func rebuildFnPointerStageColumns(b *BuildStore) rebuildCallMoves {
	return rebuildCallMoves{b: b, prefix: rebuildFnPointerStagePrefix, rowBytes: rebuildFnPointerRowBytes,
		label: "function pointer", inputRow: "edge row"}
}

// FreezeFnPointerInputs retains only identities from the native pre-loop
// census. The stage refetches the live payload at each surviving key.
func (b *BuildStore) FreezeFnPointerInputs(ctx context.Context, match func(*graph.Edge) bool) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("%w: %w", ErrRebuildFnPointer, err)
		}
	}()
	p := rebuildFnPointerStageColumns(b)
	busy, err := rebuildGuardHasPrefix(ctx, b.pending, p.prefix)
	if err != nil {
		return err
	}
	if busy || match == nil {
		return invalid("function pointer census inputs")
	}
	if err := b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(_ *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || kind != graph.EdgeCalls && kind != graph.EdgeReferences {
			return err
		}
		e, err := p.decodeKeyedEdge(r.key, r.value)
		if err != nil || !match(e) {
			return err
		}
		return p.put(append(p.column('i'), r.key...), nil)
	}); err != nil {
		return errors.Join(err, b.pending.DeleteRange(p.prefix, prefixEnd(p.prefix), pebble.NoSync))
	}
	return p.put(p.column('I'), nil)
}

// RebuildFnPointerStage resolves the frozen inputs at framework position #30.
func (b *BuildStore) RebuildFnPointerStage(ctx context.Context, receipts iter.Seq2[ParseFacts, error], rules RebuildFnPointerStageRules) (stats RebuildFnPointerStageStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildFnPointerStageStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildFnPointer, err)
		}
	}()
	cols := rebuildFnPointerStageColumns(b)
	frozen, err := cols.present(cols.column('I'))
	if err != nil {
		return stats, err
	}
	if !frozen {
		return stats, invalid("function pointer census missing")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(cols.prefix, prefixEnd(cols.prefix), pebble.NoSync))
	}()
	if rules.Registration == nil || rules.Dispatch == nil || rules.Callable == nil || rules.Getter == nil ||
		rules.BindRef == nil || rules.BindCall == nil || rules.Fanout == nil || rules.Sweeps < 1 || rules.Cap < 1 || receipts == nil {
		return stats, invalid("function pointer stage inputs")
	}
	calls := cols
	calls.prefix = cols.column('D')
	p := rebuildFnPointerStage{
		rebuildStoreFactoryPass: rebuildStoreFactoryPass{
			rebuildCallMoves: cols,
			rules: rebuildStoreDispatchRules{
				kind: func(kind graph.NodeKind) bool { return kind == graph.KindFunction || kind == graph.KindMethod },
				action: func(n *graph.Node) (string, string) {
					if rules.Callable(n) && n.Name != "" {
						return "fn", n.Name
					}
					return "", ""
				},
				getter: rules.Getter,
			},
		},
		calls:  calls,
		decide: rules,
	}
	if err := p.capture(ctx); err != nil {
		return stats, err
	}
	wanted, err := rebuildGuardHasPrefix(ctx, b.pending, p.column('q'))
	if err != nil || !wanted {
		return stats, err
	}
	if err := p.collectCandidates(ctx); err != nil {
		return stats, err
	}
	if err := p.planRegistrations(ctx); err != nil {
		return stats, err
	}
	seeded, err := rebuildGuardHasPrefix(ctx, b.pending, p.column('y'))
	if err != nil || !seeded {
		return p.result, err
	}
	if err := p.record(ctx, receipts); err != nil {
		return stats, err
	}
	if err := p.finishRegistrations(ctx); err != nil {
		return stats, err
	}
	if err := p.propagate(ctx); err != nil {
		return stats, err
	}
	if err := p.planDispatch(ctx); err != nil {
		return stats, err
	}
	if err := p.refused.err(); err != nil {
		return stats, err
	}
	if p.result.CallMoves != 0 || p.result.Adds != 0 {
		if err := p.dependencies.err(); err != nil {
			return stats, err
		}
	}
	return p.result, p.write(ctx)
}

func (p *rebuildFnPointerStage) visit(ctx context.Context, prefix []byte, visit func(row) error) error {
	return pendingVisit(ctx, p.b.pending, prefix, visit)
}

func (p *rebuildFnPointerStage) capture(ctx context.Context) error {
	prefix := p.column('i')
	return p.visit(ctx, prefix, func(r row) error {
		old := r.key[len(prefix):]
		e, err := p.edge(old)
		if err != nil || e == nil {
			return err
		}
		moves := &p.rebuildCallMoves
		col := byte('h')
		if e.Kind == graph.EdgeReferences {
			_, source, name := p.decide.Registration(e)
			if source == "" && name == "" {
				return nil
			}
			if name != "" {
				if err := p.put(p.key('q', name), nil); err != nil {
					return err
				}
			}
		} else {
			if _, _, ok := p.decide.Dispatch(e); !ok {
				return nil
			}
			moves = &p.calls
		}
		raw, err := rebuildReceiptKey(p.b.pending, old)
		if err != nil && !errors.Is(err, ErrRebuildUnknownProvenance) {
			return err
		}
		if raw != nil {
			id, err := decodeLinkRecorder(raw)
			if err != nil {
				return err
			}
			if id.Kind != e.Kind || id.From != e.From || id.FilePath != e.FilePath || id.Line != e.Line {
				raw = nil
			} else if err := moves.claim(raw, old); err != nil {
				return err
			}
		}
		return moves.put(append(moves.column(col), old...), raw)
	})
}

func (p *rebuildFnPointerStage) collectCandidates(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		n, err := checkedRebuildLightNode(r, p.label)
		if err != nil || !p.decide.Callable(n) || n.Name == "" {
			return err
		}
		wanted, err := p.present(p.key('q', n.Name))
		if err != nil || !wanted {
			return err
		}
		if err := p.nodeInput(r.key, r.value); err != nil {
			return err
		}
		cohort := p.key('u', "fn", n.Name)
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

func fnPointerRegistrationCall(e *graph.Edge, name string) rebuildStoreDispatchCall {
	binding, _ := e.Meta["store_binding"].(string)
	return rebuildStoreDispatchCall{group: "fn", member: name, getter: binding}
}

func (p *rebuildFnPointerStage) planRegistrations(ctx context.Context) error {
	prefix := p.column('h')
	if err := p.visit(ctx, prefix, func(r row) error {
		e, err := p.edge(r.key[len(prefix):])
		if err != nil || e == nil {
			return errors.Join(err, invalid("function pointer registration missing"))
		}
		_, _, name := p.decide.Registration(e)
		if name == "" {
			return nil
		}
		call := fnPointerRegistrationCall(e, name)
		pick, err := p.elect(e.From, call, false)
		if err != nil || !pick.getter {
			return err
		}
		return p.want(p.key('b', call.getter))
	}); err != nil {
		return err
	}
	getters, err := rebuildGuardHasPrefix(ctx, p.b.pending, p.column('b'))
	if err != nil {
		return err
	}
	if getters {
		if err := p.collectGetters(ctx); err != nil {
			return err
		}
	}
	return p.visit(ctx, prefix, func(r row) error {
		old := r.key[len(prefix):]
		e, err := p.edge(old)
		if err != nil || e == nil {
			return errors.Join(err, invalid("function pointer registration missing"))
		}
		slot, _, name := p.decide.Registration(e)
		if name == "" {
			return nil
		}
		call := fnPointerRegistrationCall(e, name)
		pick, err := p.elect(e.From, call, true)
		if err != nil {
			return err
		}
		if err := p.want(p.key('u', call.group, call.member)); err != nil {
			return err
		}
		if pick.getter {
			if err := p.want(p.key('v', call.getter)); err != nil {
				return err
			}
		}
		if pick.caller != nil {
			if err := p.want(p.key('n', pick.caller.ID)); err != nil {
				return err
			}
		}
		if err := p.wantCandidateBuckets(pick.buckets); err != nil || pick.target == "" {
			return err
		}
		if err := p.put(p.key('y', slot, pick.target), nil); err != nil {
			return err
		}
		if !p.decide.BindRef(e, pick.target) {
			return nil
		}
		p.result.RefMoves++
		return p.stage(old, e)
	})
}

func (p *rebuildFnPointerStage) record(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	var ordinal uint64
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file, lang := pathkey.Normalize(facts.File.FilePath), facts.EffectiveLanguage
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := p.recordNode(n, file, lang); err != nil {
				return err
			}
		}
		for _, e := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			if e.Kind == graph.EdgeCalls || e.Kind == graph.EdgeReferences {
				if err := p.recordInput(e, file, lang, ordinal); err != nil {
					return err
				}
			}
			ordinal++
		}
	}
	if _, err := p.proveActions(ctx); err != nil {
		return err
	}
	return p.foldGetterFiles(ctx, p.key('g'), p.node, p.addUnique)
}

func (p *rebuildFnPointerStage) recordInput(e *graph.Edge, file, lang string, ordinal uint64) error {
	moves := &p.rebuildCallMoves
	if e.Kind == graph.EdgeCalls {
		moves = &p.calls
	}
	raw := pendingEdgeKey(e)
	key := append(moves.column('r'), raw...)
	claim, err := moves.marked(key)
	if err != nil || len(claim) <= 1 {
		return err
	}
	live, err := moves.edge(claim[1:])
	if err != nil || live == nil {
		return errors.Join(err, invalid("function pointer receipt has no live row"))
	}
	exact := lang != "" && e.FilePath == file
	if e.Kind == graph.EdgeReferences {
		a, b, c := p.decide.Registration(e)
		x, y, z := p.decide.Registration(live)
		exact = exact && a == x && b == y && c == z &&
			fnPointerRegistrationCall(e, c) == fnPointerRegistrationCall(live, z)
		return moves.noteReceipt(key, claim, exact)
	}
	a, b, ok := p.decide.Dispatch(e)
	x, y, _ := p.decide.Dispatch(live)
	exact = exact && ok && a == x && b == y
	if exact && claim[0] == rebuildCapabilityWanted {
		order := binary.BigEndian.AppendUint64(moves.column('z'), ordinal)
		if err := moves.put(order, raw); err != nil {
			return err
		}
	}
	return noteRebuildScratchPayload(moves.put, key, claim, rebuildScratchProofBit(exact))
}

func fnPointerInputProved(moves *rebuildCallMoves, old, raw []byte) (bool, error) {
	if len(raw) == 0 {
		return false, nil
	}
	claim, err := moves.marked(append(moves.column('r'), raw...))
	return len(claim) > 1 && claim[0] == rebuildScratchExact && bytes.Equal(claim[1:], old), err
}

func (p *rebuildFnPointerStage) registrationProved(e *graph.Edge, name string) (bool, error) {
	call := fnPointerRegistrationCall(e, name)
	pick, err := p.elect(e.From, call, true)
	if err != nil {
		return false, err
	}
	if ok, err := p.candidateBucketsProved(pick.buckets); err != nil || !ok {
		return false, err
	}
	if pick.getter {
		wanted := p.key('v', call.getter)
		var ok bool
		if pick.getterProof {
			ok, err = p.getterFilesProved(p.key('t', call.getter), wanted, p.unique)
		} else {
			ok, err = p.getterFallbackProved(p.key('t', call.getter), wanted)
		}
		if err != nil || !ok {
			return false, err
		}
	}
	if pick.caller == nil {
		return true, nil
	}
	state, err := p.state(p.key('n', pick.caller.ID))
	return state == rebuildScratchExact, err
}

// Copies use the plan's complete logical edge tuple, not receipt arrival.
// Claims still prove the input bytes; registration writes use receipt order.
func (p *rebuildFnPointerStage) finishRegistrations(ctx context.Context) error {
	prefix := p.column('h')
	return p.visit(ctx, prefix, func(r row) error {
		old := r.key[len(prefix):]
		e, err := p.edge(old)
		if err != nil || e == nil {
			return errors.Join(err, invalid("function pointer registration missing"))
		}
		slot, source, name := p.decide.Registration(e)
		proved, err := fnPointerInputProved(&p.rebuildCallMoves, old, r.value)
		if err != nil {
			return err
		}
		if name != "" && proved {
			proved, err = p.registrationProved(e, name)
			if err != nil {
				return err
			}
		}
		if !proved {
			moving, err := p.present(append(p.column('m'), old...))
			if err != nil {
				return err
			}
			if moving {
				p.refused.add(graph.EdgeIdentityFor(e), bytes.Clone(old))
			} else {
				p.dependencies.add(graph.EdgeIdentityFor(e), bytes.Clone(old))
			}
		}
		if source == "" || slot == source {
			return nil
		}
		tuple := appendKeyLine(p.key('x', e.From, string(e.Kind), e.To, e.FilePath), e.Line)
		return p.put(tuple, keyStrings(nil, slot, source))
	})
}

func (p *rebuildFnPointerStage) members(ctx context.Context, slot string, visit func(string) error) error {
	prefix := p.key('y', slot)
	return p.visit(ctx, prefix, func(r row) error {
		id, rest, err := readKeyString(r.key[len(prefix):])
		if err != nil || id == "" || len(rest) != 0 {
			return invalid("function pointer slot member")
		}
		return visit(id)
	})
}

func (p *rebuildFnPointerStage) propagate(ctx context.Context) error {
	for range p.decide.Sweeps {
		changed := false
		err := p.visit(ctx, p.column('x'), func(r row) error {
			to, rest, err := readKeyString(r.value)
			if err != nil {
				return err
			}
			from, rest, err := readKeyString(rest)
			if err != nil || len(rest) != 0 {
				return invalid("function pointer copy tuple")
			}
			return p.members(ctx, from, func(id string) error {
				key := p.key('y', to, id)
				seen, err := p.present(key)
				if err != nil || seen {
					return err
				}
				changed = true
				return p.put(key, nil)
			})
		})
		if err != nil {
			return err
		}
		p.result.Sweeps++
		if !changed {
			break
		}
	}
	return nil
}

func (p *rebuildFnPointerStage) dispatchTargets(ctx context.Context, st, field string) ([]string, error) {
	var ids []string
	err := p.members(ctx, st+"\x00"+field, func(id string) error {
		if len(ids) < p.decide.Cap {
			ids = append(ids, id)
		}
		return nil
	})
	return ids, err
}

func (p *rebuildFnPointerStage) planDispatch(ctx context.Context) error {
	prefix := p.calls.column('h')
	return p.visit(ctx, prefix, func(r row) error {
		old := r.key[len(prefix):]
		e, err := p.calls.edge(old)
		if err != nil || e == nil {
			return errors.Join(err, invalid("function pointer call missing"))
		}
		st, field, _ := p.decide.Dispatch(e)
		ids, err := p.dispatchTargets(ctx, st, field)
		if err != nil {
			return err
		}
		var first string
		if len(ids) != 0 {
			first = ids[0]
		}
		p.result.Resolved += uint64(len(ids))
		moving := p.decide.BindCall(e, first, len(ids) != 0)
		if !moving && len(ids) <= 1 {
			return nil
		}
		proved, err := fnPointerInputProved(&p.calls, old, r.value)
		if err != nil {
			return err
		}
		if !proved {
			p.refused.add(graph.EdgeIdentityFor(e), bytes.Clone(old))
			return nil
		}
		if len(ids) > 1 {
			if err := p.calls.put(append(p.calls.column('f'), old...), nil); err != nil {
				return err
			}
			p.result.Adds += uint64(len(ids) - 1)
		}
		if moving {
			p.result.CallMoves++
			return p.calls.stage(old, e)
		}
		return nil
	})
}

// Fanout follows the native framework batch: first position, last payload
// for the same identity, then insert-if-absent after the two reindex phases.
func (p *rebuildFnPointerStage) orderDispatch(ctx context.Context) error {
	return p.visit(ctx, p.calls.column('z'), func(r row) error {
		key := append(p.calls.column('r'), r.value...)
		claim, err := p.calls.marked(key)
		if err != nil || len(claim) <= 1 || claim[0] != rebuildScratchExact {
			return err
		}
		old := claim[1:]
		// The first exact position was recorded before dispatch planning.
		claim[0] = rebuildCapabilityWanted
		if err := p.calls.noteReceipt(key, claim, true); err != nil {
			return err
		}
		fanout, err := p.calls.present(append(p.calls.column('f'), old...))
		if err != nil || !fanout {
			return err
		}
		e, err := p.calls.edge(old)
		if err != nil || e == nil {
			return errors.Join(err, invalid("function pointer fanout source missing"))
		}
		st, field, _ := p.decide.Dispatch(e)
		ids, err := p.dispatchTargets(ctx, st, field)
		if err != nil {
			return err
		}
		if len(ids) < 2 {
			return invalid("function pointer fanout has no secondary")
		}
		for _, id := range ids[1:] {
			if err := p.offerFanout(p.decide.Fanout(e, id, st, field)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (p *rebuildFnPointerStage) offerFanout(e *graph.Edge) error {
	key := pendingEdgeKey(e)
	index := append(p.calls.column('a'), key...)
	order, err := p.calls.get(index)
	if err != nil {
		return err
	}
	if order == nil {
		order = binary.BigEndian.AppendUint64(nil, p.adds)
		if err := p.calls.put(index, order); err != nil {
			return err
		}
		p.adds++
	}
	value, err := encodeEdge(e)
	if err != nil {
		return err
	}
	if err := p.calls.input(key, value); err != nil {
		return err
	}
	return p.calls.put(append(p.calls.column('d'), order...), value)
}

func (p *rebuildFnPointerStage) prepareFanout(ctx context.Context, path string) (err error) {
	w, err := newLinkTable(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, w.Close()) }()
	prefix := p.calls.column('d')
	var rows uint64
	if err := p.visit(ctx, prefix, func(r row) error {
		rows++
		return w.Set(r.key[len(prefix):], r.value)
	}); err != nil {
		return err
	}
	if rows != p.adds {
		return invalid("function pointer fanout count does not match plan")
	}
	return nil
}

func (p *rebuildFnPointerStage) write(ctx context.Context) (err error) {
	if p.result.RefMoves == 0 && p.result.CallMoves == 0 && p.result.Adds == 0 {
		return nil
	}
	if err := p.orderDispatch(ctx); err != nil {
		return err
	}
	if p.ordered != p.result.RefMoves || p.calls.ordered != p.result.CallMoves || p.result.Adds > 0 && p.adds == 0 {
		return invalid("function pointer ordered write count does not match plan")
	}
	var run string
	if p.adds != 0 {
		dir, mkdirErr := os.MkdirTemp(filepath.Join(p.b.scratchParent, "pending"), "fn-pointer-fanout-")
		if mkdirErr != nil {
			return mkdirErr
		}
		defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
		run = filepath.Join(dir, "fanout.sst")
		if err := p.prepareFanout(ctx, run); err != nil {
			return err
		}
	}
	return p.b.withWrites(func() error {
		if err := p.apply(ctx, p.result.RefMoves); err != nil {
			return err
		}
		if err := p.calls.apply(ctx, p.result.CallMoves); err != nil || run == "" {
			return err
		}
		return visitLinkRun(ctx, run, nil, func(r row) error {
			e, err := decodeEdge(r.value)
			if err != nil {
				return err
			}
			occupied, err := p.calls.present(pendingEdgeKey(e))
			if err != nil || occupied {
				return err
			}
			return p.b.addEdgeWithCustody(e, rebuildEdgeCustody{proof: rebuildProofFnPointerFanout})
		})
	})
}

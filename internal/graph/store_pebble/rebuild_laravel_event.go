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

var ErrRebuildLaravelEvent = errors.New("pebble rebuild Laravel event stage failed")

var rebuildLaravelEventPrefix = []byte{'g', 'q', 'L'}

// RebuildLaravelEventStats counts tagged Calls, native matches, moves and attempted adds.
type RebuildLaravelEventStats struct {
	Stubs, Resolved, Moves, Adds uint64
}

// RebuildLaravelEventRules supplies the native role, name and payload decisions.
type RebuildLaravelEventRules struct {
	ListenMap    func(*graph.Node) string
	ListenerType func(*graph.Node) string
	Entries      func(string, func(event, class string))
	SimpleName   func(string) string
	Call         func(*graph.Edge) string
	Rebind       func(*graph.Edge, string, bool) bool
	Fanout       func(*graph.Edge, string, string) *graph.Edge
}

type rebuildLaravelEventPass struct {
	rebuildCallMoves
	rules     RebuildLaravelEventRules
	stats     RebuildLaravelEventStats
	haveCalls bool
}

// RebuildLaravelEvent runs once on a fresh private graph, after Sidekiq.
func (b *BuildStore) RebuildLaravelEvent(ctx context.Context, receipts iter.Seq2[ParseFacts, error], rules RebuildLaravelEventRules) (stats RebuildLaravelEventStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildLaravelEventStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildLaravelEvent, err)
		}
	}()
	if rules.ListenMap == nil || rules.ListenerType == nil || rules.Entries == nil || rules.SimpleName == nil ||
		rules.Call == nil || rules.Rebind == nil || rules.Fanout == nil {
		return stats, invalid("Laravel event inputs")
	}
	prefix := rebuildLaravelEventPrefix
	busy, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return stats, err
	}
	if busy {
		return stats, invalid("Laravel scratch range occupied")
	}
	defer func() { err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync)) }()
	p := rebuildLaravelEventPass{
		rebuildCallMoves: rebuildCallMoves{b: b, prefix: prefix, rowBytes: 16 << 20, label: "Laravel event", inputRow: "edge row"},
		rules:            rules,
	}
	active, err := p.collectNodes(ctx)
	if err != nil || !active {
		return stats, err
	}
	if err := p.collectCalls(ctx); err != nil || !p.haveCalls {
		return p.stats, err
	}
	if receipts != nil {
		if err := p.captureMembers(ctx); err != nil {
			return stats, err
		}
		if err := p.record(ctx, receipts); err != nil {
			return stats, err
		}
		if err := p.projectReceiptRoles(ctx); err != nil {
			return stats, err
		}
	}
	if err := p.joinHandles(ctx); err != nil {
		return stats, err
	}
	if err := p.planCalls(ctx); err != nil || p.stats.Moves == 0 && p.stats.Adds == 0 {
		return p.stats, err
	}
	var run string
	if p.stats.Adds != 0 {
		dir, mkdirErr := os.MkdirTemp(filepath.Join(b.scratchParent, "pending"), "laravel-fanout-")
		if mkdirErr != nil {
			return stats, mkdirErr
		}
		defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
		run = filepath.Join(dir, "fanout.sst")
	}
	if err := p.prepareOrdered(ctx, run); err != nil {
		return stats, err
	}
	return p.stats, b.withWrites(func() error {
		if err := p.apply(ctx, p.stats.Moves); err != nil || run == "" {
			return err
		}
		return visitLinkRun(ctx, run, nil, func(r row) error {
			e, err := decodeEdge(r.value)
			if err != nil {
				return err
			}
			return p.addFanoutIfAbsent(e, rebuildProofLaravelEventFanout)
		})
	})
}

func (p *rebuildLaravelEventPass) visit(ctx context.Context, prefix []byte, yield func(row) error) error {
	return pendingVisit(ctx, p.b.pending, prefix, yield)
}

func laravelCallable(kind graph.NodeKind) bool {
	return kind == graph.KindMethod || kind == graph.KindFunction
}

type laravelReceiptRole struct {
	kind         byte
	event, class string
}

func (p *rebuildLaravelEventPass) roles(n *graph.Node, visit func(laravelReceiptRole)) bool {
	if n.Kind == graph.KindType {
		var active bool
		p.rules.Entries(p.rules.ListenMap(n), func(event, class string) {
			visit(laravelReceiptRole{kind: 'q', event: event, class: class})
			active = true
		})
		return active
	}
	if !laravelCallable(n.Kind) {
		return false
	}
	if n.Name == "handle" {
		visit(laravelReceiptRole{kind: 'h'})
	}
	if role := p.rules.ListenerType(n); role != "" {
		visit(laravelReceiptRole{kind: 't', event: p.rules.SimpleName(role)})
		return true
	}
	return false
}

func (p *rebuildLaravelEventPass) collectNodes(ctx context.Context) (bool, error) {
	var active bool
	err := p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		light, err := checkedRebuildLightNode(r, p.label)
		if err != nil || light.Kind != graph.KindType && !laravelCallable(light.Kind) {
			return err
		}
		n, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		if n.ID == "" || !bytes.Equal(r.key, pendingNodeKey(n.ID)) {
			return invalid("Laravel node identity")
		}
		var hasRole bool
		found := p.roles(n, func(role laravelReceiptRole) {
			if !hasRole {
				hasRole = true
				err = p.input(r.key, r.value)
			}
			if err == nil {
				err = p.putRole(role.kind, n.ID, role.event, role.class)
			}
		})
		active = active || found
		if err != nil || !hasRole {
			return err
		}
		return p.put(p.key('n', n.ID), nil)
	})
	return active, err
}

func (p *rebuildLaravelEventPass) collectCalls(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || kind != graph.EdgeCalls {
			return err
		}
		p.haveCalls = true
		raw, err := rebuildReceiptKey(snap, r.key)
		if err != nil && !errors.Is(err, ErrRebuildUnknownProvenance) {
			return err
		}
		if raw != nil {
			if err := p.claim(raw, r.key); err != nil {
				return err
			}
		}
		e, err := p.decodeKeyedEdge(r.key, r.value)
		if err != nil || p.rules.Call(e) == "" {
			return err
		}
		if err := p.input(r.key, r.value); err != nil {
			return err
		}
		p.stats.Stubs++
		if err := p.put(p.key('e', p.rules.SimpleName(p.rules.Call(e))), nil); err != nil {
			return err
		}
		if raw != nil {
			id, err := decodeLinkRecorder(raw)
			if err != nil {
				return err
			}
			if id.Kind != graph.EdgeCalls || id.From != e.From || id.FilePath != e.FilePath || id.Line != e.Line {
				raw = nil
			}
		}
		return p.put(append(p.column('c'), r.key...), raw)
	})
}

func (p *rebuildLaravelEventPass) members(ctx context.Context, id string, visit func(row, *graph.Edge) error) error {
	return p.visit(ctx, guardOutgoing(id, graph.EdgeMemberOf), func(r row) error {
		e, err := guardLightEdge(r)
		if err != nil || e.To == "" {
			return err
		}
		return visit(r, e)
	})
}

func (p *rebuildLaravelEventPass) captureMembers(ctx context.Context) error {
	mapped, err := rebuildGuardHasPrefix(ctx, p.b.pending, p.column('q'))
	if err != nil || !mapped {
		return err
	}
	prefix := p.column('h')
	return p.visit(ctx, prefix, func(r row) error {
		id, _, err := readKeyString(r.key[len(prefix):])
		if err != nil {
			return err
		}
		return p.members(ctx, id, func(r row, e *graph.Edge) error {
			raw, err := rebuildReceiptKey(p.b.pending, r.key)
			if errors.Is(err, ErrRebuildUnknownProvenance) {
				return nil
			}
			if err != nil {
				return err
			}
			original, err := decodeLinkRecorder(raw)
			if err != nil || original.Kind != e.Kind || original.From != e.From || original.FilePath != e.FilePath || original.Line != e.Line {
				return err
			}
			return p.claimKey(append(p.column('u'), raw...), r.key)
		})
	})
}

func (p *rebuildLaravelEventPass) record(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	var order uint64
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if n.FilePath != file || facts.EffectiveLanguage == "" {
				continue
			}
			if err := p.recordNode(ctx, n); err != nil {
				return err
			}
		}
		for _, e := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			switch e.Kind {
			case graph.EdgeCalls, graph.EdgeMemberOf:
				if err := p.recordEdge(e, facts.EffectiveLanguage != "" && e.FilePath == file, order); err != nil {
					return err
				}
			}
			order++
		}
	}
	return nil
}

func (p *rebuildLaravelEventPass) putRole(col byte, id, event, class string) error {
	switch col {
	case 'h', 'H', 'I':
		return p.put(p.key(col, id), nil)
	case 'q', 'Q', 'Y':
		return p.put(p.key(col, class, event), nil)
	default:
		return p.put(p.key(col, event, id), nil)
	}
}

func (p *rebuildLaravelEventPass) recordNode(ctx context.Context, n *graph.Node) error {
	next := make(map[string]bool)
	active := p.roles(n, func(role laravelReceiptRole) {
		next[string(keyStrings([]byte{role.kind}, role.event, role.class))] = true
	})
	seenKey := p.key('d', n.ID)
	const seenRoleless, seenChecked = 1, 2
	seen, err := rebuildScratchState(p.b.pending, seenKey, "Laravel receipt node state")
	if err != nil {
		return err
	}
	if seen != seenChecked {
		if len(next) == 0 {
			wanted, err := p.present(p.key('n', n.ID))
			if err != nil {
				return err
			}
			if !wanted {
				// Keep the earlier role-less fact without loading an unrelated live row.
				return noteRebuildScratchState(p.put, seenKey, seen, seenRoleless)
			}
		}
		key := pendingNodeKey(n.ID)
		live, err := pendingGet(p.b.pending, key, func(val []byte) (*graph.Node, error) {
			if err := p.input(key, val); err != nil {
				return nil, err
			}
			return checkedRebuildLightNode(row{key, val}, p.label)
		})
		if err != nil || live == nil {
			return err
		}
		if err := p.put(seenKey, []byte{seenChecked}); err != nil {
			return err
		}
	}
	anchor := p.key('a', n.ID)
	if !active && seen != 0 {
		if err := p.b.pending.Delete(anchor, pebble.NoSync); err != nil {
			return err
		}
	} else if active && seen == 0 {
		if err := p.put(anchor, nil); err != nil {
			return err
		}
	}
	// An admitted role is definite only if every receipt for the same ID
	// reports it. Keep differences per role rather than per raw metadata blob.
	prefix := p.key('R', n.ID)
	const exact, possible = 1, 2
	if err := p.visit(ctx, prefix, func(r row) error {
		key := string(r.key[len(prefix):])
		if next[key] {
			delete(next, key)
			return nil
		}
		return p.put(r.key, []byte{possible})
	}); err != nil {
		return err
	}
	state := byte(exact)
	if seen != 0 {
		state = possible
	}
	for key := range next {
		if err := p.put(append(prefix[:len(prefix):len(prefix)], key...), []byte{state}); err != nil {
			return err
		}
	}
	return nil
}

func (p *rebuildLaravelEventPass) projectReceiptRoles(ctx context.Context) error {
	prefix := p.column('R')
	return p.visit(ctx, prefix, func(r row) error {
		id, rest, err := readKeyString(r.key[len(prefix):])
		if err != nil || len(rest) == 0 || len(r.value) != 1 {
			return invalid("Laravel receipt role")
		}
		kind := rest[0]
		event, rest, err := readKeyString(rest[1:])
		if err != nil {
			return err
		}
		class, rest, err := readKeyString(rest)
		if err != nil || len(rest) != 0 {
			return invalid("Laravel receipt role class")
		}
		if r.value[0] != 1 {
			switch kind {
			case 'q':
				return p.putRole('Y', id, event, class)
			case 'h':
				return p.putRole('I', id, event, class)
			default:
				return p.putRole('x', id, event, class)
			}
		}
		switch kind {
		case 'q':
			return p.putRole('Q', id, event, class)
		case 'h':
			return p.putRole('H', id, event, class)
		default:
			return p.putRole('T', id, event, class)
		}
	})
}

// z retains each Call's first exact receipt; w retains the last
// receipt-backed owner with a retained position per handle.
func (p *rebuildLaravelEventPass) recordEdge(e *graph.Edge, admitted bool, order uint64) error {
	if e.Kind == graph.EdgeCalls {
		raw := pendingEdgeKey(e)
		claim, err := p.marked(append(p.column('r'), raw...))
		if err != nil || len(claim) <= 1 || claim[0] != rebuildCapabilityWanted {
			return err
		}
		old := claim[1:]
		key := append(p.column('c'), old...)
		owned, err := p.get(key)
		if err != nil {
			return err
		}
		if owned == nil {
			return p.recordReceiptOnlyCall(e, admitted, raw, old, claim)
		}
		if !bytes.Equal(owned, raw) {
			return nil
		}
		if admitted {
			typ := p.rules.Call(e)
			if err := p.put(append(p.column('j'), claim[1:]...), appendKeyString(nil, typ)); err != nil {
				return err
			}
			if typ != "" {
				if err := p.put(p.key('e', p.rules.SimpleName(typ)), nil); err != nil {
					return err
				}
			}
		}
	}
	edge := p.edge
	if e.Kind == graph.EdgeMemberOf {
		edge = func(key []byte) (*graph.Edge, error) {
			return pendingGet(p.b.pending, key, func(val []byte) (*graph.Edge, error) {
				return guardLightEdge(row{key, val})
			})
		}
	}
	return p.recordOwnerEdge(e, admitted, order, func(live *graph.Edge) bool {
		return p.rules.Call(e) == p.rules.Call(live)
	}, func(live *graph.Edge) (string, bool) {
		return p.rules.SimpleName(live.To), true
	}, 'w', func(err error) error {
		if err != nil {
			return err
		}
		return invalid("Laravel receipt live edge missing")
	}, edge)
}

// A raw-only route may affect a stationary live Call, but only a unique
// receipt with the same custody identity can be assigned to that Call.
func (p *rebuildLaravelEventPass) recordReceiptOnlyCall(e *graph.Edge, admitted bool, raw, old, claim []byte) error {
	claimKey := append(p.column('r'), raw...)
	if err := noteRebuildScratchPayload(p.put, claimKey, claim, rebuildScratchProofBit(admitted)); err != nil {
		return err
	}
	typ := p.rules.Call(e)
	if typ == "" {
		return nil
	}
	live, err := p.edge(old)
	if err != nil || live == nil {
		return errors.Join(err, invalid("Laravel receipt-only Call missing"))
	}
	if p.rules.Call(live) != "" {
		return nil
	}
	key := append(p.column('C'), old...)
	if !admitted || live.Kind != e.Kind || live.From != e.From || live.FilePath != e.FilePath || live.Line != e.Line {
		// Retain the unproved tag until replay establishes whether any route can affect output.
		return p.put(key, nil)
	}
	if err := p.put(p.key('e', p.rules.SimpleName(typ)), nil); err != nil {
		return err
	}
	return p.put(key, appendKeyString(nil, typ))
}

// joinHandles elects the owner from MemberOf rows, not from a class node or
// receiver metadata. Receipt order witnesses the last retained raw row.
func (p *rebuildLaravelEventPass) joinHandles(ctx context.Context) error {
	var mapped bool
	for _, col := range [...]byte{'q', 'Q', 'Y'} {
		found, err := rebuildGuardHasPrefix(ctx, p.b.pending, p.column(col))
		if err != nil {
			return err
		}
		mapped = mapped || found
	}
	if !mapped {
		return nil
	}
	prefix := p.column('h')
	if err := p.visit(ctx, prefix, func(r row) error {
		id, _, err := readKeyString(r.key[len(prefix):])
		if err != nil {
			return err
		}
		class, proved, uncertain, err := p.owner(ctx, id)
		if err != nil {
			return err
		}
		if uncertain {
			return p.possibleOwners(ctx, id)
		}
		exact, err := p.present(p.key('H', id))
		if err != nil {
			return err
		}
		if err := p.join(ctx, 'q', 't', class, id); err != nil {
			return err
		}
		if proved && exact {
			if err := p.join(ctx, 'Q', 'T', class, id); err != nil {
				return err
			}
			return p.join(ctx, 'Y', 'x', class, id)
		}
		if exact {
			return p.possibleOwners(ctx, id)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := p.visit(ctx, p.column('I'), func(r row) error {
		id, _, err := readKeyString(r.key[len(p.prefix)+1:])
		if err != nil {
			return err
		}
		return p.possibleOwners(ctx, id)
	}); err != nil {
		return err
	}
	// A known raw handle that no longer has the live handle role can
	// affect an election, including when a typed route otherwise matches.
	prefix = p.column('H')
	return p.visit(ctx, prefix, func(r row) error {
		id, _, err := readKeyString(r.key[len(prefix):])
		if err != nil {
			return err
		}
		live, err := p.present(p.key('h', id))
		if err != nil || live {
			return err
		}
		class, exact, uncertain, err := p.owner(ctx, id)
		if err != nil {
			return err
		}
		if uncertain || !exact {
			return p.possibleOwners(ctx, id)
		}
		if err := p.join(ctx, 'Q', 'T', class, id); err != nil {
			return err
		}
		return p.join(ctx, 'Y', 'x', class, id)
	})
}

func (p *rebuildLaravelEventPass) possibleOwners(ctx context.Context, id string) error {
	return p.members(ctx, id, func(r row, e *graph.Edge) error {
		class := p.rules.SimpleName(e.To)
		used, err := p.classUsed(ctx, class)
		if err != nil || !used {
			return err
		}
		if err := p.input(r.key, r.value); err != nil {
			return err
		}
		if err := p.join(ctx, 'Q', 'x', class, id); err != nil {
			return err
		}
		return p.join(ctx, 'Y', 'x', class, id)
	})
}

func (p *rebuildLaravelEventPass) join(ctx context.Context, mapped, listeners byte, class, id string) error {
	prefix := p.key(mapped, class)
	return p.visit(ctx, prefix, func(r row) error {
		event, _, err := readKeyString(r.key[len(prefix):])
		if err != nil {
			return err
		}
		wanted, err := p.present(p.key('e', event))
		if err != nil || !wanted {
			return err
		}
		return p.put(p.key(listeners, event, id), nil)
	})
}

func (p *rebuildLaravelEventPass) classUsed(ctx context.Context, class string) (bool, error) {
	for _, col := range [...]byte{'q', 'Q', 'Y'} {
		prefix := p.key(col, class)
		var used bool
		if err := p.visit(ctx, prefix, func(r row) error {
			event, _, err := readKeyString(r.key[len(prefix):])
			if err != nil || used {
				return err
			}
			used, err = p.present(p.key('e', event))
			return err
		}); err != nil || used {
			return used, err
		}
	}
	return false, nil
}

func (p *rebuildLaravelEventPass) owner(ctx context.Context, id string) (string, bool, bool, error) {
	var owners rebuildMemberOwners
	var mapped bool
	err := p.members(ctx, id, func(r row, e *graph.Edge) error {
		name := p.rules.SimpleName(e.To)
		owners.observe(name)
		used, err := p.classUsed(ctx, name)
		if err != nil {
			return err
		}
		if used {
			mapped = true
			if err := p.input(r.key, r.value); err != nil {
				return err
			}
		}
		raw, err := rebuildReceiptKey(p.b.pending, r.key)
		if errors.Is(err, ErrRebuildUnknownProvenance) {
			raw, err = nil, nil
		}
		if err != nil {
			return err
		}
		return owners.prove(&p.rebuildCallMoves, raw, r.key, name)
	})
	if err != nil || !owners.seen {
		return "", !owners.seen, false, err
	}
	if !mapped {
		return owners.class, owners.exact, false, nil
	}
	return owners.elect(&p.rebuildCallMoves, id, 'w', nil)
}

func (p *rebuildLaravelEventPass) listeners(ctx context.Context, event string, visit func(string, bool) error) error {
	prefix := p.key('t', event)
	first := true
	return p.visit(ctx, prefix, func(r row) error {
		id, rest, err := readKeyString(r.key[len(prefix):])
		if err != nil || id == "" || len(rest) != 0 {
			return invalid("Laravel listener key")
		}
		err = visit(id, first)
		first = false
		return err
	})
}

func (p *rebuildLaravelEventPass) listenersProved(ctx context.Context, event string) (bool, error) {
	// A possible owner adds no new output if the same listener is proved
	// independently, for example by its typed listener declaration.
	for _, pair := range [...][2]byte{{'t', 'T'}, {'T', 't'}, {'x', 'T'}} {
		ok, err := p.listenerSubset(ctx, event, pair[0], pair[1])
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

func (p *rebuildLaravelEventPass) listenerSubset(ctx context.Context, event string, from, to byte) (bool, error) {
	prefix, other := p.key(from, event), p.key(to, event)
	ok := true
	err := p.visit(ctx, prefix, func(r row) error {
		if !ok {
			return nil
		}
		var err error
		ok, err = p.present(append(other[:len(other):len(other)], r.key[len(prefix):]...))
		return err
	})
	return ok, err
}

func (p *rebuildLaravelEventPass) planCalls(ctx context.Context) error {
	anchor, err := rebuildGuardHasPrefix(ctx, p.b.pending, p.column('a'))
	if err != nil {
		return err
	}
	var refused rebuildScratchRefusals
	prefix := p.column('C')
	if err := p.visit(ctx, prefix, func(r row) error {
		old := r.key[len(prefix):]
		e, err := p.edge(old)
		if err != nil || e == nil {
			return errors.Join(err, invalid("Laravel planned receipt-only Call missing"))
		}
		if len(r.value) == 0 {
			if anchor {
				refused.add(graph.EdgeIdentityFor(e), bytes.Clone(old))
			}
			return nil
		}
		changed, err := p.receiptChangesStationaryCall(ctx, r.value, *e, anchor)
		if changed {
			refused.add(graph.EdgeIdentityFor(e), bytes.Clone(old))
		}
		return err
	}); err != nil {
		return err
	}
	prefix = p.column('c')
	if err := p.visit(ctx, prefix, func(r row) error {
		old := r.key[len(prefix):]
		e, err := p.edge(old)
		if err != nil || e == nil {
			return errors.Join(err, invalid("Laravel planned Call missing"))
		}
		event := p.rules.SimpleName(p.rules.Call(e))
		before := *e
		var count uint64
		var target string
		if err := p.listeners(ctx, event, func(id string, first bool) error {
			if first {
				target = id
			}
			count++
			return nil
		}); err != nil {
			return err
		}
		p.stats.Resolved += count
		moving := p.rules.Rebind(e, target, count != 0)
		if !moving && count <= 1 {
			raw, err := p.get(append(p.column('j'), old...))
			if err != nil {
				return err
			}
			if raw == nil {
				if anchor {
					refused.add(graph.EdgeIdentityFor(e), bytes.Clone(old))
				}
				return nil
			}
			changed, err := p.receiptChangesStationaryCall(ctx, raw, before, anchor)
			if changed {
				refused.add(graph.EdgeIdentityFor(e), bytes.Clone(old))
			}
			return err
		}
		proved, err := p.listenersProved(ctx, event)
		if err != nil {
			return err
		}
		var claim []byte
		if len(r.value) != 0 {
			claim, err = p.marked(append(p.column('r'), r.value...))
			if err != nil {
				return err
			}
		}
		if (!anchor && count == 0) || !proved || len(claim) <= 1 || claim[0] != rebuildScratchExact || !bytes.Equal(claim[1:], old) {
			refused.add(graph.EdgeIdentityFor(e), pendingEdgeKey(e))
			return nil
		}
		if count > 1 {
			p.stats.Adds += count - 1
			if err := p.put(append(p.column('f'), old...), nil); err != nil {
				return err
			}
		}
		if moving {
			p.stats.Moves++
			return p.stage(old, e)
		}
		return nil
	}); err != nil {
		return err
	}
	return refused.err()
}

func (p *rebuildLaravelEventPass) receiptChangesStationaryCall(ctx context.Context, raw []byte, before graph.Edge, anchor bool) (bool, error) {
	typ, rest, err := readKeyString(raw)
	if err != nil || len(rest) != 0 {
		return false, invalid("Laravel receipt Call route")
	}
	if typ == "" || !anchor {
		return false, nil
	}
	prefix := p.key('T', p.rules.SimpleName(typ))
	var first string
	var matched uint64
	if err := p.visit(ctx, prefix, func(r row) error {
		id, _, err := readKeyString(r.key[len(prefix):])
		if matched == 0 {
			first = id
		}
		matched++
		return err
	}); err != nil {
		return false, err
	}
	certain, err := p.listenerSubset(ctx, p.rules.SimpleName(typ), 'x', 'T')
	if err != nil {
		return false, err
	}
	return !certain || p.rules.Rebind(&before, first, matched != 0) || matched > 1, nil
}

func (p *rebuildLaravelEventPass) prepareOrdered(ctx context.Context, path string) (err error) {
	var write func([]byte, []byte) error
	if path != "" {
		w, openErr := newLinkTable(path)
		if openErr != nil {
			return openErr
		}
		defer func() { err = errors.Join(err, w.Close()) }()
		write = w.Set
	}
	var adds uint64
	if err := p.visit(ctx, p.column('z'), func(r row) error {
		old, err := p.orderCallReceipt(r.value)
		if err != nil || old == nil {
			return err
		}
		if write == nil {
			return nil
		}
		fanout, err := p.present(append(p.column('f'), old...))
		if err != nil || !fanout {
			return err
		}
		e, err := p.edge(old)
		if err != nil || e == nil {
			return errors.Join(err, invalid("Laravel fanout source missing"))
		}
		kind := p.rules.Call(e)
		return p.listeners(ctx, p.rules.SimpleName(kind), func(id string, first bool) error {
			if first {
				return nil
			}
			add := p.rules.Fanout(e, id, kind)
			value, err := encodeEdge(add)
			if err != nil {
				return err
			}
			if err := p.input(pendingEdgeKey(add), value); err != nil {
				return err
			}
			key := binary.BigEndian.AppendUint64(nil, adds)
			adds++
			return write(key, value)
		})
	}); err != nil {
		return err
	}
	if p.ordered != p.stats.Moves || adds != p.stats.Adds {
		return invalid("Laravel ordered write count does not match plan")
	}
	return nil
}

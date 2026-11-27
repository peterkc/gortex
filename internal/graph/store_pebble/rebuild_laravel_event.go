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
	rules RebuildLaravelEventRules
	stats RebuildLaravelEventStats
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
	if err := p.collectCalls(ctx); err != nil || p.stats.Stubs == 0 {
		return p.stats, err
	}
	if receipts != nil {
		if err := p.captureMembers(ctx); err != nil {
			return stats, err
		}
		if err := p.record(ctx, receipts); err != nil {
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
			occupied, err := p.present(pendingEdgeKey(e))
			if err != nil || occupied {
				return err
			}
			return b.addEdgeWithCustody(e, rebuildEdgeCustody{proof: rebuildProofLaravelEventFanout})
		})
	})
}

func (p *rebuildLaravelEventPass) visit(ctx context.Context, prefix []byte, yield func(row) error) error {
	return pendingVisit(ctx, p.b.pending, prefix, yield)
}

func laravelCallable(kind graph.NodeKind) bool {
	return kind == graph.KindMethod || kind == graph.KindFunction
}

// t and q are the live typed listeners and class-to-event map. T and Q
// are their receipt projections. Keep a map entry even without a handler.
func (p *rebuildLaravelEventPass) roles(n *graph.Node, typed, mapped byte) (bool, error) {
	if n.Kind == graph.KindType {
		var active bool
		var err error
		p.rules.Entries(p.rules.ListenMap(n), func(event, class string) {
			if err == nil {
				err = p.put(p.key(mapped, class, event), nil)
			}
			active = true
		})
		return active, err
	}
	if role := p.rules.ListenerType(n); laravelCallable(n.Kind) && role != "" {
		return true, p.put(p.key(typed, p.rules.SimpleName(role), n.ID), nil)
	}
	return false, nil
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
		if laravelCallable(n.Kind) && n.Name == "handle" {
			if err := p.put(p.key('h', n.ID), nil); err != nil {
				return err
			}
		}
		found, err := p.roles(n, 't', 'q')
		active = active || found
		return err
	})
	return active, err
}

func (p *rebuildLaravelEventPass) collectCalls(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || kind != graph.EdgeCalls {
			return err
		}
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
			if n.FilePath != file || n.Language == "" || n.Language != facts.EffectiveLanguage {
				continue
			}
			if err := p.recordNode(n, file); err != nil {
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

func (p *rebuildLaravelEventPass) recordNode(n *graph.Node, file string) error {
	if !laravelCallable(n.Kind) && n.Kind != graph.KindType {
		return nil
	}
	live, err := p.b.pendingNode(n.ID)
	if err != nil || !sameRebuildLanguageMarker(n, live, file) {
		return err
	}
	if n.Kind == graph.KindType && (live.Kind != graph.KindType || p.rules.ListenMap(n) != p.rules.ListenMap(live)) {
		// A changed provider cannot prove a live route from another provider.
		p.rules.Entries(p.rules.ListenMap(n), func(event, _ string) {
			if err == nil {
				err = p.put(p.key('x', event), nil)
			}
		})
		return err
	}
	active, err := p.roles(n, 'T', 'Q')
	if err != nil {
		return err
	}
	if active && (n.Kind == graph.KindType || laravelCallable(live.Kind) && p.rules.ListenerType(live) != "" &&
		p.rules.SimpleName(p.rules.ListenerType(n)) == p.rules.SimpleName(p.rules.ListenerType(live))) {
		if err := p.put(p.column('a'), nil); err != nil {
			return err
		}
	}
	if laravelCallable(n.Kind) && n.Name == "handle" {
		return p.put(p.key('H', n.ID), nil)
	}
	return nil
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
		owned, err := p.get(append(p.column('c'), claim[1:]...))
		if err != nil || !bytes.Equal(owned, raw) {
			return err
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
	return p.recordOwnerEdge(e, admitted, order, func(live *graph.Edge) bool {
		return p.rules.Call(e) == p.rules.Call(live)
	}, func(live *graph.Edge) (string, bool) {
		return p.rules.SimpleName(live.To), true
	}, 'w', func(err error) error {
		if err != nil {
			return err
		}
		return invalid("Laravel receipt live edge missing")
	}, p.edge)
}

// joinHandles elects the owner from MemberOf rows, not from a class node or
// receiver metadata. Receipt order witnesses the last retained raw row.
func (p *rebuildLaravelEventPass) joinHandles(ctx context.Context) error {
	var mapped bool
	for _, col := range [...]byte{'q', 'Q'} {
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
		class, proved, err := p.owner(ctx, id)
		if err != nil {
			return err
		}
		exact, err := p.present(p.key('H', id))
		if err != nil {
			return err
		}
		if err := p.join(ctx, 'q', 't', class, id); err != nil {
			return err
		}
		if proved && exact {
			return p.join(ctx, 'Q', 'T', class, id)
		}
		return nil
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
		class, _, err := p.owner(ctx, id)
		if err != nil {
			return err
		}
		return p.join(ctx, 'Q', 'T', class, id)
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
	for _, col := range [...]byte{'q', 'Q'} {
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

func (p *rebuildLaravelEventPass) owner(ctx context.Context, id string) (string, bool, error) {
	var owners rebuildMemberOwners
	var mapped bool
	var relevant graph.EdgeIdentity
	err := p.members(ctx, id, func(r row, e *graph.Edge) error {
		name := p.rules.SimpleName(e.To)
		owners.observe(name)
		if !mapped {
			var err error
			mapped, err = p.classUsed(ctx, name)
			if err != nil {
				return err
			}
			if mapped {
				relevant = graph.EdgeIdentityFor(e)
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
		return "", !owners.seen, err
	}
	if !mapped {
		return owners.class, owners.exact, nil
	}
	name, exact, uncertain, err := owners.elect(&p.rebuildCallMoves, id, 'w', nil)
	if uncertain {
		return "", false, &RebuildUnknownProvenanceError{Count: 1, First: relevant}
	}
	return name, exact, err
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
	changed, err := p.present(p.key('x', event))
	if err != nil || changed {
		return false, err
	}
	for _, pair := range [...][2]byte{{'t', 'T'}, {'T', 't'}} {
		prefix := p.key(pair[0], event)
		other := p.key(pair[1], event)
		var missing bool
		if err := p.visit(ctx, prefix, func(r row) error {
			present, err := p.present(append(other[:len(other):len(other)], r.key[len(prefix):]...))
			missing = missing || !present
			return err
		}); err != nil || missing {
			return false, err
		}
	}
	return true, nil
}

func (p *rebuildLaravelEventPass) planCalls(ctx context.Context) error {
	anchor, err := p.present(p.column('a'))
	if err != nil {
		return err
	}
	var refused rebuildScratchRefusals
	prefix := p.column('c')
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
			if err != nil || raw == nil {
				return err
			}
			typ, rest, err := readKeyString(raw)
			if err != nil || len(rest) != 0 {
				return invalid("Laravel receipt Call route")
			}
			if typ == "" || !anchor {
				return nil
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
				return err
			}
			if p.rules.Rebind(&before, first, matched != 0) || matched > 1 {
				refused.add(graph.EdgeIdentityFor(e), bytes.Clone(old))
			}
			return nil
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

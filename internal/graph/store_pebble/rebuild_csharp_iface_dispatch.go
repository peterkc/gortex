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

var ErrRebuildCSharpIfaceDispatch = errors.New("pebble rebuild C# interface dispatch failed")

var rebuildCSharpIfacePrefix = []byte{'g', 'q', 'D'}

const rebuildCSharpIfaceRowBytes = 16 << 20

// RebuildCSharpIfaceRules carries the native predicates and Call payload.
type RebuildCSharpIfaceRules struct {
	Declared  func(*graph.Edge) bool
	Candidate func(*graph.Node, string) bool
	Anchor    func(*graph.Node) bool
	Source    func(*graph.Edge) bool
	Emit      func(*graph.Edge, string, string, int) *graph.Edge
	Cap       int
}

type rebuildCSharpIface struct {
	rebuildCallMoves
	rules    RebuildCSharpIfaceRules
	families uint64
	adds     uint64
	refused  rebuildScratchRefusals
	replay   bool
	poolGap  bool
}

// RebuildCSharpIfaceDispatch uses settled Extends/Implements, MemberOf and Call rows.
// It compares the live family with its receipt proof before any saved add.
func (b *BuildStore) RebuildCSharpIfaceDispatch(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	rules RebuildCSharpIfaceRules,
) (adds uint64, err error) {
	defer func() {
		if err != nil {
			adds = 0
			err = fmt.Errorf("%w: %w", ErrRebuildCSharpIfaceDispatch, err)
		}
	}()
	if receipts == nil || rules.Declared == nil || rules.Candidate == nil || rules.Anchor == nil || rules.Source == nil || rules.Emit == nil || rules.Cap < 2 {
		return 0, invalid("C# interface dispatch inputs")
	}
	prefix := rebuildCSharpIfacePrefix
	busy, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return 0, err
	}
	if busy {
		return 0, invalid("C# interface dispatch scratch range occupied")
	}
	defer func() { err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync)) }()
	p := rebuildCSharpIface{
		rebuildCallMoves: rebuildCallMoves{
			b: b, prefix: prefix, rowBytes: rebuildCSharpIfaceRowBytes, label: "C# interface dispatch", inputRow: "row",
		},
		rules: rules,
	}
	changed, err := p.recordRoles(ctx, receipts)
	if err != nil {
		return 0, err
	}
	if err := p.run(ctx, receipts); err != nil {
		return 0, err
	}
	if changed || p.poolGap {
		// Re-elect only when receipt roles or an unresolved pool can differ.
		raw := p
		raw.prefix = append(bytes.Clone(prefix), 'R')
		raw.replay, raw.families, raw.adds = true, 0, 0
		if err := raw.run(ctx, receipts); err != nil {
			return 0, err
		}
		if err := p.compare(ctx, &raw); err != nil {
			return 0, err
		}
	}
	if p.adds == 0 {
		return 0, nil
	}
	err = b.withWrites(func() error {
		return visitRebuildScratchKeyedEdges(ctx, b.pending, p.key('w'), 8, "C# interface dispatch output key", func(e *graph.Edge) error {
			return b.addEdgeWithCustody(e, rebuildEdgeCustody{proof: rebuildProofUntrackedMove})
		})
	})
	return p.adds, err
}

func (p *rebuildCSharpIface) run(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	if err := p.capture(ctx); err != nil {
		return err
	}
	hasArcs, err := rebuildGuardHasPrefix(ctx, p.b.pending, p.key('a'))
	if err != nil || !hasArcs {
		return err
	}
	if err := p.captureMembers(ctx); err != nil {
		return err
	}
	hasAnchors, err := rebuildGuardHasPrefix(ctx, p.b.pending, p.key('h'))
	if err != nil || !hasAnchors {
		return err
	}
	if err := p.record(ctx, receipts, false); err != nil {
		return err
	}
	if err := p.buildFamilies(ctx); err != nil || p.families == 0 {
		return err
	}
	if err := p.captureCalls(ctx); err != nil {
		return err
	}
	hasCalls, err := rebuildGuardHasPrefix(ctx, p.b.pending, p.key('s'))
	if err != nil || !hasCalls {
		return err
	}
	if err := p.record(ctx, receipts, true); err != nil {
		return err
	}
	return p.plan(ctx)
}

func (p *rebuildCSharpIface) compare(ctx context.Context, raw *rebuildCSharpIface) error {
	var refused rebuildScratchRefusals
	for _, stages := range [...][2]*rebuildCSharpIface{{p, raw}, {raw, p}} {
		prefix := stages[0].key('y')
		if err := p.visit(ctx, prefix, func(r row) error {
			key := r.key[len(prefix):]
			other, err := stages[1].get(append(stages[1].key('y'), key...))
			if err != nil || bytes.Equal(other, r.value) {
				return err
			}
			if other != nil && stages[0] == raw {
				return nil // The live comparison already named this key.
			}
			id, err := decodeLinkRecorder(key)
			if err == nil {
				refused.add(id, bytes.Clone(key))
			}
			return err
		}); err != nil {
			return err
		}
	}
	return refused.err()
}

func (p *rebuildCSharpIface) visit(ctx context.Context, prefix []byte, yield func(row) error) error {
	return pendingVisit(ctx, p.b.pending, prefix, yield)
}

func rebuildIfaceRawNodeKey(id string) []byte {
	return keyStrings(append(bytes.Clone(rebuildCSharpIfacePrefix), 'N'), id)
}

func rebuildIfaceRawNameKey(name string, ids ...string) []byte {
	return keyStrings(appendKeyString(append(bytes.Clone(rebuildCSharpIfacePrefix), 'B'), name), ids...)
}

func rebuildIfaceNodeRole(n *graph.Node) bool {
	return n != nil && (n.Kind == graph.KindMethod || n.Kind == graph.KindType || n.Kind == graph.KindInterface)
}

// Replay raw declaration roles independently of the live anchor and name
// indexes, including an ID whose receipt removes its former role.
func (p *rebuildCSharpIface) recordRoles(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) (bool, error) {
	for facts, err := range receipts {
		if err != nil {
			return false, err
		}
		if facts.EffectiveLanguage == "" {
			continue
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if n == nil || n.FilePath != file {
				continue
			}
			live, err := pendingGet(p.b.pending, pendingNodeKey(n.ID), decodeNodeLight)
			if err != nil {
				return false, err
			}
			if live == nil || !rebuildIfaceNodeRole(n) && !rebuildIfaceNodeRole(live) {
				continue
			}
			role := graph.Node{ID: n.ID, Name: n.Name, Kind: n.Kind, Language: n.Language, RepoPrefix: n.RepoPrefix,
				Meta: map[string]any{"iface_member": p.rules.Anchor(n)}}
			val, err := encodeNode(&role)
			if err != nil {
				return false, err
			}
			if err := p.put(rebuildIfaceRawNodeKey(n.ID), val); err != nil {
				return false, err
			}
		}
	}
	changed := false
	err := p.visit(ctx, append(bytes.Clone(rebuildCSharpIfacePrefix), 'N'), func(r row) error {
		n, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		live, err := pendingGet(p.b.pending, pendingNodeKey(n.ID), decodeNodeLight)
		if err != nil || live == nil {
			return err
		}
		if !changed {
			changed = n.Name != live.Name || n.Kind != live.Kind || n.Language != live.Language || n.RepoPrefix != live.RepoPrefix
			if !changed && live.Kind == graph.KindMethod && live.Language == "csharp" {
				live, err = p.node(n.ID)
				if err != nil || live == nil {
					return err
				}
				changed = p.rules.Anchor(n) != p.rules.Anchor(live)
			}
		}
		if n.Name != "" && p.rules.Candidate(n, n.RepoPrefix) {
			return p.put(rebuildIfaceRawNameKey(n.Name, n.ID), r.value)
		}
		return nil
	})
	return changed, err
}

func (p *rebuildCSharpIface) node(id string) (*graph.Node, error) {
	if p.replay {
		n, err := pendingGet(p.b.pending, rebuildIfaceRawNodeKey(id), decodeNode)
		if err != nil || n != nil {
			return n, err
		}
	}
	live := pendingNodeKey(id)
	return pendingGet(p.b.pending, live, func(v []byte) (*graph.Node, error) {
		light, err := decodeNodeLight(v)
		if err != nil || !rebuildIfaceNodeRole(light) {
			return light, err
		}
		if len(live)+len(v) > p.rowBytes {
			return nil, fmt.Errorf("%w: csharp interface dispatch node", ErrRebuildLinkInputSize)
		}
		n, err := decodeNode(v)
		if err == nil && n.ID != id {
			err = invalid("C# interface dispatch node identity")
		}
		return n, err
	})
}

// r requests a raw receipt; k binds the live key to it. Synthetic custody
// cannot serve as an original receipt. A partial fold retains the raw key.
func (p *rebuildCSharpIface) wantEdge(snap *pebble.Snapshot, live []byte) error {
	raw, err := rebuildReceiptKey(snap, live)
	if errors.Is(err, ErrRebuildUnknownProvenance) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := p.put(append(p.key('k'), live...), raw); err != nil {
		return err
	}
	return p.put(append(p.key('r'), raw...), []byte{0})
}

func (p *rebuildCSharpIface) wantNode(id string) error {
	known, err := p.present(rebuildIfaceRawNodeKey(id))
	if err != nil {
		return err
	}
	return p.put(p.key('n', id), []byte{rebuildScratchProofBit(known)})
}

func (p *rebuildCSharpIface) capture(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || (kind != graph.EdgeExtends && kind != graph.EdgeImplements) {
			return err
		}
		e, err := p.decodeKeyedEdge(r.key, r.value)
		if err != nil || !p.rules.Declared(e) {
			return err
		}
		indexed := false
		add := func(parent string, certain bool) error {
			if !indexed {
				if err := p.input(r.key, r.value); err != nil {
					return err
				}
				if err := p.wantEdge(snap, r.key); err != nil {
					return err
				}
				indexed = true
			}
			return p.put(p.key('a', parent, e.From, string(r.key)), []byte{rebuildIfaceFlag(!certain)})
		}
		if graph.IsUnresolvedTarget(e.To) {
			return p.resolveParents(ctx, e.From, graph.UnresolvedName(e.To), add)
		}
		return add(e.To, true)
	})
}

func rebuildIfaceFlag(v bool) byte {
	if v {
		return 1
	}
	return 0
}

func (p *rebuildCSharpIface) resolveParents(ctx context.Context, child, name string, visit func(string, bool) error) error {
	if name == "" {
		return nil
	}
	from, err := p.node(child)
	if err != nil || from == nil || from.Language != "csharp" {
		return err
	}
	fromExact, err := p.present(rebuildIfaceRawNodeKey(child))
	if err != nil {
		return err
	}
	var chosen string
	var count, optional int
	add := func(n *graph.Node, exact bool) error {
		if !p.rules.Candidate(n, from.RepoPrefix) {
			return nil
		}
		if exact {
			chosen = n.ID
			count++
		} else {
			optional++
		}
		return nil
	}
	if p.replay {
		if err := p.visit(ctx, rebuildIfaceRawNameKey(name), func(r row) error {
			n, err := decodeNode(r.value)
			if err != nil {
				return err
			}
			return add(n, true)
		}); err != nil {
			return err
		}
	}
	liveCandidates := func(yield func(*graph.Node, bool) error) error {
		return p.b.RebuildView().VisitNameCandidates(ctx, name, []string{"csharp"}, func(n *graph.Node) error {
			if !p.rules.Candidate(n, from.RepoPrefix) {
				return nil
			}
			exact, err := p.present(rebuildIfaceRawNodeKey(n.ID))
			if err != nil || p.replay && exact {
				return err
			}
			return yield(n, exact)
		})
	}
	if err := liveCandidates(func(n *graph.Node, exact bool) error {
		if !p.replay && !exact {
			p.poolGap = true
		}
		return add(n, !p.replay)
	}); err != nil {
		return err
	}
	if !p.replay {
		if count != 1 {
			return nil
		}
		key := pendingNodeKey(chosen)
		if _, err := pendingGet(p.b.pending, key, func(value []byte) (bool, error) {
			if len(key)+len(value) > p.rowBytes {
				return false, fmt.Errorf("%w: csharp interface dispatch parent node", ErrRebuildLinkInputSize)
			}
			return true, nil
		}); err != nil {
			return err
		}
		exact, err := p.present(rebuildIfaceRawNodeKey(chosen))
		if err != nil {
			return err
		}
		return visit(chosen, fromExact && exact)
	}
	switch count {
	case 0:
		// Any unproved declaration could be the sole remaining candidate.
		return liveCandidates(func(n *graph.Node, _ bool) error {
			return visit(n.ID, false)
		})
	case 1:
		return visit(chosen, fromExact && optional == 0)
	default:
		return nil // two known candidates always make the name ambiguous
	}
}

// Keep complete MemberOf sites, not only distinct methods. Native counts
// multiplicity toward the 128 cap and toward candidate_count.
func (p *rebuildCSharpIface) captureMembers(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || kind != graph.EdgeMemberOf {
			return err
		}
		e, err := decodeEdgeLight(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(pendingEdgeKey(e), r.key) {
			return invalid("C# interface dispatch membership key")
		}
		n, err := p.node(e.From)
		if err != nil || n == nil || n.Kind != graph.KindMethod {
			return err
		}
		if err := p.wantNode(n.ID); err != nil {
			return err
		}
		if err := p.wantEdge(snap, r.key); err != nil {
			return err
		}
		if err := p.put(p.key('m', e.To, n.Name, n.RepoPrefix, n.ID, string(r.key)), nil); err != nil {
			return err
		}
		if !p.rules.Anchor(n) || graph.IsUnresolvedTarget(e.To) {
			return nil
		}
		hasChildren, err := rebuildGuardHasPrefix(ctx, p.b.pending, p.key('a', e.To))
		if err != nil || !hasChildren {
			return err
		}
		if err := p.put(p.key('i', e.To, n.Name, n.ID, string(r.key)), nil); err != nil {
			return err
		}
		// SQLite's anchor projection explicitly orders by repo and complete
		// member site. Keep that order independently from the Call order.
		return p.put(append(p.key('h', n.RepoPrefix), r.key...), keyStrings(nil, e.To, n.Name, n.RepoPrefix))
	})
}

func (p *rebuildCSharpIface) wantCall(snap *pebble.Snapshot, live []byte) error {
	custody, err := readRebuildEdgeCustody(snap, live)
	if errors.Is(err, ErrRebuildUnknownProvenance) {
		return nil
	}
	if err != nil {
		return err
	}
	switch custody.proof {
	case rebuildProofTemporalSynthetic, rebuildProofEventChannel, rebuildProofRNBridge, rebuildProofObserver,
		rebuildProofClosureCollection, rebuildProofReactSetState, rebuildProofFlutterSetState, rebuildProofMediatRFanout:
		return p.put(append(p.key('S'), live...), nil)
	default:
		return p.wantEdge(snap, live)
	}
}

func (p *rebuildCSharpIface) captureCalls(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || kind != graph.EdgeCalls {
			return err
		}
		id, err := decodeLinkRecorder(r.key)
		if err != nil {
			return err
		}
		wanted, err := rebuildGuardHasPrefix(ctx, p.b.pending, p.key('f', id.To))
		if err != nil || !wanted {
			return err
		}
		e, err := p.decodeKeyedEdge(r.key, r.value)
		if err != nil || !p.rules.Source(e) {
			return err
		}
		if err := p.wantCall(snap, r.key); err != nil {
			return err
		}
		return p.put(append(p.key('s'), r.key...), nil)
	})
}

func (p *rebuildCSharpIface) record(ctx context.Context, receipts iter.Seq2[ParseFacts, error], calls bool) error {
	var ordinal uint64
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file, lang := pathkey.Normalize(facts.File.FilePath), facts.EffectiveLanguage
		for _, e := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			if (e.Kind == graph.EdgeCalls) != calls {
				ordinal++
				continue
			}
			key := append(p.key('r'), pendingEdgeKey(e)...)
			state, err := p.get(key)
			if err != nil {
				return err
			}
			if len(state) == 1 && lang != "" && e.FilePath == file &&
				((e.Kind != graph.EdgeExtends && e.Kind != graph.EdgeImplements) || p.rules.Declared(e)) {
				if err := p.put(key, binary.BigEndian.AppendUint64(nil, ordinal)); err != nil {
					return err
				}
			}
			ordinal++
		}
	}
	return nil
}

func (p *rebuildCSharpIface) edgeOrder(live []byte) ([]byte, error) {
	raw, err := p.get(append(p.key('k'), live...))
	if err != nil || raw == nil {
		return nil, err
	}
	// A kind promotion cannot prove a MemberOf or hierarchy declaration.
	rawKind, err := rebuildAttributionEdgeKind(raw)
	if err != nil {
		return nil, err
	}
	liveKind, err := rebuildAttributionEdgeKind(live)
	if err != nil || rawKind != liveKind {
		return nil, err
	}
	order, err := p.get(append(p.key('r'), raw...))
	if len(order) != 8 {
		return nil, err
	}
	return order, err
}

func (p *rebuildCSharpIface) nodeProved(id string) (bool, error) {
	state, err := rebuildScratchState(p.b.pending, p.key('n', id), "C# interface dispatch node proof")
	return state == rebuildCapabilityProved, err
}

func (p *rebuildCSharpIface) memberProved(id, site string) (bool, error) {
	good, err := p.nodeProved(id)
	if err != nil || !good {
		return false, err
	}
	order, err := p.edgeOrder([]byte(site))
	return order != nil, err
}

// The disk queue is reused per interface, so even an oversized descendant
// closure cannot accumulate in memory. Cache live/proved closure postings.
func (p *rebuildCSharpIface) closure(ctx context.Context, root string, proved bool) ([]byte, error) {
	mode := "l"
	if proved {
		mode = "p"
	}
	prefix := p.key('d', mode, root)
	done := p.key('x', mode, root)
	seen, err := p.present(done)
	if err != nil || seen {
		return prefix, err
	}
	queue := p.key('q')
	var count uint64
	enqueue := func(id string) error {
		key := appendKeyString(bytes.Clone(prefix), id)
		seen, err := p.present(key)
		if err != nil || seen {
			return err
		}
		if err := p.put(key, nil); err != nil {
			return err
		}
		if err := p.put(binary.BigEndian.AppendUint64(p.key('q'), count), []byte(id)); err != nil {
			return err
		}
		count++
		return nil
	}
	if err := enqueue(root); err != nil {
		return nil, err
	}
	for i := uint64(0); i < count; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		val, err := p.get(binary.BigEndian.AppendUint64(p.key('q'), i))
		if err != nil {
			return nil, err
		}
		parent := string(val)
		arcs := p.key('a', parent)
		if err := p.visit(ctx, arcs, func(r row) error {
			child, tail, err := readKeyString(r.key[len(arcs):])
			if err != nil {
				return err
			}
			if proved {
				key, _, err := readKeyString(tail)
				if err != nil {
					return err
				}
				order, err := p.edgeOrder([]byte(key))
				if err != nil || order == nil {
					return err
				}
				if r.value[0] != 0 {
					return nil // unresolved parent is only possible
				}
			}
			return enqueue(child)
		}); err != nil {
			return nil, err
		}
	}
	return prefix, errors.Join(p.b.pending.DeleteRange(queue, prefixEnd(queue), pebble.NoSync), p.put(done, nil))
}

var errRebuildIfaceCap = errors.New("interface family over the native C# cap")

// Keep one disk posting per native family slot, including overloads.
func (p *rebuildCSharpIface) buildFamilies(ctx context.Context) error {
	return p.visit(ctx, p.key('h'), func(r row) error {
		iface, tail, err := readKeyString(r.value)
		if err != nil {
			return err
		}
		name, tail, err := readKeyString(tail)
		if err != nil {
			return err
		}
		repo, _, err := readKeyString(tail)
		if err != nil {
			return err
		}
		group := p.key('g', iface, name)
		seen, err := p.present(group)
		if err != nil || seen {
			return err
		}
		if err := p.put(group, nil); err != nil {
			return err
		}
		return p.buildFamily(ctx, iface, name, repo)
	})
}

func (p *rebuildCSharpIface) buildFamily(ctx context.Context, iface, name, repo string) error {
	types, err := p.closure(ctx, iface, false)
	if err != nil {
		return err
	}
	provedTypes, err := p.closure(ctx, iface, true)
	if err != nil {
		return err
	}
	ordinal := binary.BigEndian.AppendUint64(nil, p.families)
	postings := append(p.key('F'), ordinal...)
	count, guaranteed := 0, 0
	add := func(id string, exact bool) error {
		if exact {
			guaranteed++
			if guaranteed > p.rules.Cap {
				return errRebuildIfaceCap
			}
		}
		if err := p.put(binary.BigEndian.AppendUint64(bytes.Clone(postings), uint64(count)), []byte(id)); err != nil {
			return err
		}
		count++
		return nil
	}
	anchor := p.key('i', iface, name)
	enqueue := func(r row, prefix []byte, path bool) error {
		id, tail, err := readKeyString(r.key[len(prefix):])
		if err != nil {
			return err
		}
		site, _, err := readKeyString(tail)
		if err != nil {
			return err
		}
		ok, err := p.memberProved(id, site)
		if err != nil {
			return err
		}
		return add(id, path && ok)
	}
	if err := p.visit(ctx, anchor, func(r row) error {
		return enqueue(r, anchor, true)
	}); err != nil {
		if errors.Is(err, errRebuildIfaceCap) {
			return p.b.pending.DeleteRange(postings, prefixEnd(postings), pebble.NoSync)
		}
		return err
	}
	anchorCount := count
	if err := p.visit(ctx, types, func(r row) error {
		owner, _, err := readKeyString(r.key[len(types):])
		if err != nil || owner == iface {
			return err
		}
		path, err := p.present(appendKeyString(bytes.Clone(provedTypes), owner))
		if err != nil {
			return err
		}
		prefix := p.key('m', owner, name, repo)
		return p.visit(ctx, prefix, func(r row) error {
			id, _, err := readKeyString(r.key[len(prefix):])
			if err != nil {
				return err
			}
			isAnchor, err := rebuildGuardHasPrefix(ctx, p.b.pending, appendKeyString(bytes.Clone(anchor), id))
			if err != nil || isAnchor {
				return err
			}
			return enqueue(r, prefix, path)
		})
	}); err != nil {
		if errors.Is(err, errRebuildIfaceCap) {
			return p.b.pending.DeleteRange(postings, prefixEnd(postings), pebble.NoSync)
		}
		return err
	}
	if count == anchorCount {
		return p.b.pending.DeleteRange(postings, prefixEnd(postings), pebble.NoSync)
	}
	// An uncertain posting is not absent. In particular, a family with
	// a proved lower count <= cap and a possible count > cap can be active.
	// Keep its candidate keys on disk so only a free affected Call refuses.
	p.families++
	val := keyStrings([]byte{rebuildIfaceFlag(guaranteed == count)}, iface)
	val = binary.BigEndian.AppendUint64(val, uint64(min(count, p.rules.Cap)-1))
	if err := p.put(append(p.key('v'), ordinal...), val); err != nil {
		return err
	}
	return p.visit(ctx, postings, func(r row) error {
		return p.put(append(p.key('f', string(r.value)), ordinal...), nil)
	})
}

func (p *rebuildCSharpIface) plan(ctx context.Context) error {
	prefix := p.key('s')
	if err := p.visit(ctx, prefix, func(r row) error {
		live := r.key[len(prefix):]
		order, err := p.edgeOrder(live)
		if err != nil || order == nil {
			return err
		}
		return p.put(append(append(p.key('c'), order...), live...), nil)
	}); err != nil {
		return err
	}
	prefix = p.key('c')
	if err := p.visit(ctx, prefix, func(r row) error {
		return p.planCall(ctx, r.key[len(prefix)+8:], true)
	}); err != nil {
		return err
	}
	prefix = p.key('s')
	if err := p.visit(ctx, prefix, func(r row) error {
		live := r.key[len(prefix):]
		order, err := p.edgeOrder(live)
		if err != nil || order != nil {
			return err
		}
		proved, err := p.present(append(p.key('S'), live...))
		if err != nil {
			return err
		}
		return p.planCall(ctx, live, proved)
	}); err != nil {
		return err
	}
	return p.refused.err()
}

func (p *rebuildCSharpIface) planCall(ctx context.Context, live []byte, proved bool) error {
	id, err := decodeLinkRecorder(live)
	if err != nil {
		return err
	}
	fams := p.key('f', id.To)
	wanted, err := rebuildGuardHasPrefix(ctx, p.b.pending, fams)
	if err != nil || !wanted {
		return err
	}
	e, err := p.edge(live)
	if err != nil || e == nil {
		return err
	}
	return p.visit(ctx, fams, func(r row) error {
		ordinal := r.key[len(fams):]
		if e.Origin == graph.OriginTextMatched {
			ok, err := p.present(append(p.key('f', e.From), ordinal...))
			if err != nil || !ok {
				return err
			}
		}
		val, err := p.get(append(p.key('v'), ordinal...))
		if err != nil {
			return err
		}
		if len(val) < 2 {
			return invalid("C# interface dispatch family missing")
		}
		famProved := val[0] != 0
		iface, tail, err := readKeyString(val[1:])
		if err != nil {
			return err
		}
		if len(tail) != 8 {
			return invalid("csharp interface dispatch family count")
		}
		fanout := int(binary.BigEndian.Uint64(tail))
		return p.visit(ctx, append(p.key('F'), ordinal...), func(member row) error {
			id := string(member.value)
			if id == e.From || id == e.To {
				return nil
			}
			return p.offer(p.rules.Emit(e, id, iface, fanout), proved, famProved)
		})
	})
}

func (p *rebuildCSharpIface) offer(e *graph.Edge, callProved, familyProved bool) error {
	key := pendingEdgeKey(e)
	occupied, err := p.present(key)
	if err != nil || occupied {
		return err // SQLite INSERT OR IGNORE also keeps a speculative incumbent.
	}
	seen := append(p.key('y'), key...)
	prior, err := p.get(seen)
	if err != nil || (prior != nil && callProved) {
		return err
	}
	raw, err := encodeEdge(e)
	if err != nil {
		return err
	}
	if !callProved || !familyProved {
		if !bytes.Equal(raw, prior) {
			p.refused.add(graph.EdgeIdentityFor(e), key)
		}
		return nil
	}
	if err := p.put(seen, raw); err != nil {
		return err
	}
	order := binary.BigEndian.AppendUint64(p.key('w'), p.adds)
	p.adds++
	return p.put(append(order, key...), raw)
}

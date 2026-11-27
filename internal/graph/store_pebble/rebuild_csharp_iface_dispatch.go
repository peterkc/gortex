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
	if err := p.capture(ctx); err != nil {
		return 0, err
	}
	hasArcs, err := rebuildGuardHasPrefix(ctx, b.pending, p.key('a'))
	if err != nil || !hasArcs {
		return 0, err
	}
	if err := p.captureMembers(ctx); err != nil {
		return 0, err
	}
	hasAnchors, err := rebuildGuardHasPrefix(ctx, b.pending, p.key('h'))
	if err != nil || !hasAnchors {
		return 0, err
	}
	if err := p.captureCalls(ctx); err != nil {
		return 0, err
	}
	hasCalls, err := rebuildGuardHasPrefix(ctx, b.pending, p.key('s'))
	if err != nil || !hasCalls {
		return 0, err
	}
	if err := p.record(ctx, receipts); err != nil {
		return 0, err
	}
	if err := p.buildFamilies(ctx); err != nil || p.families == 0 {
		return 0, err
	}
	if err := p.plan(ctx); err != nil || p.adds == 0 {
		return 0, err
	}
	err = b.withWrites(func() error {
		return visitRebuildScratchKeyedEdges(ctx, b.pending, p.key('w'), 8, "C# interface dispatch output key", func(e *graph.Edge) error {
			return b.addEdgeWithCustody(e, rebuildEdgeCustody{proof: rebuildProofUntrackedMove})
		})
	})
	return p.adds, err
}

func (p *rebuildCSharpIface) visit(ctx context.Context, prefix []byte, yield func(row) error) error {
	return pendingVisit(ctx, p.b.pending, prefix, yield)
}

func (p *rebuildCSharpIface) node(id string) (*graph.Node, error) {
	return pendingGet(p.b.pending, pendingNodeKey(id), func(v []byte) (*graph.Node, error) {
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
	return p.put(p.key('n', id), []byte{0})
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
		parent := e.To
		if graph.IsUnresolvedTarget(parent) {
			parent, err = p.resolveParent(ctx, e.From, graph.UnresolvedName(parent))
			if err != nil || parent == "" {
				return err
			}
		}
		if err := p.input(r.key, r.value); err != nil {
			return err
		}
		if err := p.wantEdge(snap, r.key); err != nil {
			return err
		}
		return p.put(p.key('a', parent, e.From, string(r.key)), []byte{rebuildIfaceFlag(parent != e.To)})
	})
}

func rebuildIfaceFlag(v bool) byte {
	if v {
		return 1
	}
	return 0
}

func (p *rebuildCSharpIface) resolveParent(ctx context.Context, child, name string) (string, error) {
	if name == "" {
		return "", nil
	}
	from, err := p.node(child)
	if err != nil || from == nil || from.Language != "csharp" {
		return "", err
	}
	var id string
	ambiguous := false
	err = p.b.RebuildView().VisitNameCandidates(ctx, name, []string{"csharp"}, func(n *graph.Node) error {
		if p.rules.Candidate(n, from.RepoPrefix) {
			ambiguous = ambiguous || id != ""
			id = n.ID
		}
		return nil
	})
	if err != nil || ambiguous || id == "" {
		return "", err
	}
	if err := p.wantNode(child); err != nil {
		return "", err
	}
	return id, p.wantNode(id)
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
		if err := p.put(p.key('j', n.ID), []byte{1}); err != nil {
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
		wanted, err := p.present(p.key('j', id.To))
		if err != nil || !wanted {
			return err
		}
		e, err := p.decodeKeyedEdge(r.key, r.value)
		if err != nil || !p.rules.Source(e) {
			return err
		}
		if err := p.wantEdge(snap, r.key); err != nil {
			return err
		}
		return p.put(append(p.key('s'), r.key...), nil)
	})
}

func (p *rebuildCSharpIface) record(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
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
			key := p.key('n', n.ID)
			wanted, err := p.present(key)
			if err != nil {
				return err
			}
			if !wanted {
				continue
			}
			live, err := p.node(n.ID)
			if err != nil {
				return err
			}
			good := lang != "" && sameRebuildLanguageMarker(n, live, file) && n.Kind == live.Kind && n.Name == live.Name &&
				n.RepoPrefix == live.RepoPrefix && p.rules.Anchor(n) == p.rules.Anchor(live)
			state, err := rebuildScratchState(p.b.pending, key, "C# interface dispatch node proof")
			if err != nil {
				return err
			}
			if err := noteRebuildScratchState(p.put, key, state, rebuildScratchProofBit(good)); err != nil {
				return err
			}
		}
		for _, e := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
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
					for _, id := range [...]string{child, parent} {
						ok, err := p.nodeProved(id)
						if err != nil || !ok {
							return err
						}
					}
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

// Only one family is retained in memory. Its member count and total ID
// bytes are bounded. Keep one slot per native posting, including overloads.
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
	members := make([]string, 0, p.rules.Cap)
	anchors := make(map[string]bool)
	good := true
	remaining := rebuildCSharpIfaceRowBytes
	add := func(id string) error {
		if len(members) == p.rules.Cap {
			return errRebuildIfaceCap
		}
		if len(id) > remaining {
			return fmt.Errorf("%w: C# interface family IDs", ErrRebuildLinkInputSize)
		}
		remaining -= len(id)
		members = append(members, id)
		return nil
	}
	prefix := p.key('i', iface, name)
	if err := p.visit(ctx, prefix, func(r row) error {
		id, tail, err := readKeyString(r.key[len(prefix):])
		if err != nil {
			return err
		}
		if err := add(id); err != nil {
			return err
		}
		anchors[id] = true
		site, _, err := readKeyString(tail)
		if err != nil {
			return err
		}
		ok, err := p.memberProved(id, site)
		good = good && ok
		return err
	}); err != nil {
		if errors.Is(err, errRebuildIfaceCap) {
			return nil
		}
		return err
	}
	anchorCount := len(members)
	provedTypes, err := p.closure(ctx, iface, true)
	if err != nil {
		return err
	}
	if err := p.visit(ctx, types, func(r row) error {
		owner, _, err := readKeyString(r.key[len(types):])
		if err != nil || owner == iface {
			return err
		}
		prefix := p.key('m', owner, name, repo)
		return p.visit(ctx, prefix, func(r row) error {
			id, tail, err := readKeyString(r.key[len(prefix):])
			if err != nil || anchors[id] {
				return err
			}
			if err := add(id); err != nil {
				return err
			}
			site, _, err := readKeyString(tail)
			if err != nil {
				return err
			}
			path, err := p.present(appendKeyString(bytes.Clone(provedTypes), owner))
			if err != nil {
				return err
			}
			ok, err := p.memberProved(id, site)
			good = good && path && ok
			return err
		})
	}); err != nil {
		if errors.Is(err, errRebuildIfaceCap) {
			return nil
		}
		return err
	}
	if len(members) == anchorCount {
		return nil
	}
	ordinal := binary.BigEndian.AppendUint64(nil, p.families)
	p.families++
	val := keyStrings([]byte{rebuildIfaceFlag(good)}, iface)
	for _, id := range members {
		val = appendKeyString(val, id)
		if err := p.put(append(p.key('f', id), ordinal...), nil); err != nil {
			return err
		}
	}
	return p.put(append(p.key('v'), ordinal...), val)
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
		return p.planCall(ctx, live, false)
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
		var members []string
		for len(tail) > 0 {
			var id string
			id, tail, err = readKeyString(tail)
			if err != nil {
				return err
			}
			members = append(members, id)
		}
		for _, id := range members {
			if id != e.From && id != e.To {
				if err := p.offer(p.rules.Emit(e, id, iface, len(members)-1), proved, famProved); err != nil {
					return err
				}
			}
		}
		return nil
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

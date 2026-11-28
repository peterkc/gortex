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

var ErrRebuildFactoryChain = errors.New("pebble rebuild factory chain failed")

var rebuildFactoryChainPrefix = []byte{'g', 'q', 46}

// RebuildFactoryChainLookup supplies the native walk with exact-name, point,
// and one-hop conformance reads from one projection.
type RebuildFactoryChainLookup struct {
	Nodes    func(name string, role func(*graph.Node) bool) []*graph.Node
	Incoming func(string) []*graph.Edge
	Node     func(string) *graph.Node
}

type RebuildFactoryChainRules struct {
	Call   func(*graph.Edge) (expr, method string)
	Target func(RebuildFactoryChainLookup, string, string, string) (string, bool)
	Rebind func(*graph.Edge, string, bool) graph.EdgeReindex
}

type rebuildFactoryChain struct {
	rebuildCallMoves
	rules RebuildFactoryChainRules
}

type rebuildFactoryChainQuery struct{ expr, method string }

func (p *rebuildFactoryChain) query(e *graph.Edge) rebuildFactoryChainQuery {
	expr, method := p.rules.Call(e)
	if expr == "" || method == "" {
		return rebuildFactoryChainQuery{}
	}
	return rebuildFactoryChainQuery{expr, method}
}

func rebuildFactoryChainColumns(b *BuildStore) rebuildCallMoves {
	return rebuildCallMoves{b: b, prefix: rebuildFactoryChainPrefix, rowBytes: 16 << 20, label: "factory chain", inputRow: "input row"}
}

// FreezeFactoryChainInputs captures the native pre-loop Calls and References.
func (b *BuildStore) FreezeFactoryChainInputs(ctx context.Context, match func(graph.FrameworkCensusEdge) bool) error {
	return b.freezeConventionInputs(ctx, rebuildFactoryChainColumns(b), ErrRebuildFactoryChain, match)
}

func (b *BuildStore) RebuildFactoryChain(ctx context.Context, receipts iter.Seq2[ParseFacts, error], rules RebuildFactoryChainRules) (moves uint64, err error) {
	defer func() {
		if err != nil {
			moves = 0
			err = fmt.Errorf("%w: %w", ErrRebuildFactoryChain, err)
		}
	}()
	if receipts == nil || rules.Call == nil || rules.Target == nil || rules.Rebind == nil {
		return 0, invalid("factory chain inputs")
	}
	cols := rebuildFactoryChainColumns(b)
	frozen, err := cols.present(cols.column('I'))
	if err != nil {
		return 0, err
	}
	if !frozen {
		return 0, invalid("factory chain census missing")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(cols.prefix, prefixEnd(cols.prefix), pebble.NoSync))
	}()
	active, err := rebuildGuardHasPrefix(ctx, b.pending, cols.column('i'))
	if err != nil || !active {
		return 0, err
	}
	p := rebuildFactoryChain{rebuildCallMoves: cols, rules: rules}
	if err := p.capture(ctx); err != nil {
		return 0, err
	}
	active, err = rebuildGuardHasPrefix(ctx, b.pending, p.column('h'))
	if err != nil || !active {
		return 0, err
	}
	if err := p.record(ctx, receipts); err != nil {
		return 0, err
	}
	if err := p.captureNames(ctx); err != nil {
		return 0, err
	}
	moves, err = p.plan(ctx)
	if err != nil || moves == 0 {
		return 0, err
	}
	if err := p.order(ctx); err != nil {
		return 0, err
	}
	return moves, b.withWrites(func() error { return p.apply(ctx, moves) })
}

func (p *rebuildFactoryChain) visit(ctx context.Context, prefix []byte, visit func(row) error) error {
	return pendingVisit(ctx, p.b.pending, prefix, visit)
}

func canNameFactoryChain(kind graph.NodeKind) bool {
	switch kind {
	case graph.KindType, graph.KindInterface, graph.KindMethod, graph.KindFunction:
		return true
	default:
		return false
	}
}

func (p *rebuildFactoryChain) captureNames(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		n, err := checkedRebuildLightNode(r, p.label)
		if err != nil || !canNameFactoryChain(n.Kind) {
			return err
		}
		return p.put(p.key('n', n.Name, n.ID), nil)
	})
}

func (p *rebuildFactoryChain) capture(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		id, err := decodeLinkRecorder(r.key)
		if err != nil {
			return err
		}
		switch id.Kind {
		case graph.EdgeImplements, graph.EdgeExtends:
			return p.captureArc(snap, id, r.key)
		case graph.EdgeCalls, graph.EdgeReferences:
		default:
			return nil
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
		frozen, err := p.present(append(p.column('i'), r.key...))
		if err != nil || !frozen || !graph.IsUnresolvedTarget(id.To) {
			return err
		}
		e, err := p.decodeKeyedEdge(r.key, r.value)
		if err != nil || p.query(e).expr == "" {
			return err
		}
		if err := p.input(r.key, r.value); err != nil {
			return err
		}
		return p.put(append(p.column('h'), r.key...), raw)
	})
}

func (p *rebuildFactoryChain) captureArc(snap *pebble.Snapshot, id graph.EdgeIdentity, key []byte) error {
	if err := p.put(append(p.key('a', id.To), key...), nil); err != nil {
		return err
	}
	if id.Kind == graph.EdgeImplements {
		stamped, err := rebuildImplementsStamped(snap, key)
		if err != nil {
			return err
		}
		if stamped {
			return p.put(append(p.key('A', id.To), key...), nil)
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
		if id.Kind == graph.EdgeImplements && custody.proof == rebuildProofKMPExpectActual {
			return p.put(append(p.key('A', id.To), key...), nil)
		}
		return nil
	}
	raw := custody.raw
	if raw == nil {
		raw = key
	}
	return p.claimKey(append(p.column('u'), raw...), key)
}

func (p *rebuildFactoryChain) record(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	var ordinal uint64
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
			if n.FilePath != file {
				continue
			}
			if err := p.recordNode(n); err != nil {
				return err
			}
		}
		for _, e := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			if e.FilePath != file {
				continue
			}
			switch e.Kind {
			case graph.EdgeImplements, graph.EdgeExtends:
				if err := p.recordArc(e); err != nil {
					return err
				}
			case graph.EdgeCalls, graph.EdgeReferences:
				if err := p.recordCall(e, ordinal); err != nil {
					return err
				}
				ordinal++
			}
		}
	}
	return ctx.Err()
}

func (p *rebuildFactoryChain) recordNode(n *graph.Node) error {
	known, err := p.present(pendingNodeKey(n.ID))
	if err != nil || !known {
		return err
	}
	key := p.key('P', n.ID)
	prior, err := pendingGet(p.b.pending, key, decodeNode)
	if err != nil {
		return err
	}
	if prior != nil && canNameFactoryChain(prior.Kind) {
		if err := p.b.pending.Delete(p.key('N', prior.Name, n.ID), pebble.NoSync); err != nil {
			return err
		}
	}
	projection := &graph.Node{ID: n.ID, Kind: n.Kind, Name: n.Name}
	if canNameFactoryChain(n.Kind) {
		projection.Meta = make(map[string]any, 2)
		if receiver, ok := n.Meta["receiver"]; ok {
			value, _ := receiver.(string)
			projection.Meta["receiver"] = value
		}
		if rt, _ := n.Meta["return_type"].(string); rt != "" {
			projection.Meta["return_type"] = rt
		}
		if err := p.put(p.key('N', n.Name, n.ID), nil); err != nil {
			return err
		}
	}
	val, err := encodeNode(projection)
	if err != nil {
		return err
	}
	return p.put(key, val)
}

func (p *rebuildFactoryChain) recordArc(e *graph.Edge) error {
	raw := pendingEdgeKey(e)
	claim, err := p.marked(append(p.column('u'), raw...))
	if err != nil || len(claim) == 1 {
		return err
	}
	if len(claim) > 1 {
		id, err := decodeLinkRecorder(claim[1:])
		if err != nil || id.Kind != e.Kind || id.From != e.From || id.FilePath != e.FilePath || id.Line != e.Line {
			return err
		}
		return p.put(append(p.key('A', id.To), claim[1:]...), nil)
	}
	// A raw arc can enter the conformance lookup even if it is not kept
	// live. Keep it only between known IDs; the two elections decide impact.
	for _, id := range [...]string{e.From, e.To} {
		known, err := p.present(pendingNodeKey(id))
		if err != nil || !known {
			return err
		}
	}
	return p.put(append(p.key('A', e.To), raw...), nil)
}

func (p *rebuildFactoryChain) recordCall(e *graph.Edge, ordinal uint64) error {
	raw := pendingEdgeKey(e)
	claim, err := p.marked(append(p.column('r'), raw...))
	if err != nil || len(claim) <= 1 {
		return err
	}
	old := claim[1:]
	id, err := decodeLinkRecorder(old)
	if err != nil || id.Kind != e.Kind || id.From != e.From || id.FilePath != e.FilePath || id.Line != e.Line || !graph.IsUnresolvedTarget(id.To) {
		return err
	}
	active, err := p.present(append(p.column('h'), old...))
	if err != nil || !active {
		return err
	}
	key := append(p.column('H'), old...)
	seen, err := p.present(key)
	if err != nil || seen {
		return err
	}
	q := p.query(e)
	if err := p.put(key, keyStrings(nil, q.expr, q.method)); err != nil {
		return err
	}
	if q.expr == "" {
		return nil
	}
	if err := p.put(append(p.column('h'), old...), raw); err != nil {
		return err
	}
	// SQLite's kind scan orders Calls before References, then rowid.
	kind := byte(0)
	if e.Kind == graph.EdgeReferences {
		kind = 1
	}
	return p.put(binary.BigEndian.AppendUint64(append(p.column('z'), kind), ordinal), raw)
}

func (p *rebuildFactoryChain) rawQuery(old []byte) (rebuildFactoryChainQuery, error) {
	return pendingGet(p.b.pending, append(p.column('H'), old...), func(value []byte) (rebuildFactoryChainQuery, error) {
		expr, rest, err := readKeyString(value)
		if err != nil {
			return rebuildFactoryChainQuery{}, err
		}
		method, rest, err := readKeyString(rest)
		if err != nil || len(rest) != 0 {
			return rebuildFactoryChainQuery{}, invalid("factory chain raw query")
		}
		return rebuildFactoryChainQuery{expr, method}, nil
	})
}

// The live lookup also records uncertainty: every unproved fact that enters
// a queried role might be retained or absent. Comparing only the two complete
// projections can miss a winner when several omitted rows interact.
type rebuildFactoryChainRead struct {
	pass    *rebuildFactoryChain
	ctx     context.Context
	proved  bool
	unknown bool
	budget  int
	err     error
}

func (r *rebuildFactoryChainRead) take(key, value []byte) error {
	cost := len(key) + len(value)
	if cost > r.budget {
		return fmt.Errorf("%w: factory chain lookup", ErrRebuildLinkInputSize)
	}
	r.budget -= cost
	return nil
}

func (r *rebuildFactoryChainRead) load(id string, role func(*graph.Node) bool) (*graph.Node, error) {
	p := r.pass
	key := pendingNodeKey(id)
	if r.proved {
		key = p.key('P', id)
	}
	return pendingGet(p.b.pending, key, func(value []byte) (*graph.Node, error) {
		n, err := decodeNode(value)
		if err != nil {
			return nil, err
		}
		if n.ID != id {
			return nil, invalid("factory chain lookup node identity")
		}
		if !role(n) {
			return nil, nil
		}
		if err := r.take(key, value); err != nil {
			return nil, err
		}
		if !r.proved {
			known, err := p.present(p.key('P', id))
			r.unknown = r.unknown || !known
			if err != nil {
				return nil, err
			}
		}
		return n, nil
	})
}

func (r *rebuildFactoryChainRead) Nodes(name string, role func(*graph.Node) bool) []*graph.Node {
	if r.err != nil {
		return nil
	}
	col := byte('n')
	if r.proved {
		col = 'N'
	}
	prefix := r.pass.key(col, name)
	var nodes []*graph.Node
	// Name-index keys use SQLite's ascending ID order within one name.
	r.err = r.pass.visit(r.ctx, prefix, func(item row) error {
		id, rest, err := readKeyString(item.key[len(prefix):])
		if err != nil || len(rest) != 0 {
			return invalid("factory chain name row")
		}
		n, err := r.load(id, role)
		if err != nil {
			return err
		}
		if n != nil {
			nodes = append(nodes, n)
		}
		return nil
	})
	return nodes
}

func (r *rebuildFactoryChainRead) Node(id string) *graph.Node {
	if r.err != nil {
		return nil
	}
	var n *graph.Node
	n, r.err = r.load(id, func(n *graph.Node) bool { return n.Name != "" })
	return n
}

func (r *rebuildFactoryChainRead) Incoming(id string) []*graph.Edge {
	if r.err != nil {
		return nil
	}
	p := r.pass
	col := byte('a')
	if r.proved {
		col = 'A'
	}
	prefix := p.key(col, id)
	var edges []*graph.Edge
	r.err = p.visit(r.ctx, prefix, func(item row) error {
		key := item.key[len(prefix):]
		arc, err := decodeLinkRecorder(key)
		if err != nil || arc.To != id {
			return errors.Join(err, invalid("factory chain incoming arc"))
		}
		if r.proved {
			if err := r.take(key, nil); err != nil {
				return err
			}
		} else {
			if _, err := pendingGet(p.b.pending, key, func(value []byte) (bool, error) {
				return true, r.take(key, value)
			}); err != nil {
				return err
			}
			known, err := p.present(append(p.key('A', id), key...))
			if err != nil {
				return err
			}
			r.unknown = r.unknown || !known
		}
		edges = append(edges, &graph.Edge{From: arc.From, To: arc.To, Kind: arc.Kind})
		return nil
	})
	return edges
}

func (p *rebuildFactoryChain) elect(ctx context.Context, q rebuildFactoryChainQuery, from string, proved bool) (id string, walked, unknown bool, err error) {
	if q.expr == "" {
		return "", false, false, nil
	}
	r := rebuildFactoryChainRead{pass: p, ctx: ctx, proved: proved, budget: p.rowBytes}
	lookup := RebuildFactoryChainLookup{Nodes: r.Nodes, Incoming: r.Incoming, Node: r.Node}
	id, walked = p.rules.Target(lookup, from, q.expr, q.method)
	return id, walked, r.unknown, r.err
}

func (p *rebuildFactoryChain) plan(ctx context.Context) (uint64, error) {
	var refused rebuildScratchRefusals
	var moves uint64
	prefix := p.column('h')
	if err := p.visit(ctx, prefix, func(item row) error {
		old := item.key[len(prefix):]
		e, err := p.edge(old)
		if err != nil {
			return err
		}
		if e == nil {
			return invalid("factory chain live edge missing")
		}
		q := p.query(e)
		live, walked, unknown, err := p.elect(ctx, q, e.From, false)
		if err != nil {
			return err
		}
		qRaw, err := p.rawQuery(old)
		if err != nil {
			return err
		}
		proved, rawWalked, _, err := p.elect(ctx, qRaw, e.From, true)
		if err != nil {
			return err
		}
		if unknown || live != proved || walked != rawWalked {
			refused.add(graph.EdgeIdentityFor(e), bytes.Clone(old))
			return nil
		}
		if live == "" {
			return nil
		}
		var claim []byte
		if len(item.value) != 0 {
			claim, err = p.marked(append(p.column('r'), item.value...))
			if err != nil {
				return err
			}
		}
		if q != qRaw || len(claim) <= 1 || !bytes.Equal(claim[1:], old) {
			refused.add(graph.EdgeIdentityFor(e), bytes.Clone(old))
			return nil
		}
		p.rules.Rebind(e, live, walked)
		moves++
		return p.stage(old, e)
	}); err != nil {
		return 0, err
	}
	return moves, refused.err()
}

func (p *rebuildFactoryChain) order(ctx context.Context) error {
	return p.visit(ctx, p.column('z'), func(item row) error {
		key := append(p.column('r'), item.value...)
		claim, err := p.marked(key)
		if err != nil || len(claim) <= 1 {
			return err
		}
		moving, err := p.present(append(p.column('m'), claim[1:]...))
		if err != nil || !moving {
			return err
		}
		return p.noteReceipt(key, claim, true)
	})
}

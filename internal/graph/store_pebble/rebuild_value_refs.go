package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
	"strings"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

var ErrRebuildValueRefs = errors.New("pebble rebuild value refs failed")

var rebuildValueRefsPrefix = []byte{'g', 'q', 49}

// RebuildValueRefsRules supplies the native read, naming and shadow policy.
type RebuildValueRefsRules struct {
	Name        func(*graph.Edge) string
	Distinctive func(string) bool
	Shadows     func(reader, local string) bool
	Generated   func(*graph.Node) bool
	Rebind      func(*graph.Edge, string, bool) (graph.EdgeReindex, bool)
}

type rebuildValueRefs struct {
	rebuildCallMoves
	rules RebuildValueRefsRules
}

// RebuildValueRefs follows the cold SQLite value-ref placeholder scan.
func (b *BuildStore) RebuildValueRefs(ctx context.Context, receipts iter.Seq2[ParseFacts, error], rules RebuildValueRefsRules) (moves uint64, err error) {
	defer func() {
		if err != nil {
			moves = 0
			err = fmt.Errorf("%w: %w", ErrRebuildValueRefs, err)
		}
	}()
	if receipts == nil || rules.Name == nil || rules.Distinctive == nil || rules.Shadows == nil || rules.Generated == nil || rules.Rebind == nil {
		return 0, invalid("value refs inputs")
	}
	prefix := rebuildValueRefsPrefix
	busy, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return 0, err
	}
	if busy {
		return 0, invalid("value refs scratch occupied")
	}
	defer func() { err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync)) }()
	p := rebuildValueRefs{
		rebuildCallMoves: rebuildCallMoves{b: b, prefix: prefix, rowBytes: 16 << 20, label: "value refs", inputRow: "input row"},
		rules:            rules,
	}
	if err := p.captureReads(ctx); err != nil {
		return 0, err
	}
	if err := p.recordReads(ctx, receipts); err != nil {
		return 0, err
	}
	active, err := rebuildGuardHasPrefix(ctx, b.pending, p.column('h'))
	if err != nil || !active {
		return 0, err
	}
	if err := p.captureNodes(ctx); err != nil {
		return 0, err
	}
	if err := p.recordNodes(ctx, receipts); err != nil {
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

func (p *rebuildValueRefs) visit(ctx context.Context, prefix []byte, visit func(row) error) error {
	return pendingVisit(ctx, p.b.pending, prefix, visit)
}

func valueRefPlaceholder(to string) bool {
	return strings.HasPrefix(to, "unresolved::valueref::")
}

func (p *rebuildValueRefs) want(id graph.EdgeIdentity, live, raw []byte, name string) error {
	if err := p.put(p.key('q', id.FilePath, name), nil); err != nil {
		return err
	}
	if err := p.put(p.key('g', id.From), nil); err != nil {
		return err
	}
	return p.put(append(p.column('h'), live...), raw)
}

func (p *rebuildValueRefs) captureReads(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		id, err := decodeLinkRecorder(r.key)
		if err != nil || id.Kind != graph.EdgeReads {
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
		if !valueRefPlaceholder(id.To) || id.FilePath == "" {
			return nil
		}
		e, err := p.decodeKeyedEdge(r.key, r.value)
		if err != nil {
			return err
		}
		name := p.rules.Name(e)
		if name == "" {
			return nil
		}
		if err := p.input(r.key, r.value); err != nil {
			return err
		}
		return p.want(id, r.key, raw, name)
	})
}

func (p *rebuildValueRefs) recordReads(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	var ordinal uint64
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, e := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			if facts.EffectiveLanguage == "" || e.Kind != graph.EdgeReads || e.FilePath != file {
				continue
			}
			if err := p.recordRead(e, ordinal); err != nil {
				return err
			}
			ordinal++
		}
	}
	return ctx.Err()
}

func (p *rebuildValueRefs) recordRead(e *graph.Edge, ordinal uint64) error {
	raw := pendingEdgeKey(e)
	claim, err := p.marked(append(p.column('r'), raw...))
	if err != nil || len(claim) <= 1 {
		return err
	}
	live := claim[1:]
	id, err := decodeLinkRecorder(live)
	if err != nil || id.Kind != e.Kind || id.From != e.From || id.FilePath != e.FilePath || id.Line != e.Line || !valueRefPlaceholder(id.To) {
		return err
	}
	key := append(p.column('H'), live...)
	seen, err := p.present(key)
	if err != nil || seen {
		return err
	}
	var name string
	if valueRefPlaceholder(e.To) {
		name = p.rules.Name(e)
	}
	if err := p.put(key, []byte(name)); err != nil || name == "" {
		return err
	}
	if err := p.want(id, live, raw, name); err != nil {
		return err
	}
	// Cold SQLite visits the to_id range in target order, then rowid.
	return p.put(binary.BigEndian.AppendUint64(p.key('z', id.To), ordinal), raw)
}

func (p *rebuildValueRefs) nodeRole(n *graph.Node, decl, local byte) []byte {
	switch n.Kind {
	case graph.KindConstant, graph.KindVariable:
		if p.rules.Distinctive(n.Name) {
			return p.key(decl, n.FilePath, n.Name, n.ID)
		}
	case graph.KindParam, graph.KindField, graph.KindLocal:
		return p.key(local, n.FilePath, n.Name, n.ID)
	}
	return nil
}

func (p *rebuildValueRefs) captureNodes(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		n, err := checkedRebuildLightNode(r, p.label)
		if err != nil || n.FilePath == "" {
			return err
		}
		key := p.nodeRole(n, 'd', 's')
		if key == nil {
			return nil
		}
		wanted, err := p.present(p.key('q', n.FilePath, n.Name))
		if err != nil || !wanted {
			return err
		}
		if key[len(p.prefix)] == 'd' {
			if err := p.input(r.key, r.value); err != nil {
				return err
			}
		}
		if err := p.put(p.key('i', n.ID), nil); err != nil {
			return err
		}
		return p.put(key, appendKeyLine(nil, n.StartLine))
	})
}

func (p *rebuildValueRefs) recordNodes(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if facts.EffectiveLanguage == "" || n.FilePath != file {
				continue
			}
			if err := p.recordNode(n); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

func (p *rebuildValueRefs) recordNode(n *graph.Node) error {
	reader, err := p.present(p.key('g', n.ID))
	if err != nil {
		return err
	}
	wanted, err := p.present(p.key('q', n.FilePath, n.Name))
	if err != nil {
		return err
	}
	state := p.key('p', n.ID)
	if !reader && !wanted {
		live, err := p.present(p.key('i', n.ID))
		if err != nil {
			return err
		}
		if !live {
			prior, err := p.present(state)
			if err != nil || !prior {
				return err
			}
		}
	}
	known, err := p.present(pendingNodeKey(n.ID))
	if err != nil || !known {
		return err
	}
	old, err := p.get(state)
	if err != nil {
		return err
	}
	if len(old) != 0 {
		if err := p.b.pending.Delete(old, pebble.NoSync); err != nil {
			return err
		}
	}
	var key []byte
	if wanted {
		key = p.nodeRole(n, 'D', 'S')
		if key != nil {
			if err := p.put(key, appendKeyLine(nil, n.StartLine)); err != nil {
				return err
			}
		}
	}
	if reader {
		if err := p.put(p.key('G', n.ID), []byte{rebuildScratchProofBit(p.rules.Generated(n))}); err != nil {
			return err
		}
	}
	return p.put(state, key)
}

type rebuildValueRefElection struct {
	id          string
	conditional bool
}

type rebuildValueRefFacts struct {
	winner  rebuildValueRefElection
	omitted bool
	shadow  bool
	maybe   bool
}

func (p *rebuildValueRefs) inputNode(id string) error {
	key := pendingNodeKey(id)
	_, err := pendingGet(p.b.pending, key, func(value []byte) (bool, error) {
		return true, p.input(key, value)
	})
	return err
}

func (p *rebuildValueRefs) facts(ctx context.Context, e *graph.Edge, name string, proved bool) (rebuildValueRefFacts, error) {
	var facts rebuildValueRefFacts
	if name == "" {
		return facts, nil
	}
	decl, local := byte('d'), byte('s')
	if proved {
		decl, local = 'D', 'S'
	}
	prefix := p.key(decl, e.FilePath, name)
	var line int
	if err := p.visit(ctx, prefix, func(r row) error {
		id, rest, err := readKeyString(r.key[len(prefix):])
		if err != nil || len(rest) != 0 || len(r.value) != 8 {
			return invalid("value refs declaration row")
		}
		if proved {
			if err := p.inputNode(id); err != nil {
				return err
			}
		} else {
			known, err := p.present(p.key('p', id))
			if err != nil {
				return err
			}
			facts.omitted = facts.omitted || !known
		}
		start := int(int64(binary.BigEndian.Uint64(r.value) ^ (1 << 63)))
		idSet := facts.winner.id != ""
		facts.winner.conditional = facts.winner.conditional || idSet
		// The SQLite file index supplies ascending IDs to the stable line
		// sort: choose the last preceding row, or the first future row.
		if !idSet || start <= e.Line && (line > e.Line || start >= line) || line > e.Line && start < line {
			facts.winner.id, line = id, start
		}
		return nil
	}); err != nil {
		return facts, err
	}
	prefix = p.key(local, e.FilePath, name)
	err := p.visit(ctx, prefix, func(r row) error {
		id, rest, err := readKeyString(r.key[len(prefix):])
		if err != nil || len(rest) != 0 {
			return invalid("value refs shadow row")
		}
		if !p.rules.Shadows(e.From, id) {
			return nil
		}
		if err := p.inputNode(id); err != nil {
			return err
		}
		facts.shadow = true
		if !proved {
			known, err := p.present(p.key('p', id))
			facts.maybe = facts.maybe || !known
			return err
		}
		return nil
	})
	return facts, err
}

func (p *rebuildValueRefs) reader(id string) (bool, error) {
	key := pendingNodeKey(id)
	return pendingGet(p.b.pending, key, func(value []byte) (bool, error) {
		if err := p.input(key, value); err != nil {
			return false, err
		}
		n, err := decodeNode(value)
		if err != nil || n.ID != id {
			return false, errors.Join(err, invalid("value refs reader identity"))
		}
		return p.rules.Generated(n), nil
	})
}

func (f rebuildValueRefFacts) decision(from string, generated bool) rebuildValueRefElection {
	if f.shadow || generated || f.winner.id == "" || f.winner.id == from {
		return rebuildValueRefElection{}
	}
	return f.winner
}

func (p *rebuildValueRefs) compare(ctx context.Context, e *graph.Edge, name, nameRaw string) (rebuildValueRefElection, bool, error) {
	live, err := p.facts(ctx, e, name, false)
	if err != nil {
		return rebuildValueRefElection{}, false, err
	}
	proved, err := p.facts(ctx, e, nameRaw, true)
	if err != nil {
		return rebuildValueRefElection{}, false, err
	}
	if live.winner.id == "" && proved.winner.id == "" {
		return rebuildValueRefElection{}, true, nil
	}
	generated, err := p.reader(e.From)
	if err != nil {
		return rebuildValueRefElection{}, false, err
	}
	mark, err := rebuildScratchState(p.b.pending, p.key('G', e.From), "value refs reader receipt")
	if err != nil {
		return rebuildValueRefElection{}, false, err
	}
	isRawGenerated := mark == rebuildCapabilityProved
	want, proof := live.decision(e.From, generated), proved.decision(e.From, isRawGenerated)
	// A definite raw blocker survives any combination of missing rows.
	// Otherwise an omitted declaration can decide, and an omitted shadow
	// or generated reader can block a declaration that might decide.
	unknown := !proved.shadow && !isRawGenerated && (live.omitted || (live.maybe || generated && mark == 0) && proof.id != "")
	return want, !unknown && want == proof, nil
}

func (p *rebuildValueRefs) plan(ctx context.Context) (uint64, error) {
	var moves uint64
	var refused rebuildScratchRefusals
	prefix := p.column('h')
	if err := p.visit(ctx, prefix, func(item row) error {
		old := item.key[len(prefix):]
		e, err := p.edge(old)
		if err != nil || e == nil {
			return errors.Join(err, invalid("value refs live edge missing"))
		}
		name := p.rules.Name(e)
		raw, err := p.get(append(p.column('H'), old...))
		if err != nil {
			return err
		}
		target, exact, err := p.compare(ctx, e, name, string(raw))
		if err != nil {
			return err
		}
		if !exact {
			refused.add(graph.EdgeIdentityFor(e), bytes.Clone(old))
			return nil
		}
		if target.id == "" {
			return nil
		}
		var claim []byte
		if len(item.value) != 0 {
			claim, err = p.marked(append(p.column('r'), item.value...))
			if err != nil {
				return err
			}
		}
		if len(claim) <= 1 || !bytes.Equal(claim[1:], old) || name != string(raw) {
			refused.add(graph.EdgeIdentityFor(e), bytes.Clone(old))
			return nil
		}
		if _, changed := p.rules.Rebind(e, target.id, target.conditional); !changed || e.To != target.id || bytes.Equal(old, pendingEdgeKey(e)) {
			return invalid("value refs move did not rebind")
		}
		moves++
		return p.stage(old, e)
	}); err != nil {
		return 0, err
	}
	return moves, refused.err()
}

func (p *rebuildValueRefs) order(ctx context.Context) error {
	return p.visit(ctx, p.column('z'), func(r row) error {
		key := append(p.column('r'), r.value...)
		claim, err := p.marked(key)
		if err != nil || len(claim) <= 1 {
			return err
		}
		return p.noteReceipt(key, claim, true)
	})
}

package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

var ErrRebuildEFCoreModels = errors.New("pebble rebuild EF Core models failed")

var rebuildEFCorePrefix = []byte{'g', 'q', 'E'}

const rebuildEFCoreBytes = 16 << 20

// RebuildEFCoreRules keeps mapping and payload decisions in the resolver.
type RebuildEFCoreRules struct {
	Fact     func(*graph.Node) (keep, active bool)
	Names    func(*graph.Node, func(string) error) error
	BareName func(string) string
	RoleNode func(*graph.Node) *graph.Node
	Owned    func(*graph.Edge) bool
	SameEdge func(*graph.Edge, *graph.Edge) bool
	Project  func([]*graph.Node) func(*graph.Node, []*graph.Edge) (*graph.Edge, bool)
	Table    func(*graph.Node, *graph.Edge) *graph.Node
}

type rebuildEFCore struct {
	rebuildCallMoves
	rules   RebuildEFCoreRules
	changed uint64
	refused rebuildScratchRefusals
}

// RebuildEFCoreModels plans exact private replacements against the settled
// graph. Only one repository/workspace and one set of siblings are retained.
func (b *BuildStore) RebuildEFCoreModels(ctx context.Context, receipts iter.Seq2[ParseFacts, error], rules RebuildEFCoreRules) (changed uint64, err error) {
	defer func() {
		if err != nil {
			changed = 0
			err = fmt.Errorf("%w: %w", ErrRebuildEFCoreModels, err)
		}
	}()
	if receipts == nil || rules.Fact == nil || rules.Names == nil || rules.BareName == nil || rules.RoleNode == nil ||
		rules.Owned == nil || rules.SameEdge == nil || rules.Project == nil || rules.Table == nil {
		return 0, invalid("ef core model inputs")
	}
	prefix := rebuildEFCorePrefix
	busy, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return 0, err
	}
	if busy {
		return 0, invalid("ef core scratch range occupied")
	}
	defer func() { err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync)) }()
	p := rebuildEFCore{
		rebuildCallMoves: rebuildCallMoves{b: b, prefix: prefix, rowBytes: rebuildEFCoreBytes, label: "EF Core models", inputRow: "row"},
		rules:            rules,
	}
	if err := p.recordNodes(ctx, receipts); err != nil {
		return 0, err
	}
	if err := p.captureNodes(ctx); err != nil {
		return 0, err
	}
	if err := p.captureEdges(ctx); err != nil {
		return 0, err
	}
	active, err := rebuildGuardHasPrefix(ctx, b.pending, p.key('a'))
	if err != nil || !active {
		return 0, err
	}
	if err := p.record(ctx, receipts); err != nil {
		return 0, err
	}
	if err := p.plan(ctx); err != nil || p.changed == 0 {
		return 0, err
	}
	return p.changed, b.withWrites(func() error { return p.apply(ctx) })
}

func (p *rebuildEFCore) visit(ctx context.Context, prefix []byte, yield func(row) error) error {
	return pendingVisit(ctx, p.b.pending, prefix, yield)
}

// p holds the raw roles (including a receipt that removes the live role).
// b/B index live/receipt views. m indexes only names mentioned by EF facts.
func (p *rebuildEFCore) recordNodes(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
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
			keep, _ := p.rules.Fact(n)
			if !keep {
				live, err := pendingGet(p.b.pending, pendingNodeKey(n.ID), decodeNodeLight)
				if err != nil {
					return err
				}
				if live == nil || !rebuildEFNodeKind(live.Kind) || live.FilePath != file {
					continue
				}
			}
			role, err := encodeNode(p.rules.RoleNode(n))
			if err != nil {
				return err
			}
			if err := p.put(p.key('p', n.ID), role); err != nil {
				return err
			}
		}
	}
	return nil
}

func rebuildEFNodeKind(kind graph.NodeKind) bool {
	return kind == graph.KindType || kind == graph.KindFile || kind == graph.KindField
}

func (p *rebuildEFCore) indexNode(n *graph.Node, view byte) error {
	keep, active := p.rules.Fact(n)
	if !keep {
		return nil
	}
	group := keyStrings(nil, n.RepoPrefix, n.WorkspaceID)
	data := []byte{0}
	if active {
		if err := p.put(append(p.key('a'), group...), nil); err != nil {
			return err
		}
		data[0] = 1
	}
	if err := p.rules.Names(n, func(name string) error {
		data[0] = 1 // a configuration can supply an assembly or named action
		return p.put(appendKeyString(append(p.key('m'), group...), name), nil)
	}); err != nil {
		return err
	}
	if n.Kind == graph.KindType {
		if err := p.put(p.key(view+1, n.ID), group); err != nil {
			return err
		}
		data = append(data, p.rules.BareName(n.Name)...)
	}
	return p.put(appendKeyString(append(p.key(view), group...), n.ID), data)
}

func (p *rebuildEFCore) captureNodes(ctx context.Context) error {
	if err := p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		n, err := checkedRebuildLightNode(r, p.label)
		if err != nil || !rebuildEFNodeKind(n.Kind) {
			return err
		}
		if n.Kind != graph.KindFile && !strings.EqualFold(n.Language, "csharp") {
			return nil
		}
		n, err = decodeNode(r.value)
		if err != nil {
			return err
		}
		if err := p.indexNode(n, 'b'); err != nil {
			return err
		}
		known, err := p.present(p.key('p', n.ID))
		if err != nil || known {
			return err
		}
		return p.indexNode(n, 'B')
	}); err != nil {
		return err
	}
	return p.visit(ctx, p.key('p'), func(r row) error {
		n, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		return p.indexNode(n, 'B')
	})
}

func (p *rebuildEFCore) captureEdges(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || kind != graph.EdgeModelsTable {
			return err
		}
		e, err := p.decodeKeyedEdge(r.key, r.value)
		if err != nil || !p.rules.Owned(e) {
			return err
		}
		owned := false
		for _, kind := range [...]byte{'c', 'C'} {
			group, err := p.get(p.key(kind, e.From))
			if err != nil {
				return err
			}
			if group != nil {
				owned = true
				if err := p.put(append(p.key('a'), group...), nil); err != nil {
					return err
				}
			}
		}
		if !owned {
			return nil
		}
		if err := p.input(r.key, r.value); err != nil {
			return err
		}
		if err := p.put(append(p.key('e', e.From), r.key...), nil); err != nil {
			return err
		}
		custody, err := readRebuildEdgeCustody(snap, r.key)
		if errors.Is(err, ErrRebuildUnknownProvenance) {
			return nil
		}
		if err != nil {
			return err
		}
		if custody.proof == rebuildProofEFCoreModel {
			return p.put(append(p.key('s'), r.key...), []byte{rebuildCapabilityProved})
		}
		if custody.proof != 0 {
			return nil
		}
		raw := custody.raw
		if raw == nil {
			raw = r.key
		}
		return p.addUnique(p.key('r', string(raw)), string(r.key))
	})
}

func (p *rebuildEFCore) proof(key []byte) (byte, error) {
	return rebuildScratchState(p.b.pending, key, "EF Core proof state")
}

func (p *rebuildEFCore) record(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file, lang := pathkey.Normalize(facts.File.FilePath), facts.EffectiveLanguage
		for _, raw := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			if raw == nil || raw.Kind != graph.EdgeModelsTable || raw.FilePath != file || lang == "" {
				continue
			}
			liveKey, _, err := p.unique(p.key('r', string(pendingEdgeKey(raw))))
			if err != nil {
				return err
			}
			if liveKey == "" {
				continue
			}
			live, err := p.b.pendingEdge([]byte(liveKey))
			if err != nil || live == nil {
				return errors.Join(err, invalid("ef core sibling disappeared"))
			}
			key := append(p.key('s'), liveKey...)
			state, err := p.proof(key)
			if err != nil {
				return err
			}
			ok := p.rules.Owned(raw) && p.rules.SameEdge(raw, live)
			if err := noteRebuildScratchState(p.put, key, state, rebuildScratchProofBit(ok)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *rebuildEFCore) plan(ctx context.Context) error {
	prefix := p.key('a')
	for _, view := range [...]byte{'b', 'B'} {
		if err := p.visit(ctx, prefix, func(r row) error {
			return p.planBoundary(ctx, r.key[len(prefix):], view)
		}); err != nil {
			return err
		}
		if err := p.refused.err(); err != nil {
			return err
		}
	}
	for _, sides := range [...][2]byte{{'x', 'X'}, {'X', 'x'}} {
		prefix := p.key(sides[0])
		if err := p.visit(ctx, prefix, func(r row) error {
			other, err := p.get(append(p.key(sides[1]), r.key[len(prefix):]...))
			if err != nil || bytes.Equal(other, r.value) || other != nil && sides[0] == 'X' {
				return err
			}
			id, _, err := readKeyString(r.key[len(prefix):])
			if err != nil {
				return err
			}
			if r.value[0] == 1 {
				e, err := decodeEdge(r.value[1:])
				if err == nil {
					p.refused.add(graph.EdgeIdentityFor(e), pendingEdgeKey(e))
				}
				return err
			}
			return p.visit(ctx, p.key('e', id), func(edge row) error {
				key := edge.key[len(p.key('e', id)):]
				identity, err := decodeLinkRecorder(key)
				if err == nil {
					p.refused.add(identity, bytes.Clone(key))
				}
				return err
			})
		}); err != nil {
			return err
		}
	}
	if err := p.refused.err(); err != nil {
		return err
	}
	// Equal edge deltas can still propose different node payloads for a
	// shared absent table, e.g. if its winning owner changed workspace.
	return p.compareTables(ctx)
}

func (p *rebuildEFCore) compareTables(ctx context.Context) error {
	for _, sides := range [...][2]byte{{'t', 'T'}, {'T', 't'}} {
		prefix := p.key(sides[0])
		if err := p.visit(ctx, prefix, func(r row) error {
			owner, node, err := readKeyString(r.value)
			if err != nil {
				return err
			}
			other, err := p.get(append(p.key(sides[1]), r.key[len(prefix):]...))
			if err != nil || other != nil && sides[0] == 'T' {
				return err
			}
			if other != nil {
				_, otherNode, err := readKeyString(other)
				if err != nil || bytes.Equal(node, otherNode) {
					return err
				}
			}
			out, err := p.get(p.key(sides[0]+('x'-'t'), owner))
			if err != nil || len(out) < 2 || out[0] != 1 {
				return errors.Join(err, invalid("ef core table owner missing output"))
			}
			edge, err := decodeEdge(out[1:])
			if err == nil {
				p.refused.add(graph.EdgeIdentityFor(edge), pendingEdgeKey(edge))
			}
			return err
		}); err != nil {
			return err
		}
	}
	return p.refused.err()
}

func takeRebuildEFBudget(remaining *int, key, val []byte) error {
	if len(key)+len(val) > *remaining {
		return fmt.Errorf("%w: EF Core reduction input", ErrRebuildLinkInputSize)
	}
	*remaining -= len(key) + len(val)
	return nil
}

func (p *rebuildEFCore) planBoundary(ctx context.Context, group []byte, view byte) error {
	var nodes, exact []*graph.Node
	known := map[string]bool{}
	remaining := rebuildEFCoreBytes
	prefix := append(p.key(view), group...)
	if err := p.visit(ctx, prefix, func(r row) error {
		id, _, err := readKeyString(r.key[len(prefix):])
		if err != nil || len(r.value) == 0 {
			return errors.Join(err, invalid("ef core input posting"))
		}
		if r.value[0] == 0 {
			named, err := p.present(appendKeyString(append(p.key('m'), group...), string(r.value[1:])))
			if err != nil {
				return err
			}
			owned, err := rebuildGuardHasPrefix(ctx, p.b.pending, p.key('e', id))
			if err != nil || !named && !owned {
				return err
			}
		}
		receipt, err := p.get(p.key('p', id))
		if err != nil {
			return err
		}
		decode := func(raw []byte) (*graph.Node, error) {
			if err := takeRebuildEFBudget(&remaining, r.key, raw); err != nil {
				return nil, err
			}
			return decodeNode(raw)
		}
		var rawNode, n *graph.Node
		if view == 'B' && receipt != nil {
			n, err = decode(receipt)
			rawNode = n
		} else {
			if receipt != nil {
				rawNode, err = decodeNodeLight(receipt)
				if err != nil {
					return err
				}
			}
			n, err = pendingGet(p.b.pending, pendingNodeKey(id), decode)
		}
		if err != nil || n == nil {
			return errors.Join(err, invalid("ef core fact disappeared"))
		}
		nodes = append(nodes, n)
		if rawNode != nil && sameRebuildLanguageMarker(rawNode, n, n.FilePath) {
			known[n.ID] = true
			exact = append(exact, n)
		}
		return nil
	}); err != nil {
		return err
	}
	project := p.rules.Project(nodes)
	proof := project
	if len(exact) != len(nodes) {
		proof = p.rules.Project(exact)
	}
	for _, n := range nodes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if n.Kind == graph.KindType {
			if err := p.planEntity(ctx, n, project, proof, known[n.ID], remaining, view); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *rebuildEFCore) planEntity(ctx context.Context, n *graph.Node,
	project, proof func(*graph.Node, []*graph.Edge) (*graph.Edge, bool), entityKnown bool, remaining int, view byte,
) error {
	var current, exact []*graph.Edge
	var unproved *graph.Edge
	prefix := p.key('e', n.ID)
	if err := p.visit(ctx, prefix, func(r row) error {
		key := r.key[len(prefix):]
		e, err := pendingGet(p.b.pending, key, func(raw []byte) (*graph.Edge, error) {
			if err := takeRebuildEFBudget(&remaining, r.key, raw); err != nil {
				return nil, err
			}
			return decodeEdge(raw)
		})
		if err != nil || e == nil {
			return errors.Join(err, invalid("ef core sibling disappeared"))
		}
		current = append(current, e)
		state, err := p.proof(append(p.key('s'), key...))
		if state == rebuildCapabilityProved {
			exact = append(exact, e)
		} else if unproved == nil {
			unproved = e
		}
		return err
	}); err != nil {
		return err
	}
	desired, changed := project(n, current)
	if changed && unproved != nil {
		p.refused.add(graph.EdgeIdentityFor(unproved), pendingEdgeKey(unproved))
		return nil
	}
	// Removing unproved rows can turn a live no-op (for example, an ambiguous
	// entity name or a stationary attribute edge) into a replacement.
	witness, _ := proof(n, exact)
	if changed && !entityKnown || !p.rules.SameEdge(desired, witness) {
		first := desired
		if first == nil && len(current) != 0 {
			first = current[0]
		}
		if first == nil {
			first = witness
		}
		p.refused.add(graph.EdgeIdentityFor(first), pendingEdgeKey(first))
		return nil
	}
	if !changed {
		return nil
	}
	var raw []byte
	if desired != nil {
		var err error
		raw, err = encodeEdge(desired)
		if err != nil {
			return err
		}
	}
	if err := p.put(p.key(view+('x'-'b'), n.ID), append([]byte{rebuildEFFlag(desired != nil)}, raw...)); err != nil {
		return err
	}
	if desired != nil {
		if err := p.stageTable(n, desired, view); err != nil {
			return err
		}
	}
	if view == 'B' {
		return nil
	}
	p.changed++
	for _, e := range current {
		if err := p.put(append(p.key('d'), pendingEdgeKey(e)...), nil); err != nil {
			return err
		}
	}
	if desired == nil {
		return nil
	}
	return p.put(append(p.key('w'), pendingEdgeKey(desired)...), raw)
}

func rebuildEFFlag(v bool) byte {
	if v {
		return 1
	}
	return 0
}

func (p *rebuildEFCore) stageTable(entity *graph.Node, edge *graph.Edge, view byte) error {
	occupied, err := p.present(pendingNodeKey(edge.To))
	if err != nil || occupied {
		return err // Any existing node wins, including a Go table.
	}
	n := p.rules.Table(entity, edge)
	if n == nil {
		return nil
	}
	key := p.key(view+('t'-'b'), n.ID)
	prior, err := p.get(key)
	if err != nil {
		return err
	}
	if prior != nil {
		owner, _, err := readKeyString(prior)
		if err != nil || owner > entity.ID {
			return err
		}
	}
	// Native visits entities in ID order and keeps the last proposed payload
	// for an absent table, even across different repository/workspace groups.
	raw, err := encodeNode(n)
	if err != nil {
		return err
	}
	return p.put(key, append(appendKeyString(nil, entity.ID), raw...))
}

func (p *rebuildEFCore) apply(ctx context.Context) error {
	prefix := p.key('d')
	if err := p.visit(ctx, prefix, func(r row) error {
		key := r.key[len(prefix):]
		e, err := p.b.pendingEdge(key)
		if err != nil || e == nil {
			return errors.Join(err, invalid("ef core removal row missing"))
		}
		return p.b.deleteEdge(key, e)
	}); err != nil {
		return err
	}
	if err := p.visit(ctx, p.key('t'), func(r row) error {
		_, raw, err := readKeyString(r.value)
		if err != nil {
			return err
		}
		n, err := decodeNode(raw)
		if err != nil {
			return err
		}
		return p.b.writeNode(n)
	}); err != nil {
		return err
	}
	return visitRebuildScratchKeyedEdges(ctx, p.b.pending, p.key('w'), 0, "EF Core model output key", func(e *graph.Edge) error {
		occupied, err := p.present(pendingEdgeKey(e))
		if err != nil || occupied {
			return err // SQLite keeps a non-owned row at the same key.
		}
		return p.b.addEdgeWithCustody(e, rebuildEdgeCustody{proof: rebuildProofEFCoreModel})
	})
}

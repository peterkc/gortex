package store_pebble

import (
	"context"
	"errors"
	"fmt"
	"iter"

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
	SameFact func(*graph.Node, *graph.Node) bool
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
	if receipts == nil || rules.Fact == nil || rules.SameFact == nil || rules.Owned == nil || rules.SameEdge == nil || rules.Project == nil || rules.Table == nil {
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

// b indexes facts by exact repo/workspace. n maps their IDs back to the
// boundary. a selects only boundaries with actions, fallbacks or owned rows.
func (p *rebuildEFCore) captureNodes(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		n, err := checkedRebuildLightNode(r, p.label)
		if err != nil || (n.Kind != graph.KindType && n.Kind != graph.KindFile && n.Kind != graph.KindField) {
			return err
		}
		n, err = decodeNode(r.value)
		if err != nil {
			return err
		}
		keep, active := p.rules.Fact(n)
		if !keep {
			return nil
		}
		group := keyStrings(nil, n.RepoPrefix, n.WorkspaceID)
		if err := p.put(p.key('n', n.ID), group); err != nil {
			return err
		}
		if err := p.put(p.key('b', n.RepoPrefix, n.WorkspaceID, n.ID), nil); err != nil {
			return err
		}
		if active {
			return p.put(append(p.key('a'), group...), nil)
		}
		return nil
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
		group, err := p.get(p.key('n', e.From))
		if err != nil || group == nil {
			return err
		}
		n, err := p.b.pendingNode(e.From)
		if err != nil || n == nil || n.Kind != graph.KindType {
			return err
		}
		if err := p.input(r.key, r.value); err != nil {
			return err
		}
		if err := p.put(append(p.key('a'), group...), nil); err != nil {
			return err
		}
		raw, err := rebuildReceiptKey(snap, r.key)
		if err != nil && !errors.Is(err, ErrRebuildUnknownProvenance) {
			return err
		}
		if raw != nil {
			id, err := decodeLinkRecorder(raw)
			if err != nil {
				return err
			}
			if id.Kind != graph.EdgeModelsTable {
				raw = nil
			} else if err := p.put(append(p.key('r'), raw...), []byte{0}); err != nil {
				return err
			}
		}
		return p.put(append(p.key('e', e.From), r.key...), raw)
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
		for _, raw := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			group, err := p.get(p.key('n', raw.ID))
			if err != nil {
				return err
			}
			if group == nil {
				continue
			}
			active, err := p.present(append(p.key('a'), group...))
			if err != nil {
				return err
			}
			if !active {
				continue
			}
			live, err := p.b.pendingNode(raw.ID)
			if err != nil {
				return err
			}
			key := p.key('p', raw.ID)
			state, err := p.proof(key)
			if err != nil {
				return err
			}
			ok := lang != "" && sameRebuildLanguageMarker(raw, live, file) && p.rules.SameFact(raw, live)
			if err := noteRebuildScratchState(p.put, key, state, rebuildScratchProofBit(ok)); err != nil {
				return err
			}
		}
		for _, raw := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			if raw.Kind != graph.EdgeModelsTable {
				continue
			}
			key := append(p.key('r'), pendingEdgeKey(raw)...)
			wanted, err := p.present(key)
			if err != nil {
				return err
			}
			if wanted {
				state, err := p.proof(key)
				if err != nil {
					return err
				}
				ok := lang != "" && raw.FilePath == file && p.rules.Owned(raw)
				if err := noteRebuildScratchState(p.put, key, state, rebuildScratchProofBit(ok)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (p *rebuildEFCore) plan(ctx context.Context) error {
	prefix := p.key('a')
	if err := p.visit(ctx, prefix, func(r row) error {
		return p.planBoundary(ctx, r.key[len(prefix):])
	}); err != nil {
		return err
	}
	return p.refused.err()
}

func takeRebuildEFBudget(remaining *int, key, val []byte) error {
	// Keys count too, including for an input row with no metadata.
	if len(key)+len(val) > *remaining {
		return fmt.Errorf("%w: EF Core reduction input", ErrRebuildLinkInputSize)
	}
	*remaining -= len(key) + len(val)
	return nil
}

func (p *rebuildEFCore) planBoundary(ctx context.Context, group []byte) error {
	var nodes, proved []*graph.Node
	remaining := rebuildEFCoreBytes
	prefix := append(p.key('b'), group...)
	if err := p.visit(ctx, prefix, func(r row) error {
		id, _, err := readKeyString(r.key[len(prefix):])
		if err != nil {
			return err
		}
		n, err := pendingGet(p.b.pending, pendingNodeKey(id), func(raw []byte) (*graph.Node, error) {
			if err := takeRebuildEFBudget(&remaining, r.key, raw); err != nil {
				return nil, err
			}
			return decodeNode(raw)
		})
		if err != nil || n == nil {
			return errors.Join(err, invalid("ef core fact disappeared"))
		}
		nodes = append(nodes, n)
		state, err := p.proof(p.key('p', id))
		if state == rebuildCapabilityProved {
			proved = append(proved, n)
		}
		return err
	}); err != nil {
		return err
	}
	project, proof := p.rules.Project(nodes), p.rules.Project(proved)
	for _, n := range nodes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if n.Kind == graph.KindType {
			if err := p.planEntity(ctx, n, project, proof, remaining); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *rebuildEFCore) planEntity(ctx context.Context, n *graph.Node,
	project, proof func(*graph.Node, []*graph.Edge) (*graph.Edge, bool), remaining int,
) error {
	var current []*graph.Edge
	var unproved *graph.Edge
	prefix := p.key('e', n.ID)
	if err := p.visit(ctx, prefix, func(r row) error {
		e, err := pendingGet(p.b.pending, r.key[len(prefix):], func(raw []byte) (*graph.Edge, error) {
			if err := takeRebuildEFBudget(&remaining, r.key, raw); err != nil {
				return nil, err
			}
			return decodeEdge(raw)
		})
		if err != nil || e == nil {
			return errors.Join(err, invalid("ef core sibling disappeared"))
		}
		current = append(current, e)
		state, err := p.proof(append(p.key('r'), r.value...))
		if state != rebuildCapabilityProved && unproved == nil {
			unproved = e
		}
		return err
	}); err != nil {
		return err
	}
	desired, changed := project(n, current)
	if !changed {
		return nil
	}
	if unproved != nil {
		p.refused.add(graph.EdgeIdentityFor(unproved), pendingEdgeKey(unproved))
		return nil
	}
	state, err := p.proof(p.key('p', n.ID))
	if err != nil {
		return err
	}
	want, proved := proof(n, current)
	if state != rebuildCapabilityProved || !proved || !p.rules.SameEdge(desired, want) {
		first := desired
		if first == nil {
			first = current[0] // A changed nil projection deletes an owned row.
		}
		p.refused.add(graph.EdgeIdentityFor(first), pendingEdgeKey(first))
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
	if err := p.stageTable(n, desired); err != nil {
		return err
	}
	raw, err := encodeEdge(desired)
	if err != nil {
		return err
	}
	return p.put(append(p.key('w'), pendingEdgeKey(desired)...), raw)
}

func (p *rebuildEFCore) stageTable(entity *graph.Node, edge *graph.Edge) error {
	occupied, err := p.present(pendingNodeKey(edge.To))
	if err != nil || occupied {
		return err // Any existing node wins, including a Go table.
	}
	n := p.rules.Table(entity, edge)
	if n == nil {
		return nil
	}
	key := p.key('t', n.ID)
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

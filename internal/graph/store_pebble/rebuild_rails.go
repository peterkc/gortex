package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

var ErrRebuildRails = errors.New("pebble rebuild Rails stage failed")

var rebuildRailsPrefix = []byte{'g', 'q', 'J'}

// RebuildRailsRules supplies native input roles, path decisions and move payloads.
type RebuildRailsRules struct {
	Call      func(*graph.Edge) (receiver, method string)
	Source    func(string) bool
	Owner     func(*graph.Edge) string
	Candidate func(*graph.Node) bool
	Preferred func(path, receiver string) bool
	Directory func(string) string
	Model     func(string) bool
	Rebind    func(*graph.Edge, string) graph.EdgeReindex
}

// RebuildRailsStats counts eligible residual Calls and planned moves.
type RebuildRailsStats = RebuildExpressStats

type rebuildRails struct {
	rebuildExpress
	rails RebuildRailsRules
}

func rebuildRailsScratch(b *BuildStore) rebuildCallMoves {
	return rebuildCallMoves{b: b, prefix: rebuildRailsPrefix, rowBytes: 16 << 20, label: "rails", inputRow: "edge row"}
}

// FreezeRailsInputs retains pre-loop Call identities, not their payloads.
func (b *BuildStore) FreezeRailsInputs(ctx context.Context, match func(*graph.Edge) bool) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("%w: %w", ErrRebuildRails, err)
		}
	}()
	p := rebuildRailsScratch(b)
	busy, err := rebuildGuardHasPrefix(ctx, b.pending, p.prefix)
	if err != nil {
		return err
	}
	if busy || match == nil {
		return invalid("rails census inputs")
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, b.pending.DeleteRange(p.prefix, prefixEnd(p.prefix), pebble.NoSync))
		}
	}()
	if err := b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(_ *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || kind != graph.EdgeCalls {
			return err
		}
		e, err := p.decodeKeyedEdge(r.key, r.value)
		if err != nil || !match(e) {
			return err
		}
		return p.put(append(p.column('i'), r.key...), nil)
	}); err != nil {
		return err
	}
	return p.put(p.column('I'), nil)
}

// RebuildRails rereads the surviving Calls, MemberOf and ModelsTable at its
// registry turn. It shares the directory/owner scratch reducer with Express.
func (b *BuildStore) RebuildRails(ctx context.Context, receipts iter.Seq2[ParseFacts, error], rules RebuildRailsRules) (stats RebuildRailsStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildRailsStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildRails, err)
		}
	}()
	cols := rebuildRailsScratch(b)
	frozen, err := cols.present(cols.column('I'))
	if err != nil {
		return stats, err
	}
	if !frozen {
		return stats, invalid("rails census missing")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(cols.prefix, prefixEnd(cols.prefix), pebble.NoSync))
	}()
	if receipts == nil || rules.Call == nil || rules.Source == nil || rules.Owner == nil || rules.Candidate == nil ||
		rules.Preferred == nil || rules.Directory == nil || rules.Model == nil || rules.Rebind == nil {
		return stats, invalid("rails stage inputs")
	}
	p := rebuildRails{
		rebuildExpress: rebuildExpress{
			rebuildCallMoves: cols,
			rules: RebuildExpressRules{
				Handler: func(e *graph.Edge) (class, method, name string, ok bool) {
					class, method = rules.Call(e)
					return class, method, "", class != ""
				},
				Owner: func(e *graph.Edge) (string, bool) {
					id := rules.Owner(e)
					return id, id != ""
				},
				Candidate: rules.Candidate,
				Names:     func(name, _ string) []string { return []string{name} },
				Preferred: rules.Preferred,
				Directory: rules.Directory,
			},
		},
		rails: rules,
	}
	if err := p.capture(ctx); err != nil || p.stats.Stubs == 0 {
		return p.stats, err
	}
	if err := p.captureCallClaims(ctx); err != nil {
		return stats, err
	}
	if err := p.captureNodes(ctx); err != nil {
		return stats, err
	}
	if err := p.captureModels(ctx); err != nil {
		return stats, err
	}
	if err := p.record(ctx, p.modelReceipts(ctx, receipts)); err != nil {
		return stats, err
	}
	if err := p.joinMembers(ctx); err != nil {
		return stats, err
	}
	if err := p.plan(ctx); err != nil || p.stats.Moves == 0 {
		return p.stats, err
	}
	if err := p.order(ctx); err != nil {
		return stats, err
	}
	return p.stats, b.withWrites(func() error { return p.apply(ctx, p.stats.Moves) })
}

func (p *rebuildRails) capture(ctx context.Context) error {
	prefix := p.column('i')
	return p.visit(ctx, prefix, func(r row) error {
		old := r.key[len(prefix):]
		e, err := p.edge(old)
		if err != nil || e == nil {
			return err
		}
		q, ok := p.query(e)
		if !ok {
			return nil
		}
		caller, err := p.storeReceiptNode(e.From)
		if err != nil || caller == nil || !p.rails.Source(caller.FilePath) {
			return err
		}
		raw, err := p.inputKey(old)
		if err != nil {
			return err
		}
		if raw != nil {
			if err := p.put(append(p.column('L'), raw...), nil); err != nil {
				return err
			}
		}
		if err := p.put(p.key('q', q.class), nil); err != nil {
			return err
		}
		if err := p.put(p.key('w', q.method), nil); err != nil {
			return err
		}
		if err := p.put(p.key('n', e.From), []byte{rebuildCapabilityWanted}); err != nil {
			return err
		}
		p.stats.Stubs++
		return p.put(append(p.column('h'), old...), raw)
	})
}

// A settled or non-frozen Call can own a frozen Call's raw receipt.
func (p *rebuildRails) captureCallClaims(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || kind != graph.EdgeCalls {
			return err
		}
		raw, err := rebuildReceiptKey(snap, r.key)
		if errors.Is(err, ErrRebuildUnknownProvenance) {
			return nil
		}
		if err != nil {
			return err
		}
		wanted, err := p.present(append(p.column('L'), raw...))
		if err != nil || !wanted {
			return err
		}
		return p.claim(raw, r.key)
	})
}

// ResolveByConvention ranks all eligible declarations before the model check.
// Only candidates for a requested bare receiver need ModelsTable witnesses.
func (p *rebuildRails) captureModels(ctx context.Context) error {
	prefix := p.column('a')
	return p.visit(ctx, prefix, func(r row) error {
		name, rest, err := readKeyString(r.key[len(prefix):])
		if err != nil || !p.rails.Model(name) {
			return err
		}
		id, rest, err := readKeyString(rest)
		if err != nil || len(rest) != 0 {
			return invalid("rails class key")
		}
		return p.visit(ctx, guardOutgoing(id, graph.EdgeModelsTable), func(r row) error {
			if _, err := guardLightEdge(r); err != nil {
				return err
			}
			if err := p.put(p.key('M', id), nil); err != nil {
				return err
			}
			custody, err := readRebuildEdgeCustody(p.b.pending, r.key)
			if errors.Is(err, ErrRebuildUnknownProvenance) {
				return nil
			}
			if err != nil {
				return err
			}
			switch custody.proof {
			case rebuildProofEFCoreModel:
				return p.put(p.key('m', id), nil)
			case 0:
				raw, err := p.inputKey(r.key)
				if err != nil || raw == nil {
					return err
				}
				return p.put(append(p.column('g'), raw...), []byte(id))
			default:
				return nil
			}
		})
	})
}

// ModelsTable raw receipts prove a positive live model witness only. Feed
// them through the same replay that records convention and member facts.
func (p *rebuildRails) modelReceipts(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) iter.Seq2[ParseFacts, error] {
	return func(yield func(ParseFacts, error) bool) {
		for facts, err := range receipts {
			if err == nil && facts.EffectiveLanguage != "" {
				err = p.recordModels(ctx, facts)
			}
			if !yield(facts, err) || err != nil {
				return
			}
		}
	}
}

func (p *rebuildRails) recordModels(ctx context.Context, facts ParseFacts) error {
	file := pathkey.Normalize(facts.File.FilePath)
	for _, e := range facts.Edges {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.Kind != graph.EdgeModelsTable || e.FilePath != file {
			continue
		}
		id, err := p.get(append(p.column('g'), pendingEdgeKey(e)...))
		if err != nil {
			return err
		}
		if id != nil {
			if err := p.put(p.key('m', string(id)), nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *rebuildRails) elect(ctx context.Context, q rebuildExpressQuery, fromDir string, proved bool) (target string, uncertain bool, err error) {
	nameCol, methodCol, modelCol := byte('a'), byte('b'), byte('M')
	if proved {
		nameCol, methodCol, modelCol = 'A', 'B', 'm'
	}
	id, _, _, err := p.convention(ctx, q, fromDir, nameCol)
	if err != nil || id == "" {
		return "", false, err
	}
	if p.rails.Model(q.class) {
		model, err := p.present(p.key(modelCol, id))
		if err != nil || !model {
			return "", false, err
		}
	}
	uncertain, err = p.present(p.key('x', id, q.method))
	if err != nil || uncertain {
		return "", uncertain, err
	}
	target = id
	prefix := p.key(methodCol, id, q.method)
	// Native uses the first matching member in the combined-kind ID order.
	found := false
	err = p.visit(ctx, prefix, func(r row) error {
		if found {
			return nil
		}
		method, rest, err := readKeyString(r.key[len(prefix):])
		if err != nil || method == "" || len(rest) != 0 {
			return invalid("rails method key")
		}
		target, found = method, true
		return nil
	})
	return target, false, err
}

func (p *rebuildRails) plan(ctx context.Context) error {
	var refused rebuildScratchRefusals
	prefix := p.column('h')
	if err := p.visit(ctx, prefix, func(r row) error {
		old := r.key[len(prefix):]
		e, err := p.edge(old)
		if err != nil || e == nil {
			return errors.Join(err, invalid("rails owned Call missing"))
		}
		caller, err := p.storeReceiptNode(e.From)
		if err != nil || caller == nil {
			return errors.Join(err, invalid("rails caller disappeared"))
		}
		q, _ := p.query(e)
		fromDir := p.rails.Directory(caller.FilePath)
		target, uncertain, err := p.elect(ctx, q, fromDir, false)
		if err != nil {
			return err
		}
		if uncertain {
			refused.add(graph.EdgeIdentityFor(e), bytes.Clone(old))
			return nil
		}
		proved, uncertain, err := p.elect(ctx, q, fromDir, true)
		if err != nil {
			return err
		}
		if target == "" && proved == "" && !uncertain {
			return nil
		}
		callerProof, err := rebuildScratchState(p.b.pending, p.key('n', e.From), "rails caller proof")
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
		if target != proved || uncertain || callerProof != rebuildScratchExact ||
			len(claim) <= 1 || claim[0] != rebuildScratchExact || !bytes.Equal(claim[1:], old) {
			refused.add(graph.EdgeIdentityFor(e), bytes.Clone(old))
			return nil
		}
		move := p.rails.Rebind(e, target)
		p.stats.Moves++
		return p.stage(old, move.Edge)
	}); err != nil {
		return err
	}
	return refused.err()
}

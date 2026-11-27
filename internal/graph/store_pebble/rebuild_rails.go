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
func (b *BuildStore) FreezeRailsInputs(ctx context.Context, match func(graph.FrameworkCensusEdge) bool) error {
	callMatch := match
	if match != nil {
		callMatch = func(e graph.FrameworkCensusEdge) bool {
			return e.Kind == graph.EdgeCalls && match(e)
		}
	}
	return b.freezeConventionInputs(ctx, rebuildRailsScratch(b), ErrRebuildRails, callMatch)
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
				Candidate:  rules.Candidate,
				Names:      func(name, _ string) []string { return []string{name} },
				SimpleName: func(id string) string { return id },
				Preferred:  rules.Preferred,
				Directory:  rules.Directory,
			},
		},
		rails: rules,
	}
	if err := p.capture(ctx); err != nil {
		return stats, err
	}
	active, err := rebuildGuardHasPrefix(ctx, b.pending, p.column('h'))
	if err != nil || !active {
		return stats, err
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
	if err := p.record(ctx, p.railsReceipts(ctx, receipts)); err != nil {
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
		e, val, err := p.callRow(old)
		if err != nil || e == nil {
			return err
		}
		q, ok := p.query(e)
		if !ok {
			return nil
		}
		caller, err := p.storeReceiptNode(e.From)
		if err != nil {
			return err
		}
		if caller != nil && p.rails.Source(caller.FilePath) {
			if err := p.input(old, val); err != nil {
				return err
			}
			p.stats.Stubs++
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
		if err := p.put(p.key('c', q.class), nil); err != nil {
			return err
		}
		if err := p.put(p.key('w', q.method), nil); err != nil {
			return err
		}
		if err := p.put(p.key('n', e.From), []byte{rebuildCapabilityWanted}); err != nil {
			return err
		}
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
				return p.put(p.key('Z', id), nil)
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

// Rails adds caller paths and model facts to the shared Express replay.
func (p *rebuildRails) railsReceipts(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) iter.Seq2[ParseFacts, error] {
	return func(yield func(ParseFacts, error) bool) {
		for facts, err := range receipts {
			if err == nil && facts.EffectiveLanguage != "" {
				err = p.recordRailsFacts(ctx, facts)
			}
			if !yield(facts, err) || err != nil {
				return
			}
		}
	}
}

func (p *rebuildRails) recordRailsFacts(ctx context.Context, facts ParseFacts) error {
	file := pathkey.Normalize(facts.File.FilePath)
	for _, n := range facts.Nodes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if n.FilePath != file {
			continue
		}
		wanted, err := p.present(p.key('n', n.ID))
		if err != nil {
			return err
		}
		if wanted {
			if err := p.put(p.key('F', n.ID, file), nil); err != nil {
				return err
			}
		}
	}
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
			if err := p.put(p.key('Z', string(id)), nil); err != nil {
				return err
			}
		}
	}
	return nil
}

type rebuildRailsDecision struct {
	target, class string
}

func (p *rebuildRails) elect(ctx context.Context, q rebuildExpressQuery, fromDir string, proved bool) (rebuildRailsDecision, bool, error) {
	nameCol, methodCol, modelCol, possibleCol := byte('a'), byte('b'), byte('M'), byte('x')
	if proved {
		nameCol, methodCol, modelCol, possibleCol = 'A', 'B', 'Z', 'X'
	}
	id, _, _, err := p.convention(ctx, q, fromDir, nameCol)
	if err != nil || id == "" {
		return rebuildRailsDecision{}, false, err
	}
	if p.rails.Model(q.class) {
		model, err := p.present(p.key(modelCol, id))
		if err != nil || !model {
			return rebuildRailsDecision{}, false, err
		}
	}
	certain, err := p.firstMember(ctx, p.key(methodCol, id, q.method))
	if err != nil {
		return rebuildRailsDecision{}, false, err
	}
	possible, err := p.firstMember(ctx, p.key(possibleCol, id, q.method))
	if err != nil {
		return rebuildRailsDecision{}, false, err
	}
	if possible != "" && (certain == "" || possible < certain) {
		return rebuildRailsDecision{}, true, nil
	}
	if certain == "" {
		certain = id
	}
	return rebuildRailsDecision{target: certain, class: id}, false, nil
}

// Native takes the first matching member in the combined-kind ID order.
func (p *rebuildRails) firstMember(ctx context.Context, prefix []byte) (first string, err error) {
	err = p.visit(ctx, prefix, func(r row) error {
		if first != "" {
			return nil
		}
		id, rest, err := readKeyString(r.key[len(prefix):])
		if err != nil || id == "" || len(rest) != 0 {
			return invalid("rails method key")
		}
		first = id
		return nil
	})
	return first, err
}

func (p *rebuildRails) callRow(key []byte) (*graph.Edge, []byte, error) {
	val, err := p.get(key)
	if err != nil || val == nil {
		return nil, nil, err
	}
	e, err := p.decodeKeyedEdge(key, val)
	return e, val, err
}

func (p *rebuildRails) plan(ctx context.Context) error {
	var refused rebuildScratchRefusals
	prefix := p.column('h')
	if err := p.visit(ctx, prefix, func(r row) error {
		old := r.key[len(prefix):]
		e, val, err := p.callRow(old)
		if err != nil || e == nil {
			return errors.Join(err, invalid("rails owned Call missing"))
		}
		caller, err := p.storeReceiptNode(e.From)
		if err != nil {
			return err
		}
		q, _ := p.query(e)
		var live rebuildRailsDecision
		var uncertain bool
		if caller != nil && p.rails.Source(caller.FilePath) {
			live, uncertain, err = p.elect(ctx, q, p.rails.Directory(caller.FilePath), false)
			if err != nil {
				return err
			}
		}
		var sourceSeen, changed bool
		sources := p.key('F', e.From)
		if err := p.visit(ctx, sources, func(r row) error {
			file, _, err := readKeyString(r.key[len(sources):])
			if err != nil {
				return err
			}
			sourceSeen = true
			var proved rebuildRailsDecision
			var rawUncertain bool
			if p.rails.Source(file) {
				if err := p.input(old, val); err != nil {
					return err
				}
				proved, rawUncertain, err = p.elect(ctx, q, p.rails.Directory(file), true)
				if err != nil {
					return err
				}
			}
			changed = changed || live != proved || rawUncertain
			return nil
		}); err != nil {
			return err
		}
		// A source that leaves both elections empty cannot affect a move.
		if caller != nil && (changed || uncertain || live.target != "") {
			key := pendingNodeKey(caller.ID)
			if _, err := pendingGet(p.b.pending, key, func(val []byte) (bool, error) {
				if len(key)+len(val) > p.rowBytes {
					return false, fmt.Errorf("%w: rails caller node", ErrRebuildLinkInputSize)
				}
				return true, nil
			}); err != nil {
				return err
			}
		}
		if changed || uncertain {
			refused.add(graph.EdgeIdentityFor(e), bytes.Clone(old))
			return nil
		}
		if live.target == "" {
			return nil
		}
		var conflict bool
		for _, id := range [...]string{live.class, live.target} {
			changed, err := p.present(p.key('D', id))
			if err != nil {
				return err
			}
			conflict = conflict || changed
		}
		var claim []byte
		if len(r.value) != 0 {
			claim, err = p.marked(append(p.column('r'), r.value...))
			if err != nil {
				return err
			}
		}
		if !sourceSeen || conflict ||
			len(claim) <= 1 || claim[0] != rebuildScratchExact || !bytes.Equal(claim[1:], old) {
			refused.add(graph.EdgeIdentityFor(e), bytes.Clone(old))
			return nil
		}
		move := p.rails.Rebind(e, live.target)
		p.stats.Moves++
		return p.stage(old, move.Edge)
	}); err != nil {
		return err
	}
	return refused.err()
}

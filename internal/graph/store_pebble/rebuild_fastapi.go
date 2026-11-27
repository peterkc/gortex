package store_pebble

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

var ErrRebuildFastAPI = errors.New("pebble rebuild FastAPI fallback failed")

var ErrRebuildReactResolve = errors.New("pebble rebuild React hooks and context failed")

var (
	rebuildFastAPIPrefix      = []byte{'g', 'q', 'A'}
	rebuildReactResolvePrefix = []byte{'g', 'q', 'H'}
)

// RebuildConventionRules keeps native classification and payload in the resolver.
type RebuildConventionRules struct {
	Shape    func(string, string) (names, dirs []string)
	Source   func(string) bool
	Eligible func(*graph.Node) bool
	Rank     func(file, caller string, dirs []string) int
	Rebind   func(*graph.Edge, string, float64) graph.EdgeReindex
	// AllowWinner rejects the elected candidate, not the candidate itself.
	AllowWinner func(name, file string) bool
}

type rebuildConventionPass struct {
	rebuildCallMoves
	rules RebuildConventionRules
	count uint64
}

// FreezeFastAPIInputs retains exact pre-loop identities, not edge payloads.
func (b *BuildStore) FreezeFastAPIInputs(ctx context.Context, match func(graph.FrameworkCensusEdge) bool) error {
	return b.freezeConventionInputs(ctx, rebuildFastAPIColumns(b), ErrRebuildFastAPI, match)
}

// FreezeReactResolveInputs captures Calls and References. RendersChild stays live.
func (b *BuildStore) FreezeReactResolveInputs(ctx context.Context, match func(graph.FrameworkCensusEdge) bool) error {
	return b.freezeConventionInputs(ctx, rebuildReactResolveColumns(b), ErrRebuildReactResolve, match)
}

func (b *BuildStore) freezeConventionInputs(ctx context.Context, p rebuildCallMoves, stageErr error, match func(graph.FrameworkCensusEdge) bool) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("%w: %w", stageErr, err)
		}
	}()
	busy, err := rebuildGuardHasPrefix(ctx, b.pending, p.prefix)
	if err != nil {
		return err
	}
	if busy || match == nil {
		return invalid(p.label + " census inputs")
	}
	err = b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(_ *pebble.Snapshot, r row) error {
		id, err := decodeLinkRecorder(r.key)
		if err != nil || id.Kind != graph.EdgeCalls && id.Kind != graph.EdgeReferences || id.From == "" || !graph.IsUnresolvedTarget(id.To) {
			return err
		}
		e, err := p.decodeKeyedEdge(r.key, r.value)
		if err != nil {
			return err
		}
		via, _ := e.Meta["via"].(string)
		if !match(graph.FrameworkCensusEdge{EdgeIdentity: id, Via: via}) {
			return nil
		}
		return p.put(append(p.column('i'), r.key...), nil)
	})
	if err == nil {
		err = p.put(p.column('I'), nil)
	}
	if err != nil {
		return errors.Join(err, b.pending.DeleteRange(p.prefix, prefixEnd(p.prefix), pebble.NoSync))
	}
	return nil
}

func rebuildFastAPIColumns(b *BuildStore) rebuildCallMoves {
	return rebuildCallMoves{b: b, prefix: rebuildFastAPIPrefix, rowBytes: 16 << 20, label: "FastAPI", inputRow: "edge row"}
}

func rebuildReactResolveColumns(b *BuildStore) rebuildCallMoves {
	return rebuildCallMoves{b: b, prefix: rebuildReactResolvePrefix, rowBytes: 16 << 20, label: "react hooks and context", inputRow: "edge row"}
}

// RebuildFastAPI resolves residual dependency and router rows. Calls and
// References share native move windows, with each kind in receipt order.
func (b *BuildStore) RebuildFastAPI(ctx context.Context, receipts iter.Seq2[ParseFacts, error], rules RebuildConventionRules) (uint64, error) {
	return b.rebuildConvention(ctx, receipts, rebuildFastAPIColumns(b), rules, ErrRebuildFastAPI, false)
}

// RebuildReactResolve runs after Express and before FastAPI.
func (b *BuildStore) RebuildReactResolve(ctx context.Context, receipts iter.Seq2[ParseFacts, error], rules RebuildConventionRules) (uint64, error) {
	return b.rebuildConvention(ctx, receipts, rebuildReactResolveColumns(b), rules, ErrRebuildReactResolve, true)
}

func (b *BuildStore) rebuildConvention(ctx context.Context, receipts iter.Seq2[ParseFacts, error], cols rebuildCallMoves,
	rules RebuildConventionRules, stageErr error, renders bool,
) (count uint64, err error) {
	defer func() {
		if err != nil {
			count = 0
			err = fmt.Errorf("%w: %w", stageErr, err)
		}
	}()
	if receipts == nil || rules.Shape == nil || rules.Source == nil || rules.Eligible == nil || rules.Rank == nil || rules.Rebind == nil {
		return 0, invalid(cols.label + " fallback inputs")
	}
	frozen, err := cols.present(cols.column('I'))
	if err != nil {
		return 0, err
	}
	if !frozen {
		return 0, invalid(cols.label + " census missing")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(cols.prefix, prefixEnd(cols.prefix), pebble.NoSync))
	}()
	p := rebuildConventionPass{rebuildCallMoves: cols, rules: rules}
	if err := p.capture(ctx, renders); err != nil {
		return 0, err
	}
	active, err := rebuildGuardHasPrefix(ctx, b.pending, p.column('h'))
	if err != nil || !active {
		return 0, err
	}
	kinds := []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences}
	if renders {
		kinds = append(kinds, graph.EdgeRendersChild)
	}
	for _, kind := range kinds {
		if err := p.record(ctx, receipts, kind); err != nil {
			return 0, err
		}
	}
	if err := p.prove(ctx); err != nil || p.count == 0 {
		return 0, err
	}
	return p.count, b.withWrites(func() error {
		return p.apply(ctx, p.count)
	})
}

// Rank uses one bit for caller directory and another for preferred directory.
// A tie at the highest rank is ambiguous; caller directory cannot break a
// tie among preferred candidates unless exactly one of them is local.
type rebuildConventionPick struct {
	id      string
	rank    int
	tied    bool
	chosen  bool
	allowed bool
}

func (p *rebuildConventionPick) add(id string, rank int, allowed bool) {
	if !p.chosen || rank > p.rank {
		*p = rebuildConventionPick{id: id, rank: rank, chosen: true, allowed: allowed}
	} else if rank == p.rank {
		p.tied = true
	}
}

func (p rebuildConventionPick) result() (string, float64) {
	if !p.chosen || p.tied || p.id == "" || !p.allowed {
		return "", 0
	}
	return p.id, [...]float64{0.7, 0.85, 0.9, 0.9}[p.rank]
}

func (p *rebuildConventionPass) pick(ctx context.Context, names, dirs []string, file string, prove bool) (live, proved rebuildConventionPick, err error) {
	// Exact primary Name is indexed once per ID. The suffix-stripped key
	// is distinct, so the native ID dedup adds no extra state here.
	for _, name := range names {
		if err := p.b.RebuildView().VisitNameCandidates(ctx, name, nil, func(n *graph.Node) error {
			if !p.rules.Eligible(n) {
				return nil
			}
			rank := p.rules.Rank(n.FilePath, file, dirs)
			allowed := p.rules.AllowWinner == nil || p.rules.AllowWinner(name, n.FilePath)
			live.add(n.ID, rank, allowed)
			if !prove {
				return nil
			}
			exact, err := p.present(p.key('p', n.ID))
			if exact {
				proved.add(n.ID, rank, allowed)
			}
			return err
		}); err != nil {
			return live, proved, err
		}
	}
	return live, proved, nil
}

func (p *rebuildConventionPass) source(id string) (*graph.Node, error) {
	return pendingGet(p.b.pending, pendingNodeKey(id), decodeNodeLight)
}

// h holds active sites and their raw keys; n and s request only the possible
// name candidates and source paths used by these sites.
func (p *rebuildConventionPass) capture(ctx context.Context, renders bool) error {
	prefix := p.column('i')
	if err := pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		key := r.key[len(prefix):]
		val, err := p.get(key)
		if err != nil || val == nil {
			return err // An earlier turn moved or removed the exact key.
		}
		return p.captureSite(ctx, key, val)
	}); err != nil || !renders {
		return err
	}
	// Native streams current RendersChild rows instead of freezing them.
	return p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(_ *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || kind != graph.EdgeRendersChild {
			return err
		}
		return p.captureSite(ctx, r.key, r.value)
	})
}

func (p *rebuildConventionPass) captureSite(ctx context.Context, old, val []byte) error {
	e, err := p.decodeKeyedEdge(old, val)
	if err != nil {
		return err
	}
	via, _ := e.Meta["via"].(string)
	names, dirs := p.rules.Shape(e.To, via)
	if len(names) == 0 {
		return nil
	}
	source, err := p.source(e.From)
	if err != nil || source == nil || !p.rules.Source(source.FilePath) {
		return err
	}
	live, _, err := p.pick(ctx, names, dirs, source.FilePath, false)
	if err != nil || !live.chosen {
		return err
	}
	if err := p.input(old, val); err != nil {
		return err
	}
	for _, name := range names {
		if err := p.put(p.key('n', name), nil); err != nil {
			return err
		}
	}
	if err := p.put(p.key('s', source.ID), nil); err != nil {
		return err
	}
	var raw []byte
	if target, confidence := live.result(); target != "" {
		raw, err = rebuildReceiptKey(p.b.pending, old)
		if err != nil && !errors.Is(err, ErrRebuildUnknownProvenance) {
			return err
		}
		if raw != nil {
			if err := p.claim(raw, old); err != nil {
				return err
			}
		}
		p.rules.Rebind(e, target, confidence)
		if err := p.stage(old, e); err != nil {
			return err
		}
		p.count++
	}
	return p.put(append(p.column('h'), old...), raw)
}

func (p *rebuildConventionPass) record(ctx context.Context, receipts iter.Seq2[ParseFacts, error], kind graph.EdgeKind) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file := pathkey.Normalize(facts.File.FilePath)
		if facts.EffectiveLanguage == "" {
			continue
		}
		if kind == graph.EdgeCalls {
			for _, n := range facts.Nodes {
				if err := ctx.Err(); err != nil {
					return err
				}
				if n.FilePath == file {
					if err := p.recordNode(n); err != nil {
						return err
					}
				}
			}
		}
		for _, e := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			if e.Kind == kind {
				if err := p.recordEdge(e, file); err != nil {
					return err
				}
			}
		}
	}
	return ctx.Err()
}

func (p *rebuildConventionPass) recordNode(raw *graph.Node) error {
	source, err := p.present(p.key('s', raw.ID))
	if err != nil {
		return err
	}
	named, err := p.present(p.key('n', raw.Name))
	if err != nil || !source && !named {
		return err
	}
	live, err := p.source(raw.ID)
	if err != nil || live == nil || raw.FilePath != live.FilePath {
		return err
	}
	if source {
		if err := p.put(p.key('q', raw.ID), nil); err != nil {
			return err
		}
	}
	if named && raw.Name == live.Name && p.rules.Eligible(raw) && p.rules.Eligible(live) {
		return p.put(p.key('p', raw.ID), nil)
	}
	return nil
}

func (p *rebuildConventionPass) recordEdge(raw *graph.Edge, file string) error {
	key := append(p.column('r'), pendingEdgeKey(raw)...)
	claim, err := p.marked(key)
	if err != nil || len(claim) < 2 {
		return err
	}
	live, err := p.edge(claim[1:])
	if err != nil || live == nil {
		return errors.Join(err, invalid(p.label+" receipt has no live edge"))
	}
	rawVia, _ := raw.Meta["via"].(string)
	liveVia, _ := live.Meta["via"].(string)
	rawNames, _ := p.rules.Shape(raw.To, rawVia)
	liveNames, _ := p.rules.Shape(live.To, liveVia)
	exact := raw.FilePath == file && raw.Kind == live.Kind && rawVia == liveVia && slices.Equal(rawNames, liveNames)
	return p.noteReceipt(key, claim, exact)
}

func (p *rebuildConventionPass) prove(ctx context.Context) error {
	var refused rebuildScratchRefusals
	prefix := p.column('h')
	if err := pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		old := r.key[len(prefix):]
		e, err := p.edge(old)
		if err != nil || e == nil {
			return errors.Join(err, invalid(p.label+" site disappeared"))
		}
		source, err := p.source(e.From)
		if err != nil || source == nil {
			return errors.Join(err, invalid(p.label+" source disappeared"))
		}
		via, _ := e.Meta["via"].(string)
		names, dirs := p.rules.Shape(e.To, via)
		live, proved, err := p.pick(ctx, names, dirs, source.FilePath, true)
		if err != nil {
			return err
		}
		id, confidence := live.result()
		proofID, proofConfidence := proved.result()
		if id != proofID || confidence != proofConfidence {
			refused.add(graph.EdgeIdentityFor(e), nil)
			return nil
		}
		if id == "" {
			return nil
		}
		sourceOK, err := p.present(p.key('q', e.From))
		if err != nil {
			return err
		}
		claim, err := p.marked(append(p.column('r'), r.value...))
		if err == nil && (!sourceOK || len(claim) == 0 || claim[0] != rebuildScratchExact) {
			refused.add(graph.EdgeIdentityFor(e), nil)
		}
		return err
	}); err != nil {
		return err
	}
	return refused.err()
}

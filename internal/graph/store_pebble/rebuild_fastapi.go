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

var ErrRebuildFastAPI = errors.New("pebble rebuild FastAPI fallback failed")

var rebuildFastAPIPrefix = []byte{'g', 'q', 'A'}

// RebuildFastAPIRules keeps the native marker and rebind payload in the resolver.
type RebuildFastAPIRules struct {
	Shape    func(string, string) (string, []string)
	Source   func(string) bool
	Eligible func(*graph.Node) bool
	Rank     func(file, caller string, dirs []string) int
	Rebind   func(*graph.Edge, string, float64) graph.EdgeReindex
}

type rebuildFastAPI struct {
	rebuildCallMoves
	rules RebuildFastAPIRules
	count uint64
}

// FreezeFastAPIInputs retains exact pre-loop identities, not edge payloads.
func (b *BuildStore) FreezeFastAPIInputs(ctx context.Context, match func(graph.FrameworkCensusEdge) bool) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("%w: %w", ErrRebuildFastAPI, err)
		}
	}()
	p := rebuildFastAPIColumns(b)
	busy, err := rebuildGuardHasPrefix(ctx, b.pending, p.prefix)
	if err != nil {
		return err
	}
	if busy || match == nil {
		return invalid("fastapi census inputs")
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

// RebuildFastAPI resolves residual dependency and router rows. Calls and
// References share native move windows, with each kind in receipt order.
func (b *BuildStore) RebuildFastAPI(ctx context.Context, receipts iter.Seq2[ParseFacts, error], rules RebuildFastAPIRules) (count uint64, err error) {
	defer func() {
		if err != nil {
			count = 0
			err = fmt.Errorf("%w: %w", ErrRebuildFastAPI, err)
		}
	}()
	if receipts == nil || rules.Shape == nil || rules.Source == nil || rules.Eligible == nil || rules.Rank == nil || rules.Rebind == nil {
		return 0, invalid("fastapi fallback inputs")
	}
	cols := rebuildFastAPIColumns(b)
	frozen, err := cols.present(cols.column('I'))
	if err != nil {
		return 0, err
	}
	if !frozen {
		return 0, invalid("fastapi census missing")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(cols.prefix, prefixEnd(cols.prefix), pebble.NoSync))
	}()
	p := rebuildFastAPI{rebuildCallMoves: cols, rules: rules}
	if err := p.capture(ctx); err != nil {
		return 0, err
	}
	active, err := rebuildGuardHasPrefix(ctx, b.pending, p.column('h'))
	if err != nil || !active {
		return 0, err
	}
	for _, kind := range [...]graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences} {
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
type rebuildFastAPIPick struct {
	id     string
	rank   int
	tied   bool
	chosen bool
}

func (p *rebuildFastAPIPick) add(id string, rank int) {
	if !p.chosen || rank > p.rank {
		*p = rebuildFastAPIPick{id: id, rank: rank, chosen: true}
	} else if rank == p.rank {
		p.tied = true
	}
}

func (p rebuildFastAPIPick) result() (string, float64) {
	if !p.chosen || p.tied || p.id == "" {
		return "", 0
	}
	return p.id, [...]float64{0.7, 0.85, 0.9, 0.9}[p.rank]
}

func (p *rebuildFastAPI) pick(ctx context.Context, name, file string, dirs []string, prove bool) (live, proved rebuildFastAPIPick, err error) {
	err = p.b.RebuildView().VisitNameCandidates(ctx, name, nil, func(n *graph.Node) error {
		if !p.rules.Eligible(n) {
			return nil
		}
		rank := p.rules.Rank(n.FilePath, file, dirs)
		live.add(n.ID, rank)
		if !prove {
			return nil
		}
		exact, err := p.present(p.key('p', n.ID))
		if exact {
			proved.add(n.ID, rank)
		}
		return err
	})
	return live, proved, err
}

func (p *rebuildFastAPI) source(id string) (*graph.Node, error) {
	return pendingGet(p.b.pending, pendingNodeKey(id), decodeNodeLight)
}

// h holds active sites and their raw keys; n and s request only the possible
// name candidates and source paths used by these sites.
func (p *rebuildFastAPI) capture(ctx context.Context) error {
	prefix := p.column('i')
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		old := r.key[len(prefix):]
		val, err := p.get(old)
		if err != nil || val == nil {
			return err // An earlier turn settled or removed the exact key.
		}
		e, err := p.decodeKeyedEdge(old, val)
		if err != nil {
			return err
		}
		via, _ := e.Meta["via"].(string)
		name, dirs := p.rules.Shape(e.To, via)
		if name == "" || dirs == nil {
			return nil
		}
		source, err := p.source(e.From)
		if err != nil || source == nil || !p.rules.Source(source.FilePath) {
			return err
		}
		live, _, err := p.pick(ctx, name, source.FilePath, dirs, false)
		if err != nil || !live.chosen {
			return err
		}
		if err := p.input(old, val); err != nil {
			return err
		}
		if err := p.put(p.key('n', name), nil); err != nil {
			return err
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
	})
}

func (p *rebuildFastAPI) record(ctx context.Context, receipts iter.Seq2[ParseFacts, error], kind graph.EdgeKind) error {
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

func (p *rebuildFastAPI) recordNode(raw *graph.Node) error {
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

func (p *rebuildFastAPI) recordEdge(raw *graph.Edge, file string) error {
	key := append(p.column('r'), pendingEdgeKey(raw)...)
	claim, err := p.marked(key)
	if err != nil || len(claim) < 2 {
		return err
	}
	live, err := p.edge(claim[1:])
	if err != nil || live == nil {
		return errors.Join(err, invalid("fastapi receipt has no live edge"))
	}
	rawVia, _ := raw.Meta["via"].(string)
	liveVia, _ := live.Meta["via"].(string)
	rawName, _ := p.rules.Shape(raw.To, rawVia)
	liveName, _ := p.rules.Shape(live.To, liveVia)
	return p.noteReceipt(key, claim, raw.FilePath == file && raw.Kind == live.Kind && rawVia == liveVia && rawName == liveName)
}

func (p *rebuildFastAPI) prove(ctx context.Context) error {
	var refused rebuildScratchRefusals
	prefix := p.column('h')
	if err := pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		old := r.key[len(prefix):]
		e, err := p.edge(old)
		if err != nil || e == nil {
			return errors.Join(err, invalid("fastapi site disappeared"))
		}
		source, err := p.source(e.From)
		if err != nil || source == nil {
			return errors.Join(err, invalid("fastapi source disappeared"))
		}
		via, _ := e.Meta["via"].(string)
		name, dirs := p.rules.Shape(e.To, via)
		live, proved, err := p.pick(ctx, name, source.FilePath, dirs, true)
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

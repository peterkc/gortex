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

var ErrRebuildSwiftObjC = errors.New("pebble rebuild Swift/ObjC stage failed")

var rebuildSwiftObjCPrefix = []byte{'g', 'J'}

// RebuildSwiftObjCStats reports installed edges.
type RebuildSwiftObjCStats struct {
	Edges uint64
}

// RebuildSwiftObjC stages selector and protocol join results from the
// resolver in the pending overlay. Preserve existing durable keys for SQLite
// parity and prove every new edge before applying any of them. All language
// declarations remain available.
func (b *BuildStore) RebuildSwiftObjC(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	reduce func(RebuildView, func(*graph.Edge) error) error,
) (stats RebuildSwiftObjCStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildSwiftObjCStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildSwiftObjC, err)
		}
	}()
	if receipts == nil || reduce == nil {
		return stats, fmt.Errorf("%w: invalid Swift/ObjC stage inputs", ErrRebuildLinkInputSize)
	}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildSwiftObjCPrefix)
	if err != nil {
		return stats, err
	}
	if occupied {
		return stats, invalid("Swift/ObjC scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(rebuildSwiftObjCPrefix, prefixEnd(rebuildSwiftObjCPrefix), pebble.NoSync))
	}()
	p := rebuildSwiftObjCPass{b: b}
	if err := reduce(b.RebuildView(), func(e *graph.Edge) error {
		return p.propose(ctx, e)
	}); err != nil {
		return stats, err
	}
	if !p.changed {
		return stats, nil
	}
	if err := p.prove(ctx, receipts); err != nil {
		return stats, err
	}
	err = b.withWrites(func() error { return p.apply(ctx) })
	return p.stats, err
}

type rebuildSwiftObjCPass struct {
	b               *BuildStore
	changed         bool
	hasAlternatives bool
	stats           RebuildSwiftObjCStats
}

func rebuildSwiftObjCKey(col byte, fields ...string) []byte {
	return keyStrings([]byte{'g', 'J', col}, fields...)
}

func (p *rebuildSwiftObjCPass) put(key, value []byte) error {
	return p.b.putRebuildScratch(key, value, 16<<20, "Swift/ObjC")
}

func (p *rebuildSwiftObjCPass) propose(ctx context.Context, e *graph.Edge) error {
	key := pendingEdgeKey(e)
	prior, err := p.b.pendingEdge(key)
	if err != nil || prior != nil {
		return err
	}
	proposal := append(rebuildSwiftObjCKey('e'), key...)
	exists, err := pendingGet(p.b.pending, proposal, func([]byte) (bool, error) { return true, nil })
	if err != nil || exists {
		// Duplicate explicit and heuristic matches have identical payload.
		return err
	}
	raw, err := encodeEdge(e)
	if err != nil {
		return err
	}
	if err := p.put(rebuildSwiftObjCKey('n', e.From), []byte{rebuildCapabilityWanted}); err != nil {
		return err
	}
	if err := p.put(rebuildSwiftObjCKey('n', e.To), []byte{rebuildCapabilityWanted}); err != nil {
		return err
	}
	if e.Kind == graph.EdgeImplements {
		if err := p.wantProtocolAlternatives(ctx, e.To); err != nil {
			return err
		}
	}
	p.changed = true
	return p.put(proposal, raw)
}

func (p *rebuildSwiftObjCPass) wantProtocolAlternatives(ctx context.Context, winnerID string) error {
	// Conformers that chose the same protocol share one election proof.
	group := rebuildSwiftObjCKey('g', winnerID)
	state, err := p.state(group)
	if err != nil || state != 0 {
		return err
	}
	view := p.b.RebuildView()
	winner, err := view.Node(ctx, winnerID)
	if err != nil {
		return err
	}
	if winner == nil {
		return invalid("Swift/ObjC protocol winner missing")
	}
	// The winner already needs endpoint proof. Stream only the other
	// interfaces, including ones that the live objc bit made ineligible.
	want := func(n *graph.Node) error {
		if n.Kind != graph.KindInterface || n.Language != "swift" || n.Name != winner.Name || n.ID == winnerID {
			return nil
		}
		p.hasAlternatives = true
		return p.put(rebuildSwiftObjCKey('p', n.ID), append([]byte{rebuildCapabilityWanted}, winnerID...))
	}
	if winner.Name == "" {
		err = view.VisitNodes(ctx, want)
	} else {
		err = view.VisitNameCandidates(ctx, winner.Name, []string{"swift"}, want)
	}
	if err != nil {
		return err
	}
	// The receipt sweep clears this proof if any alternative is unproved.
	return p.put(group, []byte{rebuildScratchExact})
}

func (p *rebuildSwiftObjCPass) state(key []byte) (byte, error) {
	return rebuildScratchState(p.b.pending, key, "Swift/ObjC proof state")
}

func (p *rebuildSwiftObjCPass) prove(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := p.noteReceipt(ctx, n, file, facts.EffectiveLanguage); err != nil {
				return err
			}
		}
	}
	if err := pendingVisit(ctx, p.b.pending, rebuildSwiftObjCKey('p'), func(r row) error {
		if len(r.value) < 2 {
			return invalid("Swift/ObjC protocol proof state")
		}
		if r.value[0] != rebuildScratchExact {
			return p.put(rebuildSwiftObjCKey('g', string(r.value[1:])), []byte{rebuildCapabilityUnproved})
		}
		return nil
	}); err != nil {
		return err
	}
	var refused rebuildScratchRefusals
	if err := p.visit(ctx, func(e *graph.Edge) error {
		ok, err := p.proved(e)
		if err == nil && !ok {
			refused.add(graph.EdgeIdentityFor(e), nil)
		}
		return err
	}); err != nil {
		return err
	}
	return refused.err()
}

func (p *rebuildSwiftObjCPass) noteReceipt(ctx context.Context, raw *graph.Node, file, language string) error {
	key := rebuildSwiftObjCKey('n', raw.ID)
	state, err := p.state(key)
	if err != nil {
		return err
	}
	var alt, alternative []byte
	if p.hasAlternatives {
		alt = rebuildSwiftObjCKey('p', raw.ID)
		alternative, err = pendingGet(p.b.pending, alt, copyValue)
		if err != nil {
			return err
		}
	}
	if state == 0 && len(alternative) == 0 {
		return nil
	}
	live, err := p.b.RebuildView().Node(ctx, raw.ID)
	if err != nil {
		return err
	}
	inReceipt := language == raw.Language && raw.FilePath == file
	if state != 0 {
		proved := inReceipt && sameRebuildSwiftObjCInput(raw, live)
		if err := noteRebuildScratchState(p.put, key, state, rebuildScratchProofBit(proved)); err != nil {
			return err
		}
	}
	if len(alternative) != 0 {
		proved := inReceipt && live != nil && live.Kind == raw.Kind && live.Language == raw.Language &&
			live.Name == raw.Name && sameRebuildSwiftObjCBit(raw, live)
		return noteRebuildScratchPayload(p.put, alt, alternative, rebuildScratchProofBit(proved))
	}
	return nil
}

func (p *rebuildSwiftObjCPass) proved(e *graph.Edge) (bool, error) {
	for _, id := range [...]string{e.From, e.To} {
		state, err := p.state(rebuildSwiftObjCKey('n', id))
		if err != nil || state != rebuildScratchExact {
			return false, err
		}
	}
	if e.Kind != graph.EdgeImplements {
		return true, nil
	}
	state, err := p.state(rebuildSwiftObjCKey('g', e.To))
	return state == rebuildScratchExact, err
}

func sameRebuildSwiftObjCBit(raw, live *graph.Node) bool {
	rawObjC, _ := raw.Meta["objc"].(bool)
	liveObjC, _ := live.Meta["objc"].(bool)
	return rawObjC == liveObjC
}

func sameRebuildSwiftObjCInput(raw, live *graph.Node) bool {
	if live == nil || live.ID != raw.ID || live.Kind != raw.Kind || live.Language != raw.Language ||
		live.Name != raw.Name || live.FilePath != raw.FilePath || live.StartLine != raw.StartLine || live.RepoPrefix != raw.RepoPrefix {
		return false
	}
	switch raw.Language {
	case "swift":
		for _, key := range [...]string{"objc_selector", "objc_setter_selector"} {
			rawValue, _ := raw.Meta[key].(string)
			liveValue, _ := live.Meta[key].(string)
			if rawValue != liveValue {
				return false
			}
		}
		if raw.Kind == graph.KindInterface {
			return sameRebuildSwiftObjCBit(raw, live)
		}
	case "objc":
		if raw.Kind == graph.KindType {
			rawProtocols, _ := raw.Meta["objc_protocols"].(string)
			liveProtocols, _ := live.Meta["objc_protocols"].(string)
			return rawProtocols == liveProtocols
		}
	}
	return true
}

func (p *rebuildSwiftObjCPass) visit(ctx context.Context, yield func(*graph.Edge) error) error {
	return visitRebuildScratchKeyedEdges(ctx, p.b.pending, rebuildSwiftObjCKey('e'), 0, "Swift/ObjC proposal key", yield)
}

func (p *rebuildSwiftObjCPass) apply(ctx context.Context) error {
	return applyRebuildScratchEdges(ctx, p.visit, p.b, rebuildProofSwiftObjC, &p.stats.Edges)
}

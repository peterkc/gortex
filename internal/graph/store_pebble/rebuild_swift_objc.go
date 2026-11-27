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

const rebuildSwiftObjCRowBytes = 16 << 20

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
	b            *BuildStore
	changed      bool
	hasElections bool
	stats        RebuildSwiftObjCStats
}

func rebuildSwiftObjCKey(col byte, fields ...string) []byte {
	return keyStrings([]byte{'g', 'J', col}, fields...)
}

func (p *rebuildSwiftObjCPass) put(key, value []byte) error {
	return p.b.putRebuildScratch(key, value, rebuildSwiftObjCRowBytes, "Swift/ObjC")
}

func (p *rebuildSwiftObjCPass) node(ctx context.Context, id string, decode func([]byte) (*graph.Node, error)) (*graph.Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := pendingNodeKey(id)
	return pendingGet(p.b.pending, key, func(raw []byte) (*graph.Node, error) {
		if len(key)+len(raw) > rebuildSwiftObjCRowBytes {
			return nil, fmt.Errorf("%w: swift/objc node row", ErrRebuildLinkInputSize)
		}
		n, err := decode(raw)
		if err == nil && n.ID != id {
			return nil, invalid("swift/objc node identity")
		}
		return n, err
	})
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
	// Only endpoints of a new edge need a deciding row. Discovery can see
	// a large declaration that has no match without refusing the turn.
	for _, id := range [...]string{e.From, e.To} {
		if _, err := p.node(ctx, id, decodeNodeLight); err != nil {
			return err
		}
	}
	raw, err := encodeEdge(e)
	if err != nil {
		return err
	}
	if err := p.put(rebuildSwiftObjCKey('s', e.From), []byte{rebuildCapabilityWanted}); err != nil {
		return err
	}
	if err := p.put(rebuildSwiftObjCKey('n', e.To), []byte{rebuildCapabilityWanted}); err != nil {
		return err
	}
	if e.Kind == graph.EdgeImplements {
		if err := p.wantProtocolElection(ctx, e.To); err != nil {
			return err
		}
	}
	p.changed = true
	return p.put(proposal, raw)
}

func (p *rebuildSwiftObjCPass) wantProtocolElection(ctx context.Context, winnerID string) error {
	// Conformers that chose the same protocol share one election proof.
	group := rebuildSwiftObjCKey('g', winnerID)
	state, err := p.state(group)
	if err != nil || state != 0 {
		return err
	}
	winner, err := p.node(ctx, winnerID, decodeNodeLight)
	if err != nil {
		return err
	}
	if winner == nil {
		return invalid("Swift/ObjC protocol winner missing")
	}
	// Index only names with a proposed winner, including the empty native map slot.
	if err := p.put(rebuildSwiftObjCKey('p', winner.Name), []byte(winnerID)); err != nil {
		return err
	}
	p.hasElections = true
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
	if objc, _ := raw.Meta["objc"].(bool); p.hasElections && objc && raw.Kind == graph.KindInterface && raw.Language == "swift" {
		winner, err := pendingGet(p.b.pending, rebuildSwiftObjCKey('p', raw.Name), copyValue)
		if err != nil {
			return err
		}
		// Native's greatest eligible ID wins. Earlier declarations cannot
		// change this proposal, even if they lack a checked receipt.
		if len(winner) != 0 && string(winner) < raw.ID {
			if err := p.put(rebuildSwiftObjCKey('g', string(winner)), []byte{rebuildCapabilityUnproved}); err != nil {
				return err
			}
		}
	}
	source, target := rebuildSwiftObjCKey('s', raw.ID), rebuildSwiftObjCKey('n', raw.ID)
	fromState, err := p.state(source)
	if err != nil {
		return err
	}
	toState, err := p.state(target)
	if err != nil || (fromState == 0 && toState == 0) {
		return err
	}
	live, err := p.node(ctx, raw.ID, decodeNode)
	if err != nil {
		return err
	}
	proved := language == raw.Language && raw.FilePath == file && sameRebuildSwiftObjCInput(raw, live)
	if fromState != 0 {
		// Native uses only the source's line to write the edge.
		if err := noteRebuildScratchState(p.put, source, fromState, rebuildScratchProofBit(proved && raw.StartLine == live.StartLine)); err != nil {
			return err
		}
	}
	if toState != 0 {
		return noteRebuildScratchState(p.put, target, toState, rebuildScratchProofBit(proved))
	}
	return nil
}

func (p *rebuildSwiftObjCPass) proved(e *graph.Edge) (bool, error) {
	for _, key := range [...][]byte{rebuildSwiftObjCKey('s', e.From), rebuildSwiftObjCKey('n', e.To)} {
		state, err := p.state(key)
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
		live.Name != raw.Name || live.FilePath != raw.FilePath || live.RepoPrefix != raw.RepoPrefix {
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

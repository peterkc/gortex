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

var ErrRebuildSwiftObjC = errors.New("pebble rebuild Swift/ObjC stage failed")

var rebuildSwiftObjCPrefix = []byte{'g', 'J'}

// RebuildSwiftObjCStats reports installed edges and physical spill bytes.
type RebuildSwiftObjCStats struct {
	Edges, SpillBytes uint64
}

// RebuildSwiftObjC stages the bounded selector and protocol joins from the
// resolver. Existing SQLite keys are stationary; prove every new edge before
// applying any of them. All language declarations remain available.
func (b *BuildStore) RebuildSwiftObjC(ctx context.Context, receipts iter.Seq2[ParseFacts, error], spillAt int,
	reduce func(RebuildView, func(*graph.Edge) error) error,
) (stats RebuildSwiftObjCStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildSwiftObjCStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildSwiftObjC, err)
		}
	}()
	if spillAt < 1 || spillAt > goldBufferBytes || receipts == nil || reduce == nil {
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
	p := rebuildSwiftObjCPass{b: b, spillAt: spillAt}
	if err := reduce(b.RebuildView(), p.propose); err != nil {
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
	b        *BuildStore
	spillAt  int
	buffered int
	changed  bool
	stats    RebuildSwiftObjCStats
}

func rebuildSwiftObjCKey(col byte, fields ...string) []byte {
	return keyStrings([]byte{'g', 'J', col}, fields...)
}

func (p *rebuildSwiftObjCPass) put(key, value []byte) error {
	if len(key)+len(value) > 16<<20 {
		return fmt.Errorf("%w: Swift/ObjC scratch row", ErrRebuildLinkInputSize)
	}
	if err := p.b.pending.Set(key, value, pebble.NoSync); err != nil {
		return err
	}
	p.buffered += len(key) + len(value)
	if p.buffered >= p.spillAt {
		before := p.b.pending.Metrics().Levels[0].TableBytesFlushed
		if err := p.b.pending.Flush(); err != nil {
			return err
		}
		p.stats.SpillBytes += p.b.pending.Metrics().Levels[0].TableBytesFlushed - before
		p.buffered = 0
	}
	return nil
}

func (p *rebuildSwiftObjCPass) propose(e *graph.Edge) error {
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
	p.changed = true
	return p.put(proposal, raw)
}

func (p *rebuildSwiftObjCPass) state(key []byte) (byte, error) {
	return pendingGet(p.b.pending, key, func(value []byte) (byte, error) {
		if len(value) != 1 {
			return 0, invalid("Swift/ObjC proof state")
		}
		return value[0], nil
	})
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
			key := rebuildSwiftObjCKey('n', n.ID)
			state, err := p.state(key)
			if err != nil {
				return err
			}
			if state == 0 {
				continue
			}
			live, err := p.b.RebuildView().Node(ctx, n.ID)
			if err != nil {
				return err
			}
			bit := byte(rebuildCapabilityUnproved)
			if facts.EffectiveLanguage == n.Language && n.FilePath == file && sameRebuildSwiftObjCInput(n, live) {
				bit = rebuildCapabilityProved
			}
			if state&bit == 0 {
				if err := p.put(key, []byte{state | bit}); err != nil {
					return err
				}
			}
		}
	}
	var refused RebuildUnknownProvenanceError
	if err := p.visit(ctx, func(e *graph.Edge) error {
		for _, id := range [...]string{e.From, e.To} {
			state, err := p.state(rebuildSwiftObjCKey('n', id))
			if err != nil {
				return err
			}
			if state != rebuildCapabilityWanted|rebuildCapabilityProved {
				if refused.Count == 0 {
					refused.First = graph.EdgeIdentityFor(e)
				}
				refused.Count++
				break
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if refused.Count != 0 {
		return &refused
	}
	return nil
}

func sameRebuildSwiftObjCInput(raw, live *graph.Node) bool {
	if live == nil || live.ID != raw.ID || live.Kind != raw.Kind || live.Language != raw.Language ||
		live.Name != raw.Name || live.FilePath != raw.FilePath || live.StartLine != raw.StartLine || live.RepoPrefix != raw.RepoPrefix {
		return false
	}
	for _, key := range [...]string{"objc_selector", "objc_setter_selector", "objc_protocols"} {
		a, _ := raw.Meta[key].(string)
		b, _ := live.Meta[key].(string)
		if a != b {
			return false
		}
	}
	a, _ := raw.Meta["objc"].(bool)
	b, _ := live.Meta["objc"].(bool)
	return a == b
}

func (p *rebuildSwiftObjCPass) visit(ctx context.Context, yield func(*graph.Edge) error) error {
	prefix := rebuildSwiftObjCKey('e')
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		e, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key[len(prefix):], pendingEdgeKey(e)) {
			return invalid("Swift/ObjC proposal key")
		}
		return yield(e)
	})
}

func (p *rebuildSwiftObjCPass) apply(ctx context.Context) error {
	return p.visit(ctx, func(e *graph.Edge) error {
		if err := p.b.putEdgeWithCustody(e, rebuildEdgeCustody{proof: rebuildProofSwiftObjC}); err != nil {
			return err
		}
		p.stats.Edges++
		return nil
	})
}

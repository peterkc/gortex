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

var ErrRebuildReactNativeNativePairing = errors.New("pebble rebuild React Native native pairing stage failed")

var rebuildRNPairPrefix = []byte{'g', 'Q'}

// RebuildReactNativeNativePairingStats reports installed edges and physical spill bytes.
type RebuildReactNativeNativePairingStats struct {
	Edges, SpillBytes uint64
}

// RebuildReactNativeNativePairing streams opposite-platform method pairs.
// For SQLite parity, preserve existing durable keys. Prove both declarations
// before installing any new edge.
func (b *BuildStore) RebuildReactNativeNativePairing(ctx context.Context, receipts iter.Seq2[ParseFacts, error], spillAt int,
	platform func(string) string, makeEdge func(*graph.Node, *graph.Node, string, string) *graph.Edge,
) (stats RebuildReactNativeNativePairingStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildReactNativeNativePairingStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildReactNativeNativePairing, err)
		}
	}()
	if spillAt < 1 || spillAt > goldBufferBytes || receipts == nil || platform == nil || makeEdge == nil {
		return stats, fmt.Errorf("%w: invalid React Native native pairing stage inputs", ErrRebuildLinkInputSize)
	}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildRNPairPrefix)
	if err != nil {
		return stats, err
	}
	if occupied {
		return stats, invalid("React Native native pairing scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(rebuildRNPairPrefix, prefixEnd(rebuildRNPairPrefix), pebble.NoSync))
	}()
	p := rebuildRNPairPass{b: b, spillAt: spillAt}
	if err := p.pair(ctx, platform, makeEdge); err != nil {
		return stats, err
	}
	if !p.changed {
		return p.stats, nil
	}
	if err := p.prove(ctx, receipts); err != nil {
		return stats, err
	}
	err = b.withWrites(func() error { return p.apply(ctx) })
	return p.stats, err
}

type rebuildRNPairPass struct {
	b        *BuildStore
	spillAt  int
	buffered int
	changed  bool
	stats    RebuildReactNativeNativePairingStats
}

func rebuildRNPairKey(col byte, fields ...string) []byte {
	return keyStrings([]byte{'g', 'Q', col}, fields...)
}

func (p *rebuildRNPairPass) put(key, value []byte) error {
	if len(key)+len(value) > 16<<20 {
		return fmt.Errorf("%w: React Native native pairing scratch row", ErrRebuildLinkInputSize)
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

func (p *rebuildRNPairPass) pair(ctx context.Context, platform func(string) string,
	makeEdge func(*graph.Node, *graph.Node, string, string) *graph.Edge,
) error {
	view := p.b.RebuildView()
	var ios, android bool
	// The node key starts with ID, not language. Scan only the node prefix
	// and defer metadata decoding until the row is a native method candidate.
	if err := view.visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		light, err := decodeNodeLight(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key, pendingNodeKey(light.ID)) {
			return invalid("working node ID does not match key")
		}
		if light.Kind != graph.KindMethod || light.ID == "" {
			return nil
		}
		var col byte
		switch platform(light.Language) {
		case "ios":
			col = 'i'
		case "android":
			col = 'a'
		default:
			return nil
		}
		n, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		module, method := rebuildRNNames(n.Meta)
		if module == "" || method == "" {
			return nil
		}
		ios = ios || col == 'i'
		android = android || col == 'a'
		return p.put(rebuildRNPairKey(col, module, method, n.ID), nil)
	}); err != nil || !ios || !android {
		return err
	}
	// Each id belongs to one platform and one module/method tuple. Both
	// directions are emitted, so neither input order nor an ordinal chooses
	// a different payload or winning endpoint.
	return pendingVisit(ctx, p.b.pending, rebuildRNPairKey('i'), func(r row) error {
		module, tail, err := readKeyString(r.key[3:])
		if err != nil {
			return err
		}
		method, tail, err := readKeyString(tail)
		if err != nil {
			return err
		}
		ios, err := rebuildRNPairCandidate(ctx, view, tail)
		if err != nil {
			return err
		}
		prefix := rebuildRNPairKey('a', module, method)
		return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
			android, err := rebuildRNPairCandidate(ctx, view, r.key[len(prefix):])
			if err != nil {
				return err
			}
			if ios.ID == android.ID {
				return nil
			}
			if err := p.propose(makeEdge(ios, android, module, method)); err != nil {
				return err
			}
			return p.propose(makeEdge(android, ios, module, method))
		})
	})
}

func rebuildRNPairCandidate(ctx context.Context, view RebuildView, key []byte) (*graph.Node, error) {
	id, tail, err := readKeyString(key)
	if err != nil {
		return nil, err
	}
	if len(tail) != 0 {
		return nil, invalid("React Native pairing candidate key")
	}
	n, err := view.Node(ctx, id)
	if err == nil && n == nil {
		err = invalid("React Native pairing lost a method")
	}
	return n, err
}

func (p *rebuildRNPairPass) propose(e *graph.Edge) error {
	key := pendingEdgeKey(e)
	prior, err := p.b.pendingEdge(key)
	if err != nil || prior != nil {
		return err
	}
	proposal := append(rebuildRNPairKey('e'), key...)
	exists, err := pendingGet(p.b.pending, proposal, func([]byte) (bool, error) { return true, nil })
	if err != nil || exists {
		// Repeated complete keys have identical payload.
		return err
	}
	raw, err := encodeEdge(e)
	if err != nil {
		return err
	}
	if err := p.put(rebuildRNPairKey('n', e.From), []byte{rebuildCapabilityWanted}); err != nil {
		return err
	}
	if err := p.put(rebuildRNPairKey('n', e.To), []byte{rebuildCapabilityWanted}); err != nil {
		return err
	}
	p.changed = true
	return p.put(proposal, raw)
}

func (p *rebuildRNPairPass) state(key []byte) (byte, error) {
	return pendingGet(p.b.pending, key, func(value []byte) (byte, error) {
		if len(value) != 1 {
			return 0, invalid("React Native native pairing proof state")
		}
		return value[0], nil
	})
}

func (p *rebuildRNPairPass) prove(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			key := rebuildRNPairKey('n', n.ID)
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
			if facts.EffectiveLanguage == n.Language && n.FilePath == file && sameRebuildRNPairInput(n, live) {
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
			state, err := p.state(rebuildRNPairKey('n', id))
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

func sameRebuildRNPairInput(raw, live *graph.Node) bool {
	if live == nil || live.ID != raw.ID || live.Kind != raw.Kind || live.Language != raw.Language ||
		live.Name != raw.Name || live.FilePath != raw.FilePath || live.StartLine != raw.StartLine || live.RepoPrefix != raw.RepoPrefix {
		return false
	}
	rawModule, rawMethod := rebuildRNNames(raw.Meta)
	liveModule, liveMethod := rebuildRNNames(live.Meta)
	return rawModule == liveModule && rawMethod == liveMethod
}

func (p *rebuildRNPairPass) visit(ctx context.Context, yield func(*graph.Edge) error) error {
	prefix := rebuildRNPairKey('e')
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		e, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key[len(prefix):], pendingEdgeKey(e)) {
			return invalid("React Native native pairing proposal key")
		}
		return yield(e)
	})
}

func (p *rebuildRNPairPass) apply(ctx context.Context) error {
	return p.visit(ctx, func(e *graph.Edge) error {
		if err := p.b.putEdgeWithCustody(e, rebuildEdgeCustody{proof: rebuildProofRNPair}); err != nil {
			return err
		}
		p.stats.Edges++
		return nil
	})
}

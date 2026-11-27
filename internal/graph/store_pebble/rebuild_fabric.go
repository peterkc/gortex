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

var ErrRebuildFabric = errors.New("pebble rebuild Fabric stage failed")

var rebuildFabricPrefix = []byte{'g', 'F'}

const rebuildFabricRowBytes = 16 << 20

type rebuildFabricCandidate func(*graph.Node) (component, normalized string, native bool)

// RebuildFabricStats reports installed edges.
type RebuildFabricStats struct {
	Edges uint64
}

// RebuildFabric pairs live specs with every matching native manager after
// the frozen registry admits Fabric. It proves every new edge before apply.
func (b *BuildStore) RebuildFabric(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	candidate rebuildFabricCandidate, makeEdge func(*graph.Node, *graph.Node, string) *graph.Edge,
) (stats RebuildFabricStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildFabricStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildFabric, err)
		}
	}()
	if receipts == nil || candidate == nil || makeEdge == nil {
		return stats, fmt.Errorf("%w: invalid Fabric inputs", ErrRebuildLinkInputSize)
	}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildFabricPrefix)
	if err != nil {
		return stats, err
	}
	if occupied {
		return stats, invalid("Fabric scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(rebuildFabricPrefix, prefixEnd(rebuildFabricPrefix), pebble.NoSync))
	}()
	p := rebuildFabricPass{b: b}
	if err := p.collect(ctx, candidate); err != nil {
		return stats, err
	}
	if err := p.pair(ctx, candidate, makeEdge); err != nil || !p.changes {
		return p.stats, err
	}
	if err := p.prove(ctx, receipts, candidate); err != nil {
		return stats, err
	}
	err = b.withWrites(func() error {
		return applyRebuildScratchEdges(ctx, p.visit, b, rebuildProofFabric, &p.stats.Edges)
	})
	return p.stats, err
}

type rebuildFabricPass struct {
	b       *BuildStore
	changes bool
	stats   RebuildFabricStats
}

// s: spec by ID; m: manager by component and ID; n: endpoint proof by ID;
// w: proposal by complete pending edge key.
func rebuildFabricKey(col byte, fields ...string) []byte {
	return keyStrings(append(bytes.Clone(rebuildFabricPrefix), col), fields...)
}

func (p *rebuildFabricPass) put(key, value []byte) error {
	return p.b.putRebuildScratch(key, value, rebuildFabricRowBytes, "Fabric")
}

func (p *rebuildFabricPass) collect(ctx context.Context, candidate rebuildFabricCandidate) error {
	return p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		light, err := checkedRebuildLightNode(r, "Fabric")
		if err != nil || light.Kind != graph.KindType {
			return err
		}
		if len(r.key)+len(r.value) > rebuildFabricRowBytes {
			return fmt.Errorf("%w: Fabric node row", ErrRebuildLinkInputSize)
		}
		n, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		component, normalized, native := candidate(n)
		if component == "" {
			return nil
		}
		projection := &graph.Node{ID: n.ID, Kind: n.Kind, Language: n.Language, FilePath: n.FilePath, StartLine: n.StartLine}
		key := rebuildFabricKey('m', normalized, n.ID)
		if !native {
			key = rebuildFabricKey('s', n.ID)
			projection.Meta = map[string]any{"fabric_component": component}
		}
		value, err := encodeNode(projection)
		if err != nil {
			return err
		}
		return p.put(key, value)
	})
}

func (p *rebuildFabricPass) pair(ctx context.Context, candidate rebuildFabricCandidate,
	makeEdge func(*graph.Node, *graph.Node, string) *graph.Edge,
) error {
	// Native SQLite visits specs by ID and each matching manager by ID.
	// One live role per ID makes every directional proposal key unique.
	return pendingVisit(ctx, p.b.pending, rebuildFabricKey('s'), func(r row) error {
		spec, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		component, normalized, _ := candidate(spec)
		return pendingVisit(ctx, p.b.pending, rebuildFabricKey('m', normalized), func(r row) error {
			manager, err := decodeNodeLight(r.value)
			if err != nil || manager.ID == spec.ID {
				return err
			}
			if err := p.propose(makeEdge(spec, manager, component)); err != nil {
				return err
			}
			return p.propose(makeEdge(manager, spec, component))
		})
	})
}

func (p *rebuildFabricPass) propose(e *graph.Edge) error {
	if graph.IsProxyID(e.From) || graph.IsProxyID(e.To) || graph.StructuralEdgeTargetInvalid(e.Kind, e.To) {
		return nil
	}
	key := pendingEdgeKey(e)
	exists, err := pendingGet(p.b.pending, key, func([]byte) (bool, error) { return true, nil })
	if err != nil || exists {
		// The direct SQLite path preserves each exact-key incumbent independently.
		return err
	}
	value, err := encodeEdge(e)
	if err != nil {
		return err
	}
	for _, id := range [...]string{e.From, e.To} {
		proof := rebuildFabricKey('n', id)
		state, err := p.state(proof)
		if err != nil {
			return err
		}
		if state == 0 {
			if err := p.put(proof, []byte{rebuildCapabilityWanted}); err != nil {
				return err
			}
		}
	}
	p.changes = true
	return p.put(append(rebuildFabricKey('w'), key...), value)
}

func (p *rebuildFabricPass) state(key []byte) (byte, error) {
	return rebuildScratchState(p.b.pending, key, "Fabric endpoint proof")
}

func (p *rebuildFabricPass) prove(ctx context.Context, receipts iter.Seq2[ParseFacts, error], candidate rebuildFabricCandidate) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, raw := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			key := rebuildFabricKey('n', raw.ID)
			state, err := p.state(key)
			if err != nil {
				return err
			}
			if state == 0 {
				continue
			}
			live, err := p.b.pendingNode(raw.ID)
			if err != nil {
				return err
			}
			proved := facts.EffectiveLanguage != "" && raw.FilePath == file && sameRebuildFabricCandidate(raw, live, candidate)
			if err := noteRebuildScratchState(p.put, key, state, rebuildScratchProofBit(proved)); err != nil {
				return err
			}
		}
	}
	return proveRebuildScratchEndpoints(ctx, p.visit, func(id string) (bool, error) {
		state, err := p.state(rebuildFabricKey('n', id))
		return state == rebuildScratchExact, err
	}, nil)
}

func sameRebuildFabricCandidate(raw, live *graph.Node, candidate rebuildFabricCandidate) bool {
	if live == nil || raw.ID != live.ID || raw.Kind != live.Kind || raw.Language != live.Language ||
		raw.FilePath != live.FilePath || raw.StartLine != live.StartLine || raw.RepoPrefix != live.RepoPrefix {
		return false
	}
	component, _, native := candidate(raw)
	liveComponent, _, liveNative := candidate(live)
	return component != "" && component == liveComponent && native == liveNative
}

func (p *rebuildFabricPass) visit(ctx context.Context, yield func(*graph.Edge) error) error {
	return visitRebuildScratchKeyedEdges(ctx, p.b.pending, rebuildFabricKey('w'), 0, "Fabric proposal key", yield)
}

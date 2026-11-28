package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

// RebuildGoBuiltins moves native builtin targets without storing sentinel nodes.
// Keep the original ordered plan for same-site placeholder reconciliation.
func (b *BuildStore) RebuildGoBuiltins(ctx context.Context, reconcileSources bool, spillBytes int,
	selectBuiltin func(*graph.Edge, func(string) (*graph.Node, error)) (string, error),
) error {
	const maxRowBytes = 16 << 20
	hasGo, err := b.rebuildHasNamedGo(ctx)
	if err != nil || !hasGo {
		return err
	}

	readSource := func(id string) (*graph.Node, error) {
		key := pendingNodeKey(id)
		return pendingGet(b.pending, key, func(raw []byte) (*graph.Node, error) {
			if len(key)+len(raw) > maxRowBytes {
				return nil, fmt.Errorf("%w: Go builtin source node", ErrRebuildLinkInputSize)
			}
			n, err := decodeNodeLight(raw)
			if err == nil && n.ID != id {
				return nil, invalid("Go builtin source node identity does not match key")
			}
			return n, err
		})
	}
	planTarget := func(e *graph.Edge) (string, error) { return selectBuiltin(e, readSource) }
	var afterApply func([]byte) error
	if reconcileSources {
		afterApply = func(plan []byte) error { return b.reconcileBuiltinSources(ctx, spillBytes, plan) }
	}
	_, err = b.rebuildAttribution(ctx, planTarget, nil, afterApply, "")
	return err
}

func (b *BuildStore) rebuildHasNamedGo(ctx context.Context) (bool, error) {
	view := b.RebuildView()
	hasGo := false
	err := view.read(ctx, func(snap *pebble.Snapshot) (err error) {
		it, err := snap.NewIterWithContext(ctx, &pebble.IterOptions{
			LowerBound: []byte{familyNode}, UpperBound: []byte{familyNode + 1},
		})
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, it.Close()) }()
		for ok := it.First(); ok; ok = it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			raw, err := it.ValueAndErr()
			if err != nil {
				return err
			}
			// This presence probe skips metadata. Material source and move
			// rows have their own byte bounds at their read boundaries.
			n, err := decodeNodeLight(raw)
			if err != nil {
				return err
			}
			if !bytes.Equal(it.Key(), pendingNodeKey(n.ID)) {
				return invalid("Go builtin language probe node identity does not match key")
			}
			if n.Language == "go" && n.Name != "" {
				hasGo = true
				break
			}
		}
		return it.Error()
	})
	return hasGo, err
}

func (b *BuildStore) reconcileBuiltinSources(ctx context.Context, spillBytes int, plan []byte) (err error) {
	// Native reads source siblings only after all target writes. Replay the
	// original intents even if a stationary edge consumed the target move.
	p, err := b.CapturePlaceholderSources(ctx, spillBytes)
	if err != nil {
		return err
	}
	p.goBuiltinRepoint = true
	defer func() { err = errors.Join(err, p.Close()) }()
	err = b.withWrites(func() error {
		return pendingVisit(ctx, b.pending, plan, func(r row) error {
			const primaryPrefixBytes = 2
			if len(r.key) <= len(plan)+primaryPrefixBytes {
				return invalid("Go builtin intent key")
			}
			key := r.key[len(plan)+primaryPrefixBytes:]
			var fields [4]string // from, kind, old target, recording file
			for i := range fields {
				var err error
				fields[i], key, err = readKeyString(key)
				if err != nil {
					return err
				}
			}
			if len(key) != 8 {
				return invalid("Go builtin intent line")
			}
			e := graph.Edge{From: fields[0], Kind: graph.EdgeKind(fields[1]), To: string(r.value),
				FilePath: fields[3], Line: int(int64(binary.BigEndian.Uint64(key) ^ (1 << 63)))}
			for _, rp := range graph.PlaceholderSourceRepoints([]graph.EdgeReindex{{Edge: &e, OldTo: fields[2]}}) {
				if err := p.claim(ctx, rp); err != nil {
					return err
				}
			}
			return nil
		})
	})
	if err != nil {
		return placeholderError(err)
	}
	_, err = p.Apply(ctx)
	return err
}

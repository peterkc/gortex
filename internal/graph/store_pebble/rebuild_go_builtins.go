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

// RebuildGoBuiltins plans against the pass-entry graph. The resolver selects
// the native sentinel from one edge and bounded source-node reads. The first
// successful complete old key for each sentinel supplies its payload.
func (b *BuildStore) RebuildGoBuiltins(ctx context.Context, reconcileSources bool, spillBytes int,
	selectBuiltin func(*graph.Edge, func(string) (*graph.Node, error)) (*graph.Node, error),
	sameBuiltin func(have, want *graph.Node) bool,
) (err error) {
	view := b.RebuildView()
	const maxRowBytes = 16 << 20
	hasGo := false
	err = view.read(ctx, func(snap *pebble.Snapshot) (err error) {
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
			if len(it.Key())+len(raw) > maxRowBytes {
				return fmt.Errorf("%w: Go builtin language probe", ErrRebuildLinkInputSize)
			}
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
	if err != nil || !hasGo {
		return err
	}

	prefix := []byte{'g', 'u'}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return err
	}
	if occupied {
		return invalid("Go builtin scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
	}()

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
	planTarget := func(e *graph.Edge) (string, error) {
		n, err := selectBuiltin(e, readSource)
		if err != nil || n == nil {
			return "", err
		}
		k := keyStrings(bytes.Clone(prefix), n.ID)
		seen, err := pendingGet(b.pending, k, fnPointerPresent)
		if err != nil || seen {
			return n.ID, err
		}
		raw, err := encodeNode(n)
		if err != nil {
			return "", err
		}
		if len(k)+len(raw) > maxRowBytes {
			return "", fmt.Errorf("%w: Go builtin sentinel", ErrRebuildLinkInputSize)
		}
		return n.ID, b.pending.Set(k, raw, pebble.NoSync)
	}
	materialize := func() error {
		return b.withWrites(func() error {
			return pendingVisit(ctx, b.pending, prefix, func(r row) error {
				n, err := decodeNode(r.value)
				if err != nil {
					return err
				}
				if !bytes.Equal(r.key, keyStrings(bytes.Clone(prefix), n.ID)) {
					return invalid("Go builtin sentinel identity does not match key")
				}
				have, err := pendingGet(b.pending, pendingNodeKey(n.ID), func(raw []byte) (*graph.Node, error) {
					if len(raw) > maxRowBytes {
						return nil, fmt.Errorf("%w: saved Go builtin node", ErrRebuildLinkInputSize)
					}
					have, err := decodeNode(raw)
					if err == nil && have.ID != n.ID {
						return nil, invalid("saved Go builtin identity does not match key")
					}
					return have, err
				})
				if err != nil || (have != nil && sameBuiltin(have, n)) {
					return err
				}
				return b.writeNode(n)
			})
		})
	}
	var afterApply func([]byte) error
	if reconcileSources {
		afterApply = func(plan []byte) error { return b.reconcileBuiltinSources(ctx, spillBytes, plan) }
	}
	_, err = b.rebuildAttribution(ctx, planTarget, materialize, afterApply)
	return err
}

func (b *BuildStore) reconcileBuiltinSources(ctx context.Context, spillBytes int, plan []byte) (err error) {
	// Native reads source siblings only after all target writes. Replay the
	// original intents even if a stationary edge consumed the target move.
	p, err := b.CapturePlaceholderSources(ctx, spillBytes)
	if err != nil {
		return err
	}
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

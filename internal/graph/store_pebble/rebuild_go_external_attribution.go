package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

// RebuildGoExternalCalls materializes the required Go external nodes and
// membership links without rewriting the saved target edges. Choices are
// frozen before applying modules, then symbols, then membership.
func (b *BuildStore) RebuildGoExternalCalls(ctx context.Context,
	desired func(*graph.Edge) (module, symbol *graph.Node, err error), roleRank func(any) int,
) (err error) {
	hasGo, err := b.rebuildHasNamedGo(ctx)
	if err != nil || !hasGo {
		return err
	}
	prefix := []byte{'g', 'x'}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return err
	}
	if occupied {
		return invalid("Go external attribution scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
	}()
	modules, symbols, members := []byte{'g', 'x', 'm'}, []byte{'g', 'x', 's'}, []byte{'g', 'x', 'e'}
	const maxRowBytes = 16 << 20
	readNode := func(key []byte) (*graph.Node, error) {
		return pendingGet(b.pending, key, func(raw []byte) (*graph.Node, error) {
			if len(key)+len(raw) > maxRowBytes {
				return nil, fmt.Errorf("%w: Go external attribution node", ErrRebuildLinkInputSize)
			}
			return decodeNode(raw)
		})
	}
	stage := func(key []byte, n *graph.Node) error {
		raw, err := encodeNode(n)
		if err != nil {
			return err
		}
		if len(key)+len(raw) > maxRowBytes {
			return fmt.Errorf("%w: Go external attribution candidate", ErrRebuildLinkInputSize)
		}
		return b.pending.Set(key, raw, pebble.NoSync)
	}
	if err := b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(_ *pebble.Snapshot, r row) error {
		if len(r.key)+len(r.value) > maxRowBytes {
			return fmt.Errorf("%w: Go external attribution edge", ErrRebuildLinkInputSize)
		}
		e, err := guardLightEdge(r)
		if err != nil {
			return err
		}
		module, symbol, err := desired(e)
		if err != nil || module == nil {
			return err
		}
		mk := keyStrings(bytes.Clone(modules), module.ID)
		prior, err := readNode(mk)
		if err != nil {
			return err
		}
		if prior == nil || roleRank(module.Meta["role"]) > roleRank(prior.Meta["role"]) {
			if err := stage(mk, module); err != nil {
				return err
			}
		}
		if symbol == nil {
			return nil
		}
		// The module write must not make an absent symbol look pre-existing.
		// Test the saved node domain before any graph writes in this pass.
		exists, err := pendingGet(b.pending, pendingNodeKey(symbol.ID), fnPointerPresent)
		if err != nil {
			return err
		}
		if !exists {
			if err := stage(keyStrings(bytes.Clone(symbols), symbol.ID), symbol); err != nil {
				return err
			}
		}
		key := keyStrings(bytes.Clone(members), symbol.ID, module.ID)
		if len(key) > maxRowBytes {
			return fmt.Errorf("%w: Go external attribution membership", ErrRebuildLinkInputSize)
		}
		return b.pending.Set(key, nil, pebble.NoSync)
	}); err != nil {
		return err
	}
	return b.withWrites(func() error {
		if err := pendingVisit(ctx, b.pending, modules, func(r row) error {
			n, err := decodeNode(r.value)
			if err != nil {
				return err
			}
			prior, err := readNode(pendingNodeKey(n.ID))
			if err != nil {
				return err
			}
			if prior == nil || roleRank(n.Meta["role"]) > roleRank(prior.Meta["role"]) {
				return b.writeNode(n)
			}
			return nil
		}); err != nil {
			return err
		}
		if err := pendingVisit(ctx, b.pending, symbols, func(r row) error {
			n, err := decodeNode(r.value)
			if err != nil {
				return err
			}
			return b.writeNode(n)
		}); err != nil {
			return err
		}
		return pendingVisit(ctx, b.pending, members, func(r row) error {
			from, rest, err := readKeyString(r.key[len(members):])
			if err != nil {
				return err
			}
			to, tail, err := readKeyString(rest)
			if err != nil {
				return err
			}
			if len(tail) != 0 {
				return invalid("Go external attribution membership key")
			}
			key := keyStrings([]byte{familyEdge, edgeOut}, from, string(graph.EdgeMemberOf), to)
			exists, err := rebuildGuardHasPrefix(ctx, b.pending, key)
			if err != nil || exists {
				return err
			}
			e := &graph.Edge{From: from, To: to, Kind: graph.EdgeMemberOf, Origin: graph.OriginASTResolved}
			return b.addEdgeWithCustody(e, rebuildEdgeCustody{proof: rebuildProofUntrackedMove})
		})
	})
}

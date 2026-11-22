package store_pebble

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

// ErrRebuildAttributionChain marks a pass in which a planned target rewrite
// lands on another complete old key that the same pass will move.
var ErrRebuildAttributionChain = errors.New("rebuild attribution target lands on a moving key")

// RebuildAttributionChainError counts all such moves in the refused pass and
// identifies the first pair in complete old-key order.
type RebuildAttributionChainError struct {
	Count            uint64
	Old, Destination graph.EdgeIdentity
}

func (e *RebuildAttributionChainError) Unwrap() error { return ErrRebuildAttributionChain }

func (e *RebuildAttributionChainError) Error() string {
	return fmt.Sprintf("%s: count=%d old=%s destination=%s", ErrRebuildAttributionChain, e.Count,
		attributionIdentityText(e.Old), attributionIdentityText(e.Destination))
}

func attributionIdentityText(id graph.EdgeIdentity) string {
	if len(id.From)+len(id.To)+len(id.Kind)+len(id.FilePath) <= 768 {
		return fmt.Sprintf("(%q,%q,%q,%q,%d)", id.From, id.Kind, id.To, id.FilePath, id.Line)
	}
	e := graph.Edge{From: id.From, Kind: id.Kind, To: id.To, FilePath: id.FilePath, Line: id.Line}
	return fmt.Sprintf("(%q,%q,%q,%q,%d) sha256=%x", id.From[:min(64, len(id.From))], id.Kind[:min(64, len(id.Kind))],
		id.To[:min(64, len(id.To))], id.FilePath[:min(64, len(id.FilePath))], id.Line, sha256.Sum256(pendingEdgeKey(&e)))
}

// rebuildAttributionMoves plans from the stable view at pass entry. target
// reads only immutable facts and receives no metadata. Its empty result is no move.
// The private plan lets a chain proof join exact keys across page boundaries
// before any graph writes, without retaining the move set in memory.
func (b *BuildStore) rebuildAttributionMoves(ctx context.Context, target func(*graph.Edge) (string, error)) (int, error) {
	return b.rebuildAttribution(ctx, target, nil, nil)
}

func (b *BuildStore) rebuildAttribution(ctx context.Context, target func(*graph.Edge) (string, error),
	beforeApply func() error, afterApply func([]byte) error,
) (moved int, err error) {
	const maxPageBytes = 16 << 20
	view := b.RebuildView()
	if err := view.mutationError(); err != nil {
		return 0, err
	}
	prefix := []byte{'g', 'a'}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return 0, err
	}
	if occupied {
		return 0, invalid("attribution move scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
		if err != nil {
			moved = 0
			err = fmt.Errorf("%w: attribution: %w", ErrRebuildView, err)
		}
	}()
	planKey := func(oldKey []byte) []byte { return append(bytes.Clone(prefix), oldKey...) }
	if err := view.visit(ctx, []byte{familyEdge, edgeOut}, func(_ *pebble.Snapshot, r row) error {
		if len(r.key)+len(r.value) > maxPageBytes {
			return fmt.Errorf("%w: attribution edge", ErrRebuildLinkInputSize)
		}
		e, err := decodeEdgeLight(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key, pendingEdgeKey(e)) {
			return invalid("attribution edge identity does not match key")
		}
		if !graph.IsUnresolvedTarget(e.To) {
			return nil
		}
		to, err := target(e)
		if err != nil || to == "" || to == e.To {
			return err
		}
		if len(r.key)+len(r.value)+len(to) > maxPageBytes {
			return fmt.Errorf("%w: attribution move", ErrRebuildLinkInputSize)
		}
		return b.pending.Set(planKey(r.key), []byte(to), pebble.NoSync)
	}); err != nil {
		return 0, err
	}
	var chain RebuildAttributionChainError
	if err := pendingVisit(ctx, b.pending, prefix, func(r row) error {
		to := string(r.value)
		if !graph.IsUnresolvedTarget(to) {
			return nil // both attribution reducers only move unresolved old targets
		}
		oldKey := r.key[len(prefix):]
		e, err := pendingGet(b.pending, oldKey, decodeEdgeLight)
		if err != nil {
			return err
		}
		if e == nil || !bytes.Equal(oldKey, pendingEdgeKey(e)) {
			return invalid("attribution plan has no matching old edge")
		}
		old := graph.EdgeIdentityFor(e)
		e.To = to
		moving, err := pendingGet(b.pending, planKey(pendingEdgeKey(e)), func([]byte) (bool, error) { return true, nil })
		if err != nil || !moving {
			return err
		}
		if chain.Count == 0 {
			chain.Old, chain.Destination = old, graph.EdgeIdentityFor(e)
		}
		chain.Count++
		return nil
	}); err != nil {
		return 0, err
	}
	if chain.Count > 0 {
		return 0, &chain
	}
	if beforeApply != nil {
		if err := beforeApply(); err != nil {
			return 0, err
		}
	}

	// Planned keys are already ordered by the entire old identity. After the
	// chain check, a prior page's destination is stationary for later pages.
	var page []graph.EdgeReindex
	pageBytes := 0
	apply := func() error {
		if len(page) == 0 {
			return nil
		}
		if err := b.ApplyLinkReindexes(ctx, page); err != nil {
			return err
		}
		moved += len(page)
		clear(page)
		page, pageBytes = page[:0], 0
		return nil
	}
	if err := pendingVisit(ctx, b.pending, prefix, func(r row) error {
		oldKey := r.key[len(prefix):]
		to := string(r.value)
		e, err := pendingGet(b.pending, oldKey, func(raw []byte) (*graph.Edge, error) {
			cost := len(oldKey) + len(raw) + len(to)
			if cost > maxPageBytes {
				return nil, fmt.Errorf("%w: attribution payload", ErrRebuildLinkInputSize)
			}
			if len(page) == rebuildLinkPendingRows || cost > maxPageBytes-pageBytes {
				if err := apply(); err != nil {
					return nil, err
				}
			}
			pageBytes += cost
			return decodeEdge(raw)
		})
		if err != nil {
			return err
		}
		if e == nil || !bytes.Equal(oldKey, pendingEdgeKey(e)) {
			return invalid("attribution plan has no matching old edge")
		}
		old := e.To
		e.To = to
		page = append(page, graph.EdgeReindex{Edge: e, OldTo: old})
		return nil
	}); err != nil {
		return 0, err
	}
	if err := apply(); err != nil {
		return 0, err
	}
	if moved != 0 && afterApply != nil {
		err = afterApply(prefix)
	}
	return moved, err
}

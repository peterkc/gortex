package store_pebble

import (
	"context"
	"errors"
	"fmt"
	"iter"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

var ErrRebuildUIKit = errors.New("pebble rebuild UIKit fallback failed")

var rebuildUIKitPrefix = []byte{'g', 'q', 'F'}

func rebuildUIKitColumns(b *BuildStore) rebuildCallMoves {
	return rebuildCallMoves{b: b, prefix: rebuildUIKitPrefix, rowBytes: 16 << 20, label: "uikit", inputRow: "edge row"}
}

// FreezeUIKitInputs captures only the native shared Calls and References.
func (b *BuildStore) FreezeUIKitInputs(ctx context.Context, match func(graph.FrameworkCensusEdge) bool) error {
	return b.freezeConventionInputs(ctx, rebuildUIKitColumns(b), ErrRebuildUIKit, match)
}

// RebuildUIKit refetches the frozen sites after SwiftUI and scans the live
// Instantiates and TypedAs sites. Moves follow native kind and receipt order.
func (b *BuildStore) RebuildUIKit(ctx context.Context, receipts iter.Seq2[ParseFacts, error], rules RebuildConventionRules) (count uint64, err error) {
	defer func() {
		if err != nil {
			count = 0
			err = fmt.Errorf("%w: %w", ErrRebuildUIKit, err)
		}
	}()
	if receipts == nil || rules.Shape == nil || rules.Source == nil || rules.Eligible == nil || rules.Rank == nil || rules.Rebind == nil {
		return 0, invalid("uikit inputs")
	}
	cols := rebuildUIKitColumns(b)
	frozen, err := cols.present(cols.column('I'))
	if err != nil {
		return 0, err
	}
	if !frozen {
		return 0, invalid("uikit census missing")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(cols.prefix, prefixEnd(cols.prefix), pebble.NoSync))
	}()
	p := rebuildConventionPass{rebuildCallMoves: cols, rules: rules}
	if err := p.capture(ctx, false); err != nil {
		return 0, err
	}
	if err := b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(_ *pebble.Snapshot, r row) error {
		id, err := decodeLinkRecorder(r.key)
		if err != nil || id.Kind != graph.EdgeInstantiates && id.Kind != graph.EdgeTypedAs || !graph.IsUnresolvedTarget(id.To) {
			return err
		}
		return p.captureSite(ctx, r.key, r.value)
	}); err != nil {
		return 0, err
	}
	active, err := rebuildGuardHasPrefix(ctx, b.pending, p.column('h'))
	if err != nil || !active {
		return 0, err
	}
	kinds := [...]graph.EdgeKind{graph.EdgeInstantiates, graph.EdgeReferences, graph.EdgeTypedAs, graph.EdgeCalls}
	if err := p.captureClaims(ctx, kinds[:]); err != nil {
		return 0, err
	}
	for _, kind := range kinds {
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

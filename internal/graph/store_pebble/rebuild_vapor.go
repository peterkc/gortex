package store_pebble

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

var ErrRebuildVapor = errors.New("pebble rebuild Vapor fallback failed")

var rebuildVaporPrefix = []byte{'g', 'q', 'Z'}

// RebuildVapor reads the four live edge kinds in native order. The
// convention reducer proves the winner and applies frozen receipt windows.
func (b *BuildStore) RebuildVapor(ctx context.Context, receipts iter.Seq2[ParseFacts, error], rules RebuildConventionRules) (count uint64, err error) {
	defer func() {
		if err != nil {
			count = 0
			err = fmt.Errorf("%w: %w", ErrRebuildVapor, err)
		}
	}()
	if receipts == nil || rules.Shape == nil || rules.Source == nil || rules.Eligible == nil || rules.Rank == nil || rules.Rebind == nil || rules.AllowWinner == nil {
		return 0, invalid("vapor inputs")
	}
	prefix := rebuildVaporPrefix
	busy, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return 0, err
	}
	if busy {
		return 0, invalid("vapor scratch occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
	}()
	p := rebuildConventionPass{
		rebuildCallMoves: rebuildCallMoves{b: b, prefix: prefix, rowBytes: 16 << 20, label: "vapor", inputRow: "edge row"},
		rules:            rules,
	}
	kinds := [...]graph.EdgeKind{graph.EdgeInstantiates, graph.EdgeReferences, graph.EdgeTypedAs, graph.EdgeCalls}
	if err := b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(_ *pebble.Snapshot, r row) error {
		id, err := decodeLinkRecorder(r.key)
		if err != nil || !slices.Contains(kinds[:], id.Kind) || !graph.IsUnresolvedTarget(id.To) {
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

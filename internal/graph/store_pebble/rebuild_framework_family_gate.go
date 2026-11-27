package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

var ErrRebuildFrameworkFamilyGate = errors.New("pebble rebuild framework family gate failed")

var rebuildFamilyDropPrefix = []byte{'g', 'q', 50}

const rebuildFamilyRowBytes = 16 << 20

// RebuildFrameworkFamilyGate plans exact drops from the settled private graph.
// This stage consumes prior stage output, not the parse receipt at the same key.
func (b *BuildStore) RebuildFrameworkFamilyGate(ctx context.Context, ordinarySynth func(*graph.Edge) bool,
	crosses func(fromLang, toLang string) bool,
) (removed uint64, err error) {
	defer func() {
		if err != nil {
			removed = 0
			err = fmt.Errorf("%w: %w", ErrRebuildFrameworkFamilyGate, err)
		}
	}()
	prefix := rebuildFamilyDropPrefix
	busy, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return 0, err
	}
	if busy {
		return 0, invalid("framework family gate scratch occupied")
	}
	defer func() { err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync)) }()
	if err := b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		id, err := decodeLinkRecorder(r.key)
		if err != nil || id.Kind != graph.EdgeReferences && id.Kind != graph.EdgeImports {
			return err
		}
		from, fromLarge, err := rebuildFamilyNodeLanguage(snap, id.From)
		if err != nil || from == "" {
			return err
		}
		to, toLarge, err := rebuildFamilyNodeLanguage(snap, id.To)
		if err != nil || !crosses(from, to) {
			return err
		}
		if len(r.key)+len(r.value) > rebuildFamilyRowBytes {
			return fmt.Errorf("%w: framework family edge", ErrRebuildLinkInputSize)
		}
		e, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key, pendingEdgeKey(e)) {
			return invalid("framework family edge identity")
		}
		if !ordinarySynth(e) {
			return nil
		}
		if fromLarge || toLarge {
			return fmt.Errorf("%w: framework family endpoint node", ErrRebuildLinkInputSize)
		}
		return b.putRebuildScratch(append(bytes.Clone(prefix), r.key...), r.value, rebuildFamilyRowBytes, "framework family drop")
	}); err != nil {
		return 0, err
	}
	err = b.withWrites(func() error {
		// Prove every old payload before deleting any row. One full identity
		// is one proposal, and the write lock excludes changed incumbents.
		if err := pendingVisit(ctx, b.pending, prefix, func(r row) error {
			return b.proveRebuildEdgePayload(r.key[len(prefix):], r.value)
		}); err != nil {
			return err
		}
		return visitRebuildScratchKeyedEdges(ctx, b.pending, prefix, 0, "framework family drop identity", func(e *graph.Edge) error {
			if err := b.deleteEdge(pendingEdgeKey(e), e); err != nil {
				return err
			}
			removed++
			return nil
		})
	})
	return removed, err
}

func rebuildFamilyNodeLanguage(snap *pebble.Snapshot, id string) (string, bool, error) {
	key := pendingNodeKey(id)
	var oversized bool
	language, err := pendingGet(snap, key, func(raw []byte) (string, error) {
		n, err := decodeNodeLight(raw)
		if err != nil {
			return "", err
		}
		if n.ID != id {
			return "", invalid("framework family endpoint identity")
		}
		oversized = len(key)+len(raw) > rebuildFamilyRowBytes
		return n.Language, nil
	})
	return language, oversized, err
}

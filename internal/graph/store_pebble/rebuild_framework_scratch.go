package store_pebble

import (
	"bytes"
	"context"
	"fmt"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

// putRebuildScratch keeps mutable proof and election rows in the pending
// overlay, where later point and prefix reads can see them immediately.
func (b *BuildStore) putRebuildScratch(key, value []byte, limit int, label string) error {
	if len(key)+len(value) > limit {
		return fmt.Errorf("%w: %s scratch row", ErrRebuildLinkInputSize, label)
	}
	return b.pending.Set(key, value, pebble.NoSync)
}

const rebuildScratchExact = rebuildCapabilityWanted | rebuildCapabilityProved

// The admission witness uses the node language, not the extractor ID or role.
func sameRebuildLanguageMarker(raw, live *graph.Node, file string) bool {
	return live != nil && live.ID == raw.ID && raw.FilePath == file && live.FilePath == file && live.Language == raw.Language
}

// rebuildReceiptLanguageMarker proves a live census label from its file receipt.
func (b *BuildStore) rebuildReceiptLanguageMarker(raw *graph.Node, file string, limit int) (bool, error) {
	key := pendingNodeKey(raw.ID)
	return pendingGet(b.pending, key, func(value []byte) (bool, error) {
		live, err := decodeNodeLight(value)
		if err != nil || !sameRebuildLanguageMarker(raw, live, file) {
			return false, err
		}
		if limit != 0 && len(key)+len(value) > limit {
			return false, ErrRebuildLinkInputSize
		}
		return true, nil
	})
}

// One exact, bounded language marker proves admission. An oversized spare
// marker cannot defeat an in-bound witness.
func (b *BuildStore) proveRebuildLanguageMarker(ctx context.Context, prefix []byte, limit int, label string) (bool, error) {
	var proved, oversized bool
	err := pendingVisit(ctx, b.pending, prefix, func(r row) error {
		if len(r.value) != 1 {
			return invalid(label + " marker proof state")
		}
		if proved || r.value[0] != rebuildScratchExact {
			return nil
		}
		id, rest, err := readKeyString(r.key[len(prefix):])
		if err != nil || id == "" || len(rest) != 0 {
			return invalid(label + " marker proof key")
		}
		key := pendingNodeKey(id)
		size, err := pendingGet(b.pending, key, func(value []byte) (int, error) {
			return len(key) + len(value), nil
		})
		if err != nil {
			return err
		}
		if size == 0 {
			return invalid(label + " exact marker missing")
		}
		proved = size <= limit
		oversized = oversized || !proved
		return nil
	})
	if err == nil && !proved && oversized {
		return false, fmt.Errorf("%w: %s marker node", ErrRebuildLinkInputSize, label)
	}
	return proved, err
}

func rebuildScratchState(db pebble.Reader, key []byte, malformed string) (byte, error) {
	return pendingGet(db, key, func(raw []byte) (byte, error) {
		if len(raw) != 1 {
			return 0, invalid(malformed)
		}
		return raw[0], nil
	})
}

func rebuildScratchProofBit(proved bool) byte {
	if proved {
		return rebuildCapabilityProved
	}
	return rebuildCapabilityUnproved
}

func noteRebuildScratchState(put func([]byte, []byte) error, key []byte, state, bit byte) error {
	if state&bit != 0 {
		return nil
	}
	return put(key, []byte{state | bit})
}

// Some proofs carry a payload after the flags. The caller owns the copied
// value, so setting a flag preserves the rest of the row.
func noteRebuildScratchPayload(put func([]byte, []byte) error, key, state []byte, bit byte) error {
	if len(state) == 0 || state[0]&bit != 0 {
		return nil
	}
	state[0] |= bit
	return put(key, state)
}

type rebuildScratchEdgeVisit func(context.Context, func(*graph.Edge) error) error

// visitRebuildScratchKeyedEdges reads bare edge values. ordinalBytes is zero
// for key-ordered proposals and eight for proposals with a leading ordinal.
func visitRebuildScratchKeyedEdges(ctx context.Context, db *pebble.DB, prefix []byte, ordinalBytes int, malformed string,
	yield func(*graph.Edge) error,
) error {
	return pendingVisit(ctx, db, prefix, func(r row) error {
		e, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		offset := len(prefix) + ordinalBytes
		if len(r.key) < offset || !bytes.Equal(r.key[offset:], pendingEdgeKey(e)) {
			return invalid(malformed)
		}
		return yield(e)
	})
}

func applyRebuildScratchEdges(ctx context.Context, visit rebuildScratchEdgeVisit, b *BuildStore, proof rebuildEdgeProof, count *uint64) error {
	return visit(ctx, func(e *graph.Edge) error {
		if err := b.putEdgeWithCustody(e, rebuildEdgeCustody{proof: proof}); err != nil {
			return err
		}
		*count++
		return nil
	})
}

type rebuildScratchRefusals struct {
	result RebuildUnknownProvenanceError
	first  []byte
}

// add retains the first row in traversal order unless the pass supplies a
// comparison key. It counts exactly the rows the caller reports.
func (r *rebuildScratchRefusals) add(id graph.EdgeIdentity, key []byte) {
	if r.result.Count == 0 || (key != nil && bytes.Compare(key, r.first) < 0) {
		r.result.First = id
		r.first = key
	}
	r.result.Count++
}

func (r *rebuildScratchRefusals) err() error {
	if r.result.Count != 0 {
		return &r.result
	}
	return nil
}

func proveRebuildScratchEndpoints(ctx context.Context, visit rebuildScratchEdgeVisit, proved func(string) (bool, error), firstKey func(*graph.Edge) []byte) error {
	var refused rebuildScratchRefusals
	if err := visit(ctx, func(e *graph.Edge) error {
		for _, id := range [...]string{e.From, e.To} {
			ok, err := proved(id)
			if err != nil {
				return err
			}
			if !ok {
				var key []byte
				if firstKey != nil {
					key = firstKey(e)
				}
				refused.add(graph.EdgeIdentityFor(e), key)
				break
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return refused.err()
}

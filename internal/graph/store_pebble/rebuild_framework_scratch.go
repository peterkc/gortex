package store_pebble

import (
	"bytes"
	"context"
	"fmt"
	"iter"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

// putRebuildScratch keeps the flush checkpoint and byte counter pass-local.
// A failed flush leaves both the buffered bytes and spill count unchanged
// from their values immediately before the flush.
func (b *BuildStore) putRebuildScratch(key, value []byte, limit, threshold int, buffered *int, spill *uint64, label string) error {
	size := len(key) + len(value)
	if size > limit {
		return fmt.Errorf("%w: %s scratch row", ErrRebuildLinkInputSize, label)
	}
	if err := b.pending.Set(key, value, pebble.NoSync); err != nil {
		return err
	}
	*buffered += size
	if *buffered >= threshold {
		before := b.pending.Metrics().Levels[0].TableBytesFlushed
		if err := b.pending.Flush(); err != nil {
			return err
		}
		*spill += b.pending.Metrics().Levels[0].TableBytesFlushed - before
		*buffered = 0
	}
	return nil
}

const rebuildScratchExact = rebuildCapabilityWanted | rebuildCapabilityProved

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

func recordRebuildScratchReceipts(receipts iter.Seq2[ParseFacts, error], record func(ParseFacts) error) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		if err := record(facts); err != nil {
			return err
		}
	}
	return nil
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

package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
	"math"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/clones"
)

const rebuildClonePostingBytes = 64 << 10

type RebuildClonePairStats struct {
	Pairs, SkippedBuckets, SkippedBucketItems uint64
}

// VisitRebuildClonePairs consumes already-finalized unique signatures on a
// quiet private build. The native hash admission runs before scoring, with
// class, band, native bucket hash and ID order. Only then are scores reduced
// by full pair identity. No graph writes occur. Results are tentative until
// the visitor and scratch cleanup return nil.
func (b *BuildStore) VisitRebuildClonePairs(ctx context.Context, items iter.Seq2[clones.Item, error],
	threshold float64, yield func(clones.Pair) error,
) (RebuildClonePairStats, error) {
	return b.visitRebuildClonePairs(ctx, items, threshold, clones.CandidateHashLimit, yield)
}

func (b *BuildStore) visitRebuildClonePairs(ctx context.Context, items iter.Seq2[clones.Item, error],
	threshold float64, hashLimit int, yield func(clones.Pair) error,
) (stats RebuildClonePairStats, err error) {
	prefix := []byte{'g', 'L'}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return stats, err
	}
	if occupied {
		return stats, invalid("clone pair scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
		if err != nil {
			stats = RebuildClonePairStats{}
		}
	}()
	if threshold <= 0 {
		threshold = clones.DefaultThreshold
	}
	postings := []byte{'g', 'L', 'b'}
	for item, err := range items {
		if err != nil {
			return stats, err
		}
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if err := b.stageRebuildCloneItem(postings, item); err != nil {
			return stats, err
		}
	}
	pass := rebuildClonePairPass{db: b.pending, threshold: threshold, hashLimit: hashLimit}
	if err := pass.scoreBuckets(ctx, postings, &stats); err != nil {
		return stats, err
	}
	err = pass.visitPairs(ctx, &stats, yield)
	return stats, err
}

func (b *BuildStore) stageRebuildCloneItem(postings []byte, item clones.Item) error {
	if item.ID == "" {
		return invalid("empty clone item ID")
	}
	if len(item.ID) > rebuildClonePostingBytes/2-32 { // allow binary key escaping
		return fmt.Errorf("%w: clone item ID", ErrRebuildLinkInputSize)
	}
	key := keyStrings([]byte{'g', 'L', 'i'}, item.ID)
	present, err := pendingGet(b.pending, key, fnPointerPresent)
	if err != nil {
		return err
	}
	if present {
		return invalid("duplicate clone item ID")
	}
	if err := b.pending.Set(key, nil, pebble.NoSync); err != nil {
		return err
	}
	var sig [clones.NumHashes * 4]byte
	for i, slot := range item.Sig {
		binary.LittleEndian.PutUint32(sig[4*i:], slot)
	}
	return clones.VisitStratifiedBandHashes(item, func(class, band int, hash uint64) error {
		k := append(bytes.Clone(postings), byte(class), byte(band))
		k = appendKeyString(binary.BigEndian.AppendUint64(k, hash), item.ID)
		return b.pending.Set(k, sig[:], pebble.NoSync)
	})
}

type rebuildClonePairPass struct {
	db        *pebble.DB
	threshold float64
	hashLimit int
	hashes    int
}

func (p *rebuildClonePairPass) scoreBuckets(ctx context.Context, prefix []byte, stats *RebuildClonePairStats) error {
	const bucketKeyBytes = 10 // class, band and big-endian native hash
	var key [bucketKeyBytes]byte
	var members [clones.BandBucketLimit]clones.Item
	var count uint64
	drain := func() error {
		defer func() { clear(members[:min(count, uint64(len(members)))]); count = 0 }()
		if count > uint64(len(members)) {
			stats.SkippedBuckets++
			stats.SkippedBucketItems += count
			return nil
		}
		for i := range int(count) {
			for j := i + 1; j < int(count); j++ {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := p.score(members[i], members[j]); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := pendingVisit(ctx, p.db, prefix, func(r row) error {
		k := r.key[len(prefix):]
		if len(k) <= bucketKeyBytes || len(r.value) != clones.NumHashes*4 {
			return invalid("malformed clone posting")
		}
		if count != 0 && !bytes.Equal(key[:], k[:bucketKeyBytes]) {
			if err := drain(); err != nil {
				return err
			}
			if key[0] != k[0] {
				if err := p.clearHashes(); err != nil {
					return err
				}
				p.hashes = 0
			}
		}
		copy(key[:], k[:bucketKeyBytes])
		count++
		if count > uint64(len(members)) {
			return nil
		}
		id, tail, err := readKeyString(k[bucketKeyBytes:])
		if err != nil || id == "" || len(tail) != 0 {
			return invalid("malformed clone posting ID")
		}
		item := &members[count-1]
		item.ID = id
		for i := range item.Sig {
			item.Sig[i] = binary.LittleEndian.Uint32(r.value[4*i:])
		}
		return nil
	}); err != nil {
		return err
	}
	return drain()
}

func (p *rebuildClonePairPass) clearHashes() error {
	prefix := []byte{'g', 'L', 'h'}
	return p.db.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync)
}

func (p *rebuildClonePairPass) score(a, b clones.Item) error {
	// Bucket members are already in ascending ID order. Count even hashes
	// that fail scoring. The cutoff hash itself reaches scoring; thereafter
	// all candidates in this class reach it, including hash collisions.
	if p.hashes < p.hashLimit {
		key := binary.BigEndian.AppendUint64([]byte{'g', 'L', 'h'}, clones.CandidatePairHash(a.ID, b.ID))
		present, err := pendingGet(p.db, key, fnPointerPresent)
		if err != nil || present {
			return err
		}
		p.hashes++
		if p.hashes == p.hashLimit {
			if err := p.clearHashes(); err != nil {
				return err
			}
		} else if err := p.db.Set(key, nil, pebble.NoSync); err != nil {
			return err
		}
	}
	score := clones.EstimateJaccard(a.Sig, b.Sig)
	if score < p.threshold {
		return nil
	}
	// This key performs the native full-pair merge after scoring. Every
	// occurrence of a pair gets the same score from its one saved signature.
	key := binary.BigEndian.AppendUint64([]byte{'g', 'L', 'p'}, ^math.Float64bits(score))
	return p.db.Set(keyStrings(key, a.ID, b.ID), nil, pebble.NoSync)
}

func (p *rebuildClonePairPass) visitPairs(ctx context.Context, stats *RebuildClonePairStats, yield func(clones.Pair) error) error {
	prefix := []byte{'g', 'L', 'p'}
	return pendingVisit(ctx, p.db, prefix, func(r row) error {
		k := r.key[len(prefix):]
		if len(k) < 8 {
			return invalid("malformed clone pair score")
		}
		a, rest, err := readKeyString(k[8:])
		if err != nil {
			return err
		}
		b, rest, err := readKeyString(rest)
		if err != nil || a == "" || a >= b || len(rest) != 0 {
			return invalid("malformed clone pair identity")
		}
		if err := yield(clones.Pair{A: a, B: b, Similarity: math.Float64frombits(^binary.BigEndian.Uint64(k))}); err != nil {
			return err
		}
		stats.Pairs++
		return nil
	})
}

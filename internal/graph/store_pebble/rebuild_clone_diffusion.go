package store_pebble

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/clones"
)

// VisitRebuildDiffusedClonePairs consumes unique canonical, materializable clone
// pairs from a quiet private build. bridgeScore returns zero for a rejected
// bridge. Only fanout neighbors of one bridge are held at a time. The sink
// must discard tentative results on error, including a scratch-cleanup error.
func (b *BuildStore) VisitRebuildDiffusedClonePairs(ctx context.Context,
	visitDirect func(func(clones.Pair) error) error, fanout, limit int,
	bridgeScore func(float64, float64) float64, yield func(clones.Pair) error,
) (pairs int, capped bool, err error) {
	prefix := []byte{'g', 'D'}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return 0, false, err
	}
	if occupied {
		return 0, false, invalid("clone diffusion scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
		if err != nil {
			pairs, capped = 0, false
		}
	}()
	pass := rebuildCloneDiffusion{
		db: b.pending, neighbors: []byte{'g', 'D', 'n'}, ranked: []byte{'g', 'D', 'r'}, seen: []byte{'g', 'D', 's'},
	}
	if err := visitDirect(func(pair clones.Pair) error { return pass.stage(ctx, pair) }); err != nil {
		return 0, false, err
	}
	if err := pass.rank(ctx, fanout, bridgeScore); err != nil {
		return 0, false, err
	}
	return pass.visit(ctx, limit, yield)
}

type rebuildCloneDiffusion struct {
	db                      *pebble.DB
	neighbors, ranked, seen []byte
}

func (p rebuildCloneDiffusion) stage(ctx context.Context, pair clones.Pair) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if pair.A == "" || pair.A >= pair.B || !(pair.Similarity > 0 && pair.Similarity <= 1) {
		return invalid("invalid direct clone pair for diffusion")
	}
	if max(len(pair.A), len(pair.B)) > rebuildClonePostingBytes/2-32 {
		return fmt.Errorf("%w: clone diffusion endpoint ID", ErrRebuildLinkInputSize)
	}
	key := keyStrings(p.seen, pair.A, pair.B)
	present, err := pendingGet(p.db, key, fnPointerPresent)
	if err != nil {
		return err
	}
	if present {
		return invalid("duplicate direct clone pair for diffusion")
	}
	if err := p.db.Set(key, nil, pebble.NoSync); err != nil {
		return err
	}
	// Group by bridge, then native score and neighbor ID order.
	for _, ids := range [2][2]string{{pair.A, pair.B}, {pair.B, pair.A}} {
		key := keyStrings(p.neighbors, ids[0])
		key = keyStrings(appendCloneDiffusionScore(key, pair.Similarity), ids[1])
		if err := p.db.Set(key, nil, pebble.NoSync); err != nil {
			return err
		}
	}
	return nil
}

func (p rebuildCloneDiffusion) rank(ctx context.Context, fanout int, bridgeScore func(float64, float64) float64) error {
	type neighbor struct {
		id    string
		score float64
	}
	var bridge string
	nbrs := make([]neighbor, 0, fanout)
	drain := func() error {
		for i, left := range nbrs {
			for _, right := range nbrs[i+1:] {
				score := bridgeScore(left.score, right.score)
				if score == 0 {
					continue
				}
				a, b := left.id, right.id
				if a > b {
					a, b = b, a
				}
				key := appendCloneDiffusionScore(p.ranked, score)
				if err := p.db.Set(keyStrings(key, a, b), nil, pebble.NoSync); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := pendingVisit(ctx, p.db, p.neighbors, func(r row) error {
		owner, rest, err := readKeyString(r.key[len(p.neighbors):])
		if err != nil || owner == "" || len(rest) < 8 {
			return invalid("malformed clone diffusion neighbor key")
		}
		if owner != bridge {
			if err := drain(); err != nil {
				return err
			}
			clear(nbrs)
			nbrs, bridge = nbrs[:0], owner
		}
		if len(nbrs) == fanout {
			return nil
		}
		id, tail, err := readKeyString(rest[8:])
		if err != nil || id == "" || len(tail) != 0 {
			return invalid("malformed clone diffusion neighbor ID")
		}
		nbrs = append(nbrs, neighbor{id: id, score: math.Float64frombits(^binary.BigEndian.Uint64(rest))})
		return nil
	}); err != nil {
		return err
	}
	return drain()
}

// Descending score then canonical IDs. The first hit for a non-direct pair
// is its strongest bridge. Mark it in the same exact set as the direct pairs
// so no weaker bridge gets a slot. Check one extra distinct pair for the cap.
func (p rebuildCloneDiffusion) visit(ctx context.Context, limit int, yield func(clones.Pair) error) (pairs int, capped bool, err error) {
	it, err := p.db.NewIterWithContext(ctx, &pebble.IterOptions{LowerBound: p.ranked, UpperBound: prefixEnd(p.ranked)})
	if err != nil {
		return 0, false, err
	}
	defer func() { err = errors.Join(err, it.Close()) }()
	for ok := it.First(); ok; ok = it.Next() {
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		k := it.Key()[len(p.ranked):]
		if len(k) < 8 {
			return 0, false, invalid("malformed clone diffusion score")
		}
		a, rest, err := readKeyString(k[8:])
		if err != nil {
			return 0, false, err
		}
		b, rest, err := readKeyString(rest)
		if err != nil || a == "" || a >= b || len(rest) != 0 {
			return 0, false, invalid("malformed clone diffusion pair identity")
		}
		key := keyStrings(p.seen, a, b)
		present, err := pendingGet(p.db, key, fnPointerPresent)
		if err != nil {
			return 0, false, err
		}
		if present {
			continue
		}
		if pairs == limit {
			return pairs, true, nil
		}
		if err := yield(clones.Pair{A: a, B: b, Similarity: math.Float64frombits(^binary.BigEndian.Uint64(k))}); err != nil {
			return 0, false, err
		}
		pairs++
		if err := p.db.Set(key, nil, pebble.NoSync); err != nil {
			return 0, false, err
		}
	}
	return pairs, false, errors.Join(ctx.Err(), it.Error())
}

func appendCloneDiffusionScore(key []byte, score float64) []byte {
	return binary.BigEndian.AppendUint64(key, ^math.Float64bits(score))
}

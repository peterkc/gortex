package store_pebble

import (
	"context"
	"encoding/binary"
	"errors"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

var rebuildContractRecordPrefix = []byte{'g', 'q', 'c'}

// RebuildContractRecord is a private logical row. A body-dependent D83
// omission keeps its exact ID and winner placement, but has no proved payload.
type RebuildContractRecord struct {
	contracts.Contract
	BodyUnproved bool
}

// RebuildContractRecords keeps mutable contract buckets in the build's private
// pending index. It is not a graph.Store or a serving registry. All access is
// serial, and valid only inside WithRebuildContractRecords.
type RebuildContractRecords struct {
	build        *BuildStore
	ctx          context.Context
	next         uint64
	representing bool
}

// WithRebuildContractRecords borrows the contract-record key range for one
// pass. The caller discards tentative results on any error.
func (b *BuildStore) WithRebuildContractRecords(ctx context.Context, use func(*RebuildContractRecords) error) (err error) {
	has, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildContractRecordPrefix)
	if err != nil {
		return err
	}
	if has || use == nil {
		return invalid("contract record scratch is occupied or missing owner")
	}
	if err := b.putRebuildScratch(rebuildContractRecordKey('a'), nil, goldBufferBytes, "contract record scope"); err != nil {
		return err
	}
	r := &RebuildContractRecords{build: b, ctx: ctx}
	defer func() {
		r.build = nil
		err = errors.Join(err, b.pending.DeleteRange(rebuildContractRecordPrefix, prefixEnd(rebuildContractRecordPrefix), pebble.NoSync))
	}()
	return errors.Join(use(r), r.ready())
}

func (r *RebuildContractRecords) ready() error {
	if r == nil || r.build == nil {
		return invalid("contract record scratch is not active")
	}
	return errors.Join(r.ctx.Err(), r.build.RebuildView().mutationError())
}

func rebuildContractRecordKey(column byte, fields ...string) []byte {
	return keyStrings([]byte{'g', 'q', 'c', column}, fields...)
}

// Add appends one row in the same order as Registry.Add.
func (r *RebuildContractRecords) Add(item RebuildContractRecord) error {
	if err := r.ready(); err != nil {
		return err
	}
	if item.ID == "" || r.next == ^uint64(0) {
		return invalid("contract record without ID or ordinal")
	}
	key := binary.BigEndian.AppendUint64(rebuildContractRecordKey('r', item.ID), r.next)
	if err := r.write(key, item); err != nil {
		return err
	}
	if err := r.build.putRebuildScratch(rebuildContractRecordKey('i', item.ID), nil, goldBufferBytes, "contract ID"); err != nil {
		return err
	}
	r.next++
	return nil
}

func (r *RebuildContractRecords) write(key []byte, item RebuildContractRecord) error {
	e := contractFactsEncoder{}
	e.boolean(item.BodyUnproved)
	writeFactRecord(&e, item.Contract)
	if e.err != nil {
		return e.err
	}
	return r.build.putRebuildScratch(key, e.buf, goldBufferBytes, "contract record")
}

// FirstOutputEdge claims the native insert identity in emission order. Call it
// before selecting coverage, even for an omitted first candidate. An existing
// ordinary edge belongs to Relations and is never a Contracts addition. A later
// duplicate must not replace the native first payload. This touches only the
// record scratch range; PrepareContracts still checks uniqueness independently.
func (r *RebuildContractRecords) FirstOutputEdge(e *graph.Edge) (bool, error) {
	if err := r.ready(); err != nil {
		return false, err
	}
	if e == nil || e.From == "" || e.To == "" || e.Kind == "" {
		return false, invalid("contract output edge without identity")
	}
	physical := pendingEdgeKey(e)
	incumbent, err := pendingGet(r.build.pending, physical, func([]byte) (bool, error) { return true, nil })
	if err != nil || incumbent {
		return false, err
	}
	key := append(rebuildContractRecordKey('e'), physical...)
	seen, err := pendingGet(r.build.pending, key, func([]byte) (bool, error) { return true, nil })
	if err != nil || seen {
		return false, err
	}
	if err := r.build.putRebuildScratch(key, nil, goldBufferBytes, "contract output edge"); err != nil {
		return false, err
	}
	return true, nil
}

// ClearID removes the rows of a live ID, retaining the raw bucket key just
// like Registry.ReplaceByID(id, nil).
func (r *RebuildContractRecords) ClearID(id string) error {
	if err := r.ready(); err != nil {
		return err
	}
	if id == "" {
		return invalid("empty contract bucket ID")
	}
	if err := r.build.putRebuildScratch(rebuildContractRecordKey('i', id), nil, goldBufferBytes, "contract ID"); err != nil {
		return err
	}
	prefix := rebuildContractRecordKey('r', id)
	return r.build.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync)
}

// VisitIDs streams raw bucket keys in ascending ID order, including empty keys.
func (r *RebuildContractRecords) VisitIDs(yield func(string) error) error {
	if err := r.ready(); err != nil {
		return err
	}
	prefix := rebuildContractRecordKey('i')
	return pendingVisit(r.ctx, r.build.pending, prefix, func(s row) error {
		id, rest, err := readKeyString(s.key[len(prefix):])
		if err != nil || id == "" || len(rest) != 0 || len(s.value) != 0 {
			return invalid("invalid contract bucket key")
		}
		return yield(id)
	})
}

// VisitByID streams a bucket in its insertion order, without deduplication.
func (r *RebuildContractRecords) VisitByID(id string, yield func(RebuildContractRecord) error) error {
	if err := r.ready(); err != nil {
		return err
	}
	return r.visit(rebuildContractRecordKey('r', id), func(_ []byte, item RebuildContractRecord) error {
		return yield(item)
	})
}

// UpdateByID changes the current bucket's matching rows without changing their
// ordinals. The caller reports whether each visited row changed.
func (r *RebuildContractRecords) UpdateByID(id string, update func(*RebuildContractRecord) (bool, error)) error {
	if err := r.ready(); err != nil {
		return err
	}
	return r.visit(rebuildContractRecordKey('r', id), func(key []byte, item RebuildContractRecord) error {
		if changed, err := update(&item); err != nil || !changed {
			return err
		}
		if item.ID != id {
			return invalid("contract bucket update changed ID")
		}
		return r.write(key, item)
	})
}

// VisitRepresentatives freezes the current rows for this scan. It uses the
// first member of each Registry.All key. Callbacks can update live buckets
// without affecting this scan. Cross-ID delimiter collisions have no stable
// native winner. The optional relevance predicate can prove a collision
// immaterial to this pass; without it, keep the conservative refusal.
func (r *RebuildContractRecords) VisitRepresentatives(yield func(RebuildContractRecord) error, relevant ...func(RebuildContractRecord) bool) error {
	return r.visitRepresentatives(false, yield, relevant...)
}

// VisitCurrentRepresentatives freezes representative membership and reads each
// member's current value when visited. The callback may change metadata, not
// bucket identity or order. This models Registry.All snapshots of shared Meta.
func (r *RebuildContractRecords) VisitCurrentRepresentatives(yield func(RebuildContractRecord) error) error {
	return r.visitRepresentatives(true, yield)
}

func (r *RebuildContractRecords) visitRepresentatives(current bool, yield func(RebuildContractRecord) error, relevant ...func(RebuildContractRecord) bool) (err error) {
	if err := r.ready(); err != nil {
		return err
	}
	if r.representing {
		return invalid("contract representative scan is already active")
	}
	r.representing = true
	seen := rebuildContractRecordKey('s')
	defer func() {
		r.representing = false
		err = errors.Join(err, r.build.pending.DeleteRange(seen, prefixEnd(seen), pebble.NoSync))
	}()
	return r.visit(rebuildContractRecordKey('r'), func(rowKey []byte, item RebuildContractRecord) error {
		key := appendKeyString(rebuildContractRecordKey('s'), item.ID+"|"+item.FilePath+"|"+item.SymbolID+"|"+string(item.Role))
		first, err := pendingGet(r.build.pending, key, copyValue)
		if err != nil {
			return err
		}
		affects := len(relevant) == 0 || relevant[0](item)
		if len(first) != 0 {
			if string(first[1:]) != item.ID && (first[0] != 0 || affects) {
				return invalid("ambiguous contract representative identity across IDs")
			}
			return nil
		}
		if err := r.build.putRebuildScratch(key, append(appendBool(nil, affects), item.ID...), goldBufferBytes, "contract representative"); err != nil {
			return err
		}
		if current {
			live, err := pendingGet(r.build.pending, rowKey, decodeRebuildContractRecord)
			if err != nil {
				return err
			}
			if live.ID != item.ID || live.FilePath != item.FilePath || live.SymbolID != item.SymbolID || live.Role != item.Role {
				return invalid("live contract representative changed identity")
			}
			item = live
		}
		return yield(item)
	})
}

func (r *RebuildContractRecords) visit(prefix []byte, yield func([]byte, RebuildContractRecord) error) error {
	return pendingVisit(r.ctx, r.build.pending, prefix, func(s row) error {
		id, ordinal, err := readKeyString(s.key[len(rebuildContractRecordPrefix)+1:])
		if err != nil || id == "" || len(ordinal) != 8 {
			return invalid("invalid contract record key")
		}
		item, err := decodeRebuildContractRecord(s.value)
		if err != nil {
			return err
		}
		if item.ID != id {
			return invalid("contract record disagrees with key")
		}
		return yield(s.key, item)
	})
}

func decodeRebuildContractRecord(raw []byte) (RebuildContractRecord, error) {
	d := recordCursor{buf: raw}
	unproved := d.boolean()
	c := readFactRecord(&d)
	return RebuildContractRecord{Contract: c, BodyUnproved: unproved}, d.finish()
}

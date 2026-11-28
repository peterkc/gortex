package store_pebble

import (
	"errors"

	"github.com/cockroachdb/pebble/v2"
)

// WithOrderedRepresentatives freezes the elected Registry.All rows in native
// graph emission order. The serial reader is valid only during use and may
// be called repeatedly. The logical buckets and their empty keys stay live.
func (r *RebuildContractRecords) WithOrderedRepresentatives(use func(read func(func(RebuildContractRecord) error) error) error) (err error) {
	if err := r.ready(); err != nil {
		return err
	}
	prefix := rebuildContractRecordKey('o')
	occupied, err := rebuildGuardHasPrefix(r.ctx, r.build.pending, prefix)
	if err != nil {
		return err
	}
	if occupied || use == nil {
		return invalid("contract order scratch is occupied or missing owner")
	}
	if err := r.build.putRebuildScratch(append(prefix, 'a'), nil, goldBufferBytes, "ordered contract records"); err != nil {
		return err
	}
	active := true
	defer func() {
		active = false
		err = errors.Join(err, r.build.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
	}()
	rows := append(prefix, 'r')
	if err := r.VisitRepresentatives(func(item RebuildContractRecord) error {
		key := keyStrings(append([]byte(nil), rows...), item.ID, item.FilePath, item.SymbolID, string(item.Role))
		return r.write(appendKeyLine(key, item.Line), item)
	}); err != nil {
		return err
	}
	return use(func(yield func(RebuildContractRecord) error) error {
		if !active || yield == nil {
			return invalid("inactive ordered contract reader or missing visitor")
		}
		return pendingVisit(r.ctx, r.build.pending, rows, func(s row) error {
			item, err := decodeRebuildContractRecord(s.value)
			if err != nil {
				return err
			}
			return yield(item)
		})
	})
}

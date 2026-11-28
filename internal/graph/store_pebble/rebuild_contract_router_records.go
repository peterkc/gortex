package store_pebble

import (
	"errors"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/contracts"
)

func rebuildRouterRecordKey(column byte, fields ...string) []byte {
	return keyStrings(append(rebuildContractRecordKey('m'), column), fields...)
}

// RewriteRouterPrefixes freezes opened buckets and their live siblings before
// any move. prefix answers the checked router lookup for an eligible record;
// check refuses any missing inputs accumulated by those lookups.
func (r *RebuildContractRecords) RewriteRouterPrefixes(prefix func(contracts.Contract) (string, error), check func() error, overlap func(string) error) (err error) {
	if err := r.ready(); err != nil {
		return err
	}
	scratch := rebuildContractRecordKey('m')
	defer func() {
		err = errors.Join(err, r.build.pending.DeleteRange(scratch, prefixEnd(scratch), pebble.NoSync))
	}()
	put := func(column byte, fields ...string) error {
		return r.build.putRebuildScratch(rebuildRouterRecordKey(column, fields...), []byte{1}, goldBufferBytes, "router rewrite key")
	}
	present := func(column byte, field string) (bool, error) {
		value, err := rebuildScratchState(r.build.pending, rebuildRouterRecordKey(column, field), "invalid router rewrite key")
		return value != 0, err
	}
	visitKeys := func(column byte, yield func(string) error) error {
		p := rebuildRouterRecordKey(column)
		return pendingVisit(r.ctx, r.build.pending, p, func(s row) error {
			key, rest, err := readKeyString(s.key[len(p):])
			if err != nil || len(rest) != 0 {
				return invalid("invalid router rewrite field")
			}
			return yield(key)
		})
	}
	eligiblePrefix := func(c contracts.Contract) (string, error) {
		if local, _ := contracts.RouterPrefixFactUse(c, nil); !local {
			return "", nil
		}
		return prefix(c)
	}
	if err := r.VisitRepresentatives(func(item RebuildContractRecord) error {
		value, err := eligiblePrefix(item.Contract)
		if err != nil || value == "" {
			return err
		}
		return put('o', item.ID)
	}); err != nil {
		return err
	}
	if err := visitKeys('o', func(id string) error {
		source := rebuildContractRecordKey('r', id)
		return r.visit(source, func(key []byte, item RebuildContractRecord) error {
			value, err := eligiblePrefix(item.Contract)
			if err != nil {
				return err
			}
			if value != "" {
				item.Contract = contracts.RewriteRouterContract(item.Contract, value)
			}
			if item.ID != id {
				opened, err := present('o', item.ID)
				if err != nil {
					return err
				}
				if opened {
					if err := put('v', id); err != nil {
						return err
					}
					if err := put('v', item.ID); err != nil {
						return err
					}
				}
			}
			frozen := rebuildRouterRecordKey('r', id)
			return r.write(append(frozen, key[len(source):]...), item)
		})
	}); err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	visitFrozen := func(id string, yield func(RebuildContractRecord) error) error {
		p := rebuildRouterRecordKey('r', id)
		return pendingVisit(r.ctx, r.build.pending, p, func(s row) error {
			if len(s.key)-len(p) != 8 {
				return invalid("invalid frozen router ordinal")
			}
			item, err := decodeRebuildContractRecord(s.value)
			if err != nil {
				return err
			}
			return yield(item)
		})
	}
	if err := visitKeys('v', func(id string) error {
		return visitFrozen(id, func(item RebuildContractRecord) error { return put('f', item.FilePath) })
	}); err != nil {
		return err
	}
	if err := visitKeys('f', overlap); err != nil {
		return err
	}
	return visitKeys('o', func(id string) error {
		if err := r.ClearID(id); err != nil {
			return err
		}
		return visitFrozen(id, r.Add)
	})
}

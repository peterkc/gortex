package store_pebble

import (
	"errors"
	"strings"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

// UpgradeBareTypeRefs opens type lookups from proved frozen representatives,
// then applies those answers to proved live records, including hidden siblings.
// Mark a row unproved if an unproved representative could open a real upgrade.
// markHeldName can label a lookup's record scope; it does not supply an answer.
func (r *RebuildContractRecords) UpgradeBareTypeRefs(markHeldName func(contracts.Contract, string) error) (err error) {
	if err := r.ready(); err != nil {
		return err
	}
	prefix := rebuildContractRecordKey('t')
	defer func() {
		err = errors.Join(err, r.build.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
	}()
	var opened, uncertain bool
	fields := [...]string{"request_type", "response_type"}
	if err := r.VisitRelevantRepresentatives(func(item RebuildContractRecord) error {
		if item.BodyUnproved {
			uncertain = true
			return nil
		}
		for _, field := range fields {
			name, _ := item.Meta[field].(string)
			if name == "" || strings.Contains(name, "::") {
				continue
			}
			if err := r.build.putRebuildScratch(rebuildContractRecordKey('t', name), []byte{1}, goldBufferBytes, "contract bare type"); err != nil {
				return err
			}
			opened = true
		}
		return nil
	}, func(item RebuildContractRecord) bool {
		if item.BodyUnproved {
			return true
		}
		for _, field := range fields {
			name, _ := item.Meta[field].(string)
			if name != "" && !strings.Contains(name, "::") {
				return true
			}
		}
		return false
	}); err != nil || (!opened && !uncertain) {
		return err
	}
	answers := make(map[[2]string]string)
	return r.VisitIDs(func(id string) error {
		return r.UpdateByID(id, func(item *RebuildContractRecord) (bool, error) {
			if item.BodyUnproved {
				return false, nil
			}
			// Native revisits the same Meta through ByID, ByRepo, ByFile
			// and, for a symbol-bound record, BySymbol. A full ID stops
			// after one upgrade; a short ID can still be an opened name.
			turns := 3
			if item.SymbolID != "" {
				turns++
			}
			var changed bool
			for _, field := range fields {
				for range turns {
					name, _ := item.Meta[field].(string)
					if name == "" || strings.Contains(name, "::") {
						break
					}
					state, err := rebuildScratchState(r.build.pending, rebuildContractRecordKey('t', name), "invalid contract bare type key")
					if err != nil {
						return false, err
					}
					if state == 0 && !uncertain {
						break
					}
					if markHeldName != nil {
						if err := markHeldName(item.Contract, name); err != nil {
							return false, err
						}
					}
					key := [2]string{name, item.RepoPrefix}
					value, known := answers[key]
					if !known {
						value, err = r.bareTypeID(name, item.RepoPrefix)
						if err != nil {
							return false, err
						}
						answers[key] = value
					}
					if value == "" || value == name {
						break
					}
					if state == 0 {
						item.BodyUnproved = true
						return true, nil
					}
					item.Meta[field] = value
					changed = true
				}
			}
			return changed, nil
		})
	})
}

func (r *RebuildContractRecords) bareTypeID(name, repo string) (string, error) {
	var same, other string
	var sameCount, otherCount int
	err := r.build.RebuildView().VisitNameCandidates(r.ctx, name, nil, func(n *graph.Node) error {
		if n.Kind != graph.KindType {
			return nil
		}
		if repo != "" && strings.HasPrefix(n.ID, repo+"/") {
			same = n.ID
			sameCount = min(sameCount+1, 2)
		} else {
			other = n.ID
			otherCount = min(otherCount+1, 2)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if sameCount == 1 {
		return same, nil
	}
	if sameCount == 0 && otherCount == 1 {
		return other, nil
	}
	return "", nil
}

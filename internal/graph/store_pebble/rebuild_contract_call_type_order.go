package store_pebble

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

type rebuildCallTypeChoice struct {
	id    string
	count int
}

func readRebuildCallTypeChoice(raw []byte) (rebuildCallTypeChoice, error) {
	d := recordCursor{buf: raw}
	choice := rebuildCallTypeChoice{id: d.str(), count: d.integer()}
	if choice.id == "" || choice.count < 0 {
		return rebuildCallTypeChoice{}, invalid("invalid call return type choice")
	}
	return choice, d.finish()
}

// ResolveCallReturnTypeName chooses the lowest ID in the native deciding
// KindType tier (repo-prefixed IDs first, all others second). The result is
// reused within this pass over the same frozen linked candidates.
func (r *RebuildContractRecords) ResolveCallReturnTypeName(repo, name string) (string, error) {
	if err := r.ready(); err != nil {
		return "", err
	}
	if name == "" || strings.Contains(name, "::") {
		return name, nil
	}
	key := rebuildContractRecordKey('y', repo, name)
	known, err := pendingGet(r.build.pending, key, readRebuildCallTypeChoice)
	if err != nil || known.id != "" {
		return known.id, err
	}
	var preferred, fallback rebuildCallTypeChoice
	if err := r.build.RebuildView().VisitNameCandidates(r.ctx, name, nil, func(n *graph.Node) error {
		if n.Kind != graph.KindType {
			return nil
		}
		tier := &fallback
		if repo != "" && strings.HasPrefix(n.ID, repo+"/") {
			tier = &preferred
		}
		if tier.id == "" || n.ID < tier.id {
			tier.id = n.ID
		}
		tier.count++
		return nil
	}); err != nil {
		return "", err
	}
	choice := preferred
	if choice.count == 0 {
		choice = fallback
	}
	if choice.count == 0 {
		choice.id = name
	}
	e := contractFactsEncoder{}
	e.strings(choice.id)
	e.integer(choice.count)
	if err := r.build.putRebuildScratch(key, e.buf, goldBufferBytes, "contract call return type"); err != nil {
		return "", err
	}
	return choice.id, nil
}

// VisitCallReturnTypeOrderDifferences reports one case per bare name and repo
// whose deciding tier contains multiple types. Losing tiers do not count.
func (r *RebuildContractRecords) VisitCallReturnTypeOrderDifferences(yield func(repo, name, id string, candidates int) error) error {
	if err := r.ready(); err != nil {
		return err
	}
	prefix := rebuildContractRecordKey('y')
	return pendingVisit(r.ctx, r.build.pending, prefix, func(s row) error {
		choice, err := readRebuildCallTypeChoice(s.value)
		if err != nil || choice.count < 2 {
			return err
		}
		repo, rest, err := readKeyString(s.key[len(prefix):])
		if err != nil {
			return err
		}
		name, rest, err := readKeyString(rest)
		if err != nil || name == "" || len(rest) != 0 {
			return invalid("invalid call return type order key")
		}
		return yield(repo, name, choice.id, choice.count)
	})
}

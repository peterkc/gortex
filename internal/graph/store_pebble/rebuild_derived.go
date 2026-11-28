package store_pebble

import (
	"path/filepath"
	"slices"

	"github.com/zeebo/blake3"
)

// UnsupportedFamily names a family that is deliberately not built. It is not
// a zero-row FamilyWitness, which would claim the family was built.
type UnsupportedFamily struct {
	Family string
	Reason string
}

var d64Unsupported = [...]UnsupportedFamily{
	{"analysis_cache", "D64"},
	{"blame", "D64"},
	{"churn", "D64"},
	{"content_search", "D64"},
	{"contracts", "D64"},
	{"coverage", "D64"},
	{"releases", "D64"},
}

func deferredFamilies(contractsBuilt bool) []UnsupportedFamily {
	if !contractsBuilt {
		return d64Unsupported[:]
	}
	return slices.DeleteFunc(slices.Clone(d64Unsupported[:]), func(f UnsupportedFamily) bool {
		return f.Family == "contracts"
	})
}

// PrepareDeferredDerived advances only the witness. The predecessor is
// Contracts, or Relations on the legacy path without contract storage.
func PrepareDeferredDerived(dir string, prior StageTables) (StageTables, error) {
	w := prior.Witness
	if !filepath.IsAbs(dir) || prior.Incarnation == "" || (w.Stage != StageContracts && w.Stage != StageRelations) {
		return StageTables{}, invalid("derived needs its predecessor witness and stage directory")
	}
	raw, err := encodeWitness(w)
	if err != nil {
		return StageTables{}, err
	}
	w.Stage = StageDerived
	w.Families = slices.Clone(w.Families)
	w.Relations = cloneRelationCoverage(w.Relations)
	w.Unsupported = slices.Clone(deferredFamilies(w.Contracts != nil))
	if w.Contracts != nil {
		coverage := *w.Contracts
		w.Contracts = &coverage
	}
	return StageTables{Dir: dir, Incarnation: prior.Incarnation, Base: blake3.Sum256(raw), Witness: w}, nil
}

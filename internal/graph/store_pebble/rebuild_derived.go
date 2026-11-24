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

// PrepareDeferredDerived advances only the witness. It does not build an
// analysis family or change any relation rows or coverage labels.
func PrepareDeferredDerived(dir string, relations StageTables) (StageTables, error) {
	if !filepath.IsAbs(dir) || relations.Witness.Stage != StageRelations || relations.Incarnation == "" {
		return StageTables{}, invalid("derived needs its relations witness and stage directory")
	}
	w := relations.Witness
	raw, err := encodeWitness(w)
	if err != nil {
		return StageTables{}, err
	}
	w.Stage = StageDerived
	w.Families = slices.Clone(w.Families)
	w.Unsupported = slices.Clone(d64Unsupported[:])
	return StageTables{Dir: dir, Incarnation: relations.Incarnation, Base: blake3.Sum256(raw), Witness: w}, nil
}

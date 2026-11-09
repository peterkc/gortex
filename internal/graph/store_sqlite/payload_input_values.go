package store_sqlite

import "github.com/zzet/gortex/internal/storage"

// NewPayloadInputRevision constructs an opaque value using a backend-private
// source identity. It performs no I/O and grants no publication authority.
func NewPayloadInputRevision(source *byte, generation int64, analysis, input, admin uint64) PayloadInputRevision {
	return storage.NewPayloadInputRevision(source, generation, analysis, input, admin)
}

// NewPayloadInputWitnessRevision constructs one immutable captured row.
func NewPayloadInputWitnessRevision(generation int64, analysis, constants, bindings uint64, state string) PayloadInputWitnessRevision {
	return storage.NewPayloadInputWitnessRevision(generation, analysis, constants, bindings, state)
}

// NewPayloadInputWitness copies its rows and retains only opaque process-local
// identity and captured values. Backend validation remains publication authority.
func NewPayloadInputWitness(source *byte, admin uint64, revisions []PayloadInputWitnessRevision) *PayloadInputWitness {
	return storage.NewPayloadInputWitness(source, admin, revisions)
}

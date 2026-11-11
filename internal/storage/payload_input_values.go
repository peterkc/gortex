package storage

import "slices"

// PayloadInputWitness binds selected ancestor rows to one live store core.
// It is process-local proof, not persisted cache authority.
type PayloadInputWitness struct {
	source    uint64
	admin     uint64
	revisions []PayloadInputWitnessRevision
}

// PayloadInputWitnessRevision is one opaque captured generation row.
type PayloadInputWitnessRevision struct {
	generation                    int64
	analysis, constants, bindings uint64
	state                         string
}

// PayloadInputRevision is an opaque comparable revision of one selected handle.
// It describes cached input ownership, not catalog lifetime or publication authority.
type PayloadInputRevision struct {
	source                 uint64
	generation             int64
	analysis, input, admin uint64
}

// NewPayloadInputRevision constructs an opaque value using a backend-private
// source identity. It performs no I/O and grants no publication authority.
func NewPayloadInputRevision(source uint64, generation int64, analysis, input, admin uint64) PayloadInputRevision {
	return PayloadInputRevision{source: source, generation: generation, analysis: analysis, input: input, admin: admin}
}

// NewPayloadInputWitnessRevision constructs one immutable captured row.
func NewPayloadInputWitnessRevision(generation int64, analysis, constants, bindings uint64, state string) PayloadInputWitnessRevision {
	return PayloadInputWitnessRevision{generation: generation, analysis: analysis, constants: constants, bindings: bindings, state: state}
}

// NewPayloadInputWitness copies its rows and retains only opaque process-local
// identity and captured values. Backend validation remains publication authority.
func NewPayloadInputWitness(source uint64, admin uint64, revisions []PayloadInputWitnessRevision) *PayloadInputWitness {
	return &PayloadInputWitness{source: source, admin: admin, revisions: slices.Clone(revisions)}
}

// MatchesSource reports whether the witness belongs to the backend's identity.
func (w *PayloadInputWitness) MatchesSource(source uint64) bool { return w.source == source }

// AdminRevision is the captured administration revision.
func (w *PayloadInputWitness) AdminRevision() uint64 { return w.admin }

// RevisionCount is the number of captured generation rows.
func (w *PayloadInputWitness) RevisionCount() int { return len(w.revisions) }

// Revision returns a captured row by value, without exposing the proof's slice.
func (w *PayloadInputWitness) Revision(index int) PayloadInputWitnessRevision {
	return w.revisions[index]
}

// GenerationID is the captured generation's identifier.
func (r PayloadInputWitnessRevision) GenerationID() int64 { return r.generation }

// Values returns the captured generation, clocks and state by value.
func (r PayloadInputWitnessRevision) Values() (generation int64, analysis, constants, bindings uint64, state string) {
	return r.generation, r.analysis, r.constants, r.bindings, r.state
}

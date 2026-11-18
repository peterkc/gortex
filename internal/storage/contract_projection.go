package storage

// ContractProjectionDomain names potential query results, including absence.
// It is not a receipt, an answer, or permission to acknowledge contract work.
// A consumer must define its complete dependency domains before using exclusion.
// Scope is literal, including the empty string; it never implies all scopes.
type ContractProjectionDomain struct {
	Kind, Scope, Key string
}

// ContractProjectionCursor contains owned scalar identity only. In particular it
// retains no store, reader, lease, transaction or context. Its unexported fields
// are carried through value helpers; captures and validation stay backend-owned.
type ContractProjectionCursor struct {
	instance, analysis, input, bindings, admin uint64
	generation                                 int64
}

// ContractProjectionDelta is current committed history, not publication proof.
// Complete=false requires the original physical read proof; Domains must never
// be used when incomplete. Publication still needs a locked input witness.
type ContractProjectionDelta struct {
	After    ContractProjectionCursor
	Complete bool
	Domains  []ContractProjectionDomain
}

// NewContractProjectionCursor preserves an owned physical clock tuple.
func NewContractProjectionCursor(instance, analysis, input, bindings, admin uint64, generation int64) ContractProjectionCursor {
	return ContractProjectionCursor{instance: instance, analysis: analysis, input: input, bindings: bindings, admin: admin, generation: generation}
}

// Values returns the cursor's owned scalar tuple without validation.
func (c ContractProjectionCursor) Values() (instance, analysis, input, bindings, admin uint64, generation int64) {
	return c.instance, c.analysis, c.input, c.bindings, c.admin, c.generation
}

package storage

import (
	"errors"
	"fmt"
)

// ErrCatalogStaleGuard means a guarded transition's expected precondition
// — an incarnation, a route epoch, a primary epoch, or a generation still
// being in the building state — no longer matched the stored row. Nothing
// was written.
var ErrCatalogStaleGuard = errors.New("store_sqlite: catalog guard mismatch, no rows changed")

// ErrCatalogNotFound means the addressed catalog row does not exist.
var ErrCatalogNotFound = errors.New("store_sqlite: catalog row not found")

// ErrCatalogGenerationReferenced means a view generation cannot be deleted
// because a route, a ref view, a dedicated graph, or another generation's
// base pointer still names it.
var ErrCatalogGenerationReferenced = errors.New("store_sqlite: view generation is still referenced")

// ErrCatalogInvalidValue means a write carried a value outside the state
// vocabulary, or left a required identifier empty.
var ErrCatalogInvalidValue = errors.New("store_sqlite: invalid catalog value")

// ErrCatalogObservationFenced is the refusal: a newer observation already
// holds the row, so nothing this one stated was written.
//
// It wraps ErrCatalogStaleGuard because that is what the outcome IS for a
// caller that does not distinguish — the write did not apply because the row
// moved on — and because both production observers already handle that error
// correctly for a refusal. A caller that wants the distinction tests this
// sentinel, or reads CheckoutObservationReport.Verdict.
var ErrCatalogObservationFenced = fmt.Errorf("%w: superseded by a newer observation", ErrCatalogStaleGuard)

// ErrCatalogGenerationRetiring means a proposed catalog reference targets a
// generation whose retirement fence has already committed.
var ErrCatalogGenerationRetiring = errors.New("store_sqlite: view generation is retiring")

// ErrCatalogGraphClosing refuses new publication or build admission while a
// dedicated graph's durable cleanup is pending.
var ErrCatalogGraphClosing = errors.New("store_sqlite: dedicated graph is closing")

// ErrPayloadGenerationSealed means a write was attempted through a handle
// whose generation is no longer building.
var ErrPayloadGenerationSealed = errors.New("store_sqlite: payload generation is published and no longer writable")

// ErrPayloadGenerationInUse means retirement was refused because a lease
// holder still reads the generation.
var ErrPayloadGenerationInUse = errors.New("store_sqlite: payload generation is leased")

// ErrPayloadGenerationIncomplete means a publish was refused because a
// producer has not finished contributing to the generation.
var ErrPayloadGenerationIncomplete = errors.New("store_sqlite: payload generation has a producer still building")

// ErrMasksAtBaseGeneration means a mask write was attempted through a
// handle pinned to the base generation, where a mask has no meaning.
var ErrMasksAtBaseGeneration = errors.New("store_sqlite: ownership masks require a derived view generation")

// ErrGenerationMaskInvalidValue means a mask write carried a value outside
// its vocabulary, or left a required identifier empty.
var ErrGenerationMaskInvalidValue = errors.New("store_sqlite: invalid generation mask value")

// ErrGenerationMaskIntegrity means a generation's masks contradict the
// payload it carries at the same generation.
var ErrGenerationMaskIntegrity = errors.New("store_sqlite: generation mask contradicts its payload")

// ErrGenerationBulkCheckpointBusy reports that a periodic checkpoint still
// owns the physical checkpoint path, so a generation build must retry instead
// of falling back to writes that compete with that checkpoint.
var ErrGenerationBulkCheckpointBusy = errors.New("store_sqlite: generation bulk checkpoint is still active")

// ErrPayloadSweepBudgetExhausted means a retirement sweep stopped on its own
// budget with work left to do. It is not a failure: the generation keeps its
// retiring fence, its payload rows are a strict subset of what they were, and
// the next pass continues from the step this one stopped on.
//
// Most callers resume it without needing to know that is what they are doing —
// the coordinator backlog, the lifecycle's owed set and the ref-view retention
// pass all offer a refused retirement again. Repository cleanup maps a budget
// yield to ErrRepositoryCleanupPending so an untrack can resume it too.
var ErrPayloadSweepBudgetExhausted = errors.New("store_sqlite: payload generation sweep yielded on its budget")

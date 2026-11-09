package storage

import (
	"errors"
	"fmt"
)

// ErrGenerationBulkLoadPopulated refuses a generation-scoped bulk window over
// a generation that already holds payload rows. The window's whole licence is
// that nothing can be reading or colliding with the rows it is about to write;
// a generation with rows has neither property, and a second window over one is
// the caller's bug, not a condition to work around.
var ErrGenerationBulkLoadPopulated = errors.New("store_sqlite: generation already holds payload rows")

var ErrDedicatedBaseCandidate = errors.New("invalid dedicated base candidate")

// ErrCatalogIntentTransitionActive means the checkout already has an
// in-flight intent transition; only one may exist at a time.
var ErrCatalogIntentTransitionActive = errors.New("store_sqlite: checkout already has an active intent transition")

// ErrRefViewBuildInFlight means another attempt is already building the
// same work — the same ref view, tree, base generation and fingerprint.
// It is the coalescing index reporting itself through ClaimRefViewBuild,
// and the caller's signal to wait on the attempt that is already running
// rather than start a second one that would produce the same payload.
var ErrRefViewBuildInFlight = errors.New("store_sqlite: a build for this ref view is already in flight")

// Manifest errors, distinguishable without matching on strings.
var (
	// ErrInputManifestAtBaseGeneration means a manifest was written or read
	// through a handle pinned to the base generation, which has no sample.
	ErrInputManifestAtBaseGeneration = errors.New("store_sqlite: input manifests require a derived view generation")

	// ErrInputManifestInvalid means a manifest write carried a value outside
	// its vocabulary, a non-canonical or duplicate path, or inconsistent meta.
	ErrInputManifestInvalid = errors.New("store_sqlite: invalid input manifest")

	// ErrInputManifestIncomplete means a stored manifest's entry rows disagree
	// with its meta row, so it must not be used as a planning input.
	ErrInputManifestIncomplete = errors.New("store_sqlite: input manifest entries disagree with its meta")
)

// ErrGenerationCopyYielded means the entire copy transaction has rolled back
// and released its writer. It is not cancellation of the caller's build.
var ErrGenerationCopyYielded = errors.New("store_sqlite: generation copy yielded to queued writer")

// ErrPayloadRetirementWriteWanted means a real writer preempted a retirement
// quantum. Its interrupted transaction rolled back and can be resumed.
var ErrPayloadRetirementWriteWanted = errors.New("store_sqlite: retirement quantum yielded to a waiting writer")

// ErrSchemaTooNew means this binary cannot safely interpret the stored schema.
// WithRebuild never overrides this refusal: the store also holds durable
// checkout metadata, and a newer schema is not an authorized rebuild target.
var ErrSchemaTooNew = errors.New("sqlite store schema is newer than this binary supports")

// SchemaTooNewError identifies the incompatible versions without suggesting
// deletion as a recovery step. Callers can match ErrSchemaTooNew with errors.Is.
type SchemaTooNewError struct {
	Stored    int
	Supported int
}

func (e *SchemaTooNewError) Error() string {
	return fmt.Sprintf("%v: stored v%d, supported v%d; database retained, use a compatible newer binary", ErrSchemaTooNew, e.Stored, e.Supported)
}

func (e *SchemaTooNewError) Unwrap() error { return ErrSchemaTooNew }

// StorageError identifies a failure reported by the persistent graph store.
// It is both returned by checked write capabilities and used as the payload of
// legacy Store-method panics. That distinction lets index/build boundaries
// recover storage failures without swallowing programmer or extractor panics.
type StorageError struct {
	Err error `json:"-"`
}

func (e *StorageError) Error() string {
	if e == nil || e.Err == nil {
		return "store_sqlite: storage operation failed"
	}
	return "store_sqlite: " + e.Err.Error()
}

func (e *StorageError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// StorageErrorFromPanic recognizes only panics emitted by the SQLite store.
// Arbitrary error-valued, runtime, parser, and programmer panics deliberately
// return ok=false and must continue propagating.
func StorageErrorFromPanic(recovered any) (err error, ok bool) {
	storageErr, ok := recovered.(*StorageError)
	if !ok || storageErr == nil {
		return nil, false
	}
	return storageErr, true
}

// ErrMaintenanceBusy reports that a whole-database maintenance action did not
// run because the store was busy: the lane was occupied, or a publish drain or
// payload build stayed in flight for the action's whole budget. It is a
// deferral, not a fault — the store is untouched and every caller treats it as
// skip-and-continue.
var ErrMaintenanceBusy = errors.New("store_sqlite: whole-database maintenance deferred: store busy")

// ErrInsufficientUpgradeSpace means Open refused a schema upgrade because
// the volume holding the store has less free space than the upgrade needs.
// The store is left exactly as it was.
var ErrInsufficientUpgradeSpace = errors.New("not enough free disk space to upgrade the sqlite store schema")

// InsufficientUpgradeSpaceError names the store, the transition, and both
// byte counts. Callers can match ErrInsufficientUpgradeSpace with errors.Is.
type InsufficientUpgradeSpaceError struct {
	Path      string
	From      int
	To        int
	Need      uint64
	Available uint64
}

func (e *InsufficientUpgradeSpaceError) Error() string {
	return fmt.Sprintf("%v: upgrading %s from v%d to v%d needs %d bytes (%s) free, %d bytes (%s) available; "+
		"the store was not modified — free disk space on that volume or move the store to a larger one, then start again",
		ErrInsufficientUpgradeSpace, e.Path, e.From, e.To, e.Need, formatMiB(e.Need), e.Available, formatMiB(e.Available))
}

func (e *InsufficientUpgradeSpaceError) Unwrap() error { return ErrInsufficientUpgradeSpace }

func formatMiB(n uint64) string {
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
}

// ErrVacuumTempDirectory means no VACUUM ran because its scratch volume could
// not be confirmed. Callers must skip instead of assuming the system temp
// volume has the headroom checked for the store directory.
var ErrVacuumTempDirectory = errors.New("sqlite vacuum: temporary directory unconfirmed")

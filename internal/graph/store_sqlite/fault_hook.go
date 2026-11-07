package store_sqlite

import "database/sql"

// Fault points: the durable boundaries at which a test can stop an operation.
//
// A store opened WithFaultHook calls the hook at each point below. When the
// hook returns an error, the operation returns that error at once: it runs no
// later step and none of the cleanup that a later step would own, so the files
// are left as a crash at that boundary would leave them. A test then opens a
// new instance over the same files and checks that it finds the old or the new
// complete state.
//
// The hook belongs to one opened instance and every handle derived from it. A
// store opened without the option has a nil hook, and every point is a no-op.

// FaultPoint names one durable boundary of this backend.
type FaultPoint string

const (
	// FaultPayloadWriteBeforeCommit and FaultPayloadWriteAfterCommit bracket
	// the commit of every payload write: each write transaction committed
	// through commitPayloadTx, commitPayloadSealTx or ownedWriteTx, and each
	// single statement run through execActiveWriteLocked.
	FaultPayloadWriteBeforeCommit FaultPoint = "payload_write_before_commit"
	FaultPayloadWriteAfterCommit  FaultPoint = "payload_write_after_commit"

	// FaultSealBeforeCommit and FaultSealAfterCommit bracket each commit that
	// moves a payload generation from building to ready: the payload
	// generation publish and a contract attachment publication.
	FaultSealBeforeCommit FaultPoint = "seal_before_commit"
	FaultSealAfterCommit  FaultPoint = "seal_after_commit"

	// FaultPublishBeforeCommit and FaultPublishAfterCommit bracket commits in
	// UpsertCheckoutRoute, FlipCheckoutRoute, FlipCheckoutRouteSlot and
	// AdoptRefViewGeneration that make a generation the one a checkout or
	// ref view serves. Route installation and full-route flips fire them when
	// they name a non-zero commit or dirty generation. A slot flip fires them
	// when it changes its slot and either slot names a generation afterward,
	// because the checkout serves the stack of both slots: a commit flip under
	// a dirty slot and a dirty-slot clear over a commit slot both publish.
	// A route that serves no generation afterward fires no publish pair.
	FaultPublishBeforeCommit FaultPoint = "publish_before_commit"
	FaultPublishAfterCommit  FaultPoint = "publish_after_commit"

	// FaultCatalogBeforeCommit and FaultCatalogAfterCommit bracket the commit
	// of every catalog transition: each multi-record transaction, the route
	// flip included, and each single-statement catalog write.
	FaultCatalogBeforeCommit FaultPoint = "catalog_before_commit"
	FaultCatalogAfterCommit  FaultPoint = "catalog_after_commit"
)

// FaultPoints returns every fault point this backend declares.
func FaultPoints() []FaultPoint {
	return []FaultPoint{
		FaultPayloadWriteBeforeCommit, FaultPayloadWriteAfterCommit,
		FaultSealBeforeCommit, FaultSealAfterCommit,
		FaultPublishBeforeCommit, FaultPublishAfterCommit,
		FaultCatalogBeforeCommit, FaultCatalogAfterCommit,
	}
}

// FaultHook is called at each fault point of the instance it was opened with.
// A non-nil result stops the operation at that point and is returned to its
// caller. The hook runs on the operation's goroutine, possibly under the
// store's write gate, so it must not call back into the store.
type FaultHook func(point FaultPoint) error

// WithFaultHook installs hook on the opened instance. Only tests pass it.
func WithFaultHook(hook FaultHook) Option {
	return func(o *openOptions) { o.faultHook = hook }
}

// commitPayloadTx commits one payload write transaction between the payload
// fault points. A stop before the commit rolls the transaction back, as a
// failed commit would.
func (c *storeCore) commitPayloadTx(tx *sql.Tx) error {
	if err := c.fault(FaultPayloadWriteBeforeCommit); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return c.fault(FaultPayloadWriteAfterCommit)
}

// commitPayloadSealTx is commitPayloadTx for a payload transaction that also
// seals a generation (moves it to ready) through a catalog transition, as a
// contract attachment publication does: the payload, seal and catalog point
// pairs all bracket its one commit.
func (c *storeCore) commitPayloadSealTx(tx *sql.Tx) error {
	for _, point := range []FaultPoint{FaultPayloadWriteBeforeCommit, FaultSealBeforeCommit, FaultCatalogBeforeCommit} {
		if err := c.fault(point); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, point := range []FaultPoint{FaultCatalogAfterCommit, FaultSealAfterCommit, FaultPayloadWriteAfterCommit} {
		if err := c.fault(point); err != nil {
			return err
		}
	}
	return nil
}

// fault runs the instance's hook at point. It returns nil when no hook is set.
func (c *storeCore) fault(point FaultPoint) error {
	if c == nil || c.faultHook == nil {
		return nil
	}
	return c.faultHook(point)
}

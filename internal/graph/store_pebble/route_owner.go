package store_pebble

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cockroachdb/pebble/v2"
)

// routeOwner is the private publication lifecycle for one checkout. The
// checkout's sole writer serializes its calls and pins any serving head until
// its readers drain. The live adapter must supply a private root dedicated to
// this checkout, not the checkout's source directory or a shared scratch root.
// Worker processes never receive this owner or its head handles.
type routeOwner struct {
	catalog     *routeCatalog
	checkout    string
	root        string
	incarnation string
	profile     Profile
}

type routeBuild struct {
	owner     *routeOwner
	locator   routeLocator
	previous  routeLocator
	prior     *Head
	head      *Head
	requested Witness
}

func (o *routeOwner) locator(lease string) routeLocator {
	return routeLocator{Lease: lease, Directory: filepath.Join(o.root, lease)}
}

func (o *routeOwner) owns(loc routeLocator) bool {
	return loc.valid() && loc == o.locator(loc.Lease)
}

func (o *routeOwner) lookup() (routeCheckout, error) {
	row, err := o.catalog.lookup(o.checkout)
	if err != nil {
		return routeCheckout{}, err
	}
	if row.Incarnation != o.incarnation {
		return routeCheckout{}, ErrStaleBase
	}
	for _, loc := range []routeLocator{row.Serving, row.Candidate, row.Retained.Locator} {
		if loc != (routeLocator{}) && !o.owns(loc) {
			return routeCheckout{}, invalid("route outside checkout head root")
		}
	}
	return row, nil
}

func completeRoute(w Witness) bool {
	return w.Stage == StageSearch && w.Relations != nil && w.Contracts != nil &&
		(w.Relations.IsolationChecked || len(w.Relations.DeferredLanguages) == 0)
}

// begin registers a fresh candidate before creating its directory. A nil
// build with no error means the verified serving head already has the requested
// complete version. In that case the caller must not start a worker.
func (o *routeOwner) begin(ctx context.Context, lease string, prior *Head, requested Witness) (*routeBuild, error) {
	row, err := o.lookup()
	if err != nil {
		return nil, err
	}
	if row.Candidate != (routeLocator{}) || requested.Profile != o.profile ||
		((prior == nil) != (row.Serving == (routeLocator{}))) ||
		(prior != nil && !headMatchesLocator(prior, o.incarnation, row.Serving)) {
		return nil, ErrStaleBase
	}
	if prior != nil {
		w, _, err := prior.ReconcilePublication(&row.Publication)
		if err != nil {
			return nil, err
		}
		if completeRoute(w) && w.Profile == requested.Profile && w.Version() == requested.Version() {
			return nil, nil
		}
	}
	loc := o.locator(lease)
	if !o.owns(loc) {
		return nil, invalid("candidate locator")
	}
	if _, err := os.Lstat(loc.Directory); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.Join(ErrStaleBase, err)
	}
	if err := o.catalog.registerCandidate(ctx, o.checkout, o.incarnation, row.Serving, loc); err != nil {
		return nil, err
	}
	h, err := OpenHead(loc.Directory, o.incarnation, o.profile)
	if err != nil {
		// The durable reservation remains for startup cleanup.
		return nil, err
	}
	return &routeBuild{owner: o, locator: loc, previous: row.Serving, prior: prior, head: h, requested: requested}, nil
}

// publishStage has the worker receiver's callback signature. A successful
// return permits ACK: the ingest is durable and, if serving, its exact local
// publication record is durable. A failed return never rolls back an ingest.
func (b *routeBuild) publishStage(ctx context.Context, tables StageTables) error {
	o := b.owner
	row, err := o.lookup()
	if err != nil {
		return err
	}
	selected := row.Serving == b.locator && row.Candidate == (routeLocator{})
	if !selected && (row.Candidate != b.locator || row.Serving != b.previous) {
		return ErrStaleBase
	}
	if tables.Witness.Version() != b.requested.Version() || tables.Witness.FrozenInputs != b.requested.FrozenInputs {
		return ErrStaleBase
	}
	if tables.Witness.Stage.rank() >= StageContracts.rank() && tables.Witness.Contracts == nil {
		return ErrStageOrder
	}
	digest, err := b.head.PublishStage(ctx, tables)
	if err != nil {
		return err
	}
	w, receipt, err := b.head.ReconcilePublication(nil)
	if err != nil {
		return err
	}
	if receipt.WitnessDigest != digest {
		return ErrStaleBase
	}
	if selected {
		return o.recordServing(ctx, b.locator, receipt)
	}
	// CAS compares the live prior stage and coverage, not the catalog receipt.
	// A shallower candidate can be durably accepted without becoming visible.
	err = o.catalog.switchHead(ctx, o.checkout, o.incarnation, b.previous, b.locator, b.prior, b.head, receipt)
	if w.Stage != StageSearch && (errors.Is(err, ErrStageOrder) || errors.Is(err, ErrCoverageRegression)) {
		return nil
	}
	return err
}

// publishDelta is the corresponding edit owner boundary. The caller has
// already hashed/compared its inputs and prepared the delta against this head.
func (o *routeOwner) publishDelta(ctx context.Context, head *Head, tables StageTables) error {
	row, err := o.lookup()
	if err != nil {
		return err
	}
	if row.Candidate != (routeLocator{}) || !headMatchesLocator(head, o.incarnation, row.Serving) {
		return ErrStaleBase
	}
	digest, err := head.PublishDelta(ctx, tables)
	if err != nil {
		return err
	}
	_, receipt, err := head.ReconcilePublication(nil)
	if err != nil {
		return err
	}
	if digest != receipt.WitnessDigest {
		return ErrStaleBase
	}
	return o.recordServing(ctx, row.Serving, receipt)
}

func (o *routeOwner) recordServing(ctx context.Context, loc routeLocator, receipt LocalPublicationRecord) error {
	o.catalog.mu.Lock()
	defer o.catalog.mu.Unlock()
	row, err := o.lookup()
	if err != nil {
		return err
	}
	if row.Serving != loc || receipt.Incarnation != row.Incarnation {
		return ErrStaleBase
	}
	row.Publication, row.LastGood = receipt, false
	return o.catalog.save(ctx, o.checkout, row)
}

// failBuild runs after the worker has exited and been reaped. It preserves
// the error for the scheduler/status adapter and leaves the serving head open.
// The caller must drain readers before failing a detached former serving head.
// Any worker spill directory is owned separately by the worker lease caller.
func (b *routeBuild) failBuild(ctx context.Context, failure error) error {
	o := b.owner
	o.catalog.mu.Lock()
	defer o.catalog.mu.Unlock()
	row, err := o.lookup()
	if err != nil {
		return errors.Join(failure, err)
	}
	if row.Serving == b.locator {
		return failure
	}
	var closeErr error
	if b.head != nil {
		closeErr = b.head.Close()
		b.head = nil
	}
	if row.Candidate != b.locator {
		return errors.Join(failure, ErrStaleBase, closeErr)
	}
	if closeErr != nil {
		return errors.Join(failure, closeErr)
	}
	// Reaping the worker ends its work, not the owner's cleanup obligation.
	return errors.Join(failure, o.discardCandidate(context.WithoutCancel(ctx), row))
}

// discardCandidate removes only the reserved, unserved directory, then clears
// its reservation. A crash between those operations can repeat the cleanup.
// The catalog lock is held and the caller has closed all candidate handles.
func (o *routeOwner) discardCandidate(ctx context.Context, row routeCheckout) error {
	if row.Candidate == (routeLocator{}) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.RemoveAll(row.Candidate.Directory); err != nil {
		return err
	}
	row.Candidate = routeLocator{}
	return o.catalog.save(ctx, o.checkout, row)
}

// retainCheckpoint records a closed, exact checkpoint of the serving head
// only after the checkpoint operation succeeds. This private seam retains one
// last-good checkpoint; rotation and lease GC belong to the live adapter.
func (o *routeOwner) retainCheckpoint(ctx context.Context, head *Head, lease string) error {
	o.catalog.mu.Lock()
	defer o.catalog.mu.Unlock()
	row, err := o.lookup()
	if err != nil {
		return err
	}
	loc := o.locator(lease)
	if !o.owns(loc) || row.Retained != (routeCheckpoint{}) || loc.conflicts(row.Serving) || loc.conflicts(row.Candidate) ||
		!headMatchesLocator(head, o.incarnation, row.Serving) {
		return ErrStaleBase
	}
	w, record, err := head.ReconcilePublication(&row.Publication)
	if err != nil {
		return err
	}
	if !completeRoute(w) || w.Profile != o.profile {
		return ErrStageOrder
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := head.Checkpoint(loc.Directory); err != nil {
		return err
	}
	row.Retained = routeCheckpoint{Locator: loc, Publication: record}
	return o.catalog.save(ctx, o.checkout, row)
}

// routeRecovery reports facts for the scheduler, not a second queue. The
// caller still hashes the input diff, and resumes only from checked entries.
// A LastGood route is not a fresh version. Failure describes the rejected
// selected head in this recovery attempt, when a checkpoint was restored.
type routeRecovery struct {
	Head        *Head
	Witness     Witness
	Publication LocalPublicationRecord
	Requeue     bool
	LastGood    bool
	Failure     error
}

// recover runs before new readers or writers. validate must run the owner's
// startup integrity check on this exact snapshot. A merely decodable witness
// is insufficient. An integrity failure must wrap errInvalidRecord or
// ErrNotPublished to authorize fallback; return transient errors unchanged.
// recover does not return a usable head on a failed check.
func (o *routeOwner) recover(ctx context.Context, validate func(context.Context, *Snapshot, Witness) error) (routeRecovery, error) {
	o.catalog.mu.Lock()
	defer o.catalog.mu.Unlock()
	row, err := o.lookup()
	if err != nil {
		return routeRecovery{}, err
	}
	if validate == nil {
		return routeRecovery{}, invalid("startup integrity check required")
	}
	if err := o.discardCandidate(ctx, row); err != nil {
		return routeRecovery{}, err
	}
	row.Candidate = routeLocator{}
	if row.Serving == (routeLocator{}) {
		return routeRecovery{Requeue: true}, nil
	}
	r, badHead, err := o.openChecked(ctx, row.Serving, row.Publication, validate)
	if err != nil {
		if !badHead || row.Retained.Locator == (routeLocator{}) || ctx.Err() != nil {
			return routeRecovery{}, err
		}
		fallback, _, fallbackErr := o.openChecked(ctx, row.Retained.Locator, row.Retained.Publication, validate)
		if fallbackErr != nil {
			return routeRecovery{}, errors.Join(err, fallbackErr)
		}
		if !completeRoute(fallback.Witness) || fallback.Publication.WitnessDigest != row.Retained.Publication.WitnessDigest ||
			fallback.Publication.Version != row.Retained.Publication.Version {
			return routeRecovery{}, errors.Join(err, ErrStaleBase, fallback.Head.Close())
		}
		fallback.Publication.EventID = nil
		row.Serving, row.Publication, row.Retained = row.Retained.Locator, fallback.Publication, routeCheckpoint{}
		row.LastGood = true
		if saveErr := o.catalog.save(ctx, o.checkout, row); saveErr != nil {
			return routeRecovery{}, errors.Join(err, saveErr, fallback.Head.Close())
		}
		fallback.Requeue, fallback.LastGood, fallback.Failure = true, true, err
		return fallback, nil
	}
	// The record may lag a committed stage or edit. ReconcilePublication
	// drops an event mark in that case; write the corrected record before
	// returning the selected head. Do not rewrite an unchanged record.
	recordChanged := row.Publication.Version != r.Publication.Version || row.Publication.WitnessDigest != r.Publication.WitnessDigest
	if recordChanged {
		row.Publication = r.Publication
		if err := o.catalog.save(ctx, o.checkout, row); err != nil {
			return routeRecovery{}, errors.Join(err, r.Head.Close())
		}
	}
	r.LastGood = row.LastGood
	// A valid watcher mark narrows the input diff; it does not prove there is
	// no edit to replay after a crash before ingest.
	r.Requeue = true
	return r, nil
}

// openChecked reports whether a failed check proves the selected head is bad.
func (o *routeOwner) openChecked(ctx context.Context, loc routeLocator, saved LocalPublicationRecord, validate func(context.Context, *Snapshot, Witness) error) (routeRecovery, bool, error) {
	if err := ctx.Err(); err != nil {
		return routeRecovery{}, false, err
	}
	info, err := os.Lstat(loc.Directory)
	if err != nil {
		return routeRecovery{}, errors.Is(err, os.ErrNotExist), err
	}
	if !info.IsDir() {
		return routeRecovery{}, true, invalid("route head directory")
	}
	options := headOptions()
	options.ErrorIfNotExists = true
	db, err := pebble.Open(loc.Directory, options)
	if err != nil {
		return routeRecovery{}, errors.Is(err, pebble.ErrDBDoesNotExist) || pebble.IsCorruptionError(err), err
	}
	h := &Head{db: db, options: options, incarnation: o.incarnation, profile: o.profile, directory: loc.Directory}
	snap := h.OpenSnapshot()
	w, record, err := snap.reconcilePublication(o.incarnation, &saved)
	if err == nil && w.Profile != o.profile {
		err = invalid("startup head profile")
	}
	if err == nil {
		err = snap.verifyIsolationCoverage(ctx, w, nil)
	}
	if err == nil {
		err = validate(ctx, snap, w)
	}
	readErr, closeErr := snap.ReadError(), snap.Close()
	if err != nil || readErr != nil || closeErr != nil {
		// A validator can report an apparent mismatch after a failed read.
		// An unclassified read or close failure cannot authorize a switch.
		badHead := (err == nil || badRouteRecord(err)) && (readErr == nil || badRouteRecord(readErr))
		closeErr = errors.Join(closeErr, h.Close())
		return routeRecovery{}, badHead && closeErr == nil, fmt.Errorf("check route head %q: %w", loc.Lease, errors.Join(err, readErr, closeErr))
	}
	return routeRecovery{Head: h, Witness: w, Publication: record}, false, nil
}

func badRouteRecord(err error) bool {
	return errors.Is(err, errInvalidRecord) || errors.Is(err, ErrNotPublished) || pebble.IsCorruptionError(err)
}

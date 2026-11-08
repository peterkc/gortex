package storetest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

// Additional lifecycle cases use the same operations and fixtures as K1-K14.
// testK2PublishRequiresSeal publishes a written but unsealed build with a
// fresh guard: P accepts only a sealed candidate, so it refuses and nothing
// changes.
func testK2PublishRequiresSeal(t *testing.T, backend Backend) {
	ctx, f, inst := newK1Chain(t, backend)
	publishVersion(t, ctx, inst, f, CheckoutR1, E0)
	build, err := inst.Begin(ctx, BuildRequest{Checkout: CheckoutR1, Inputs: f.Inputs(CheckoutR1, E1), Operation: "k2-unsealed"})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := build.Write(ctx, f.Payload("r", E1)); err != nil {
		t.Fatalf("write: %v", err)
	}
	before, inv := current(t, ctx, inst, CheckoutR1), inventory(t, ctx, inst)
	if err := build.Publish(ctx, before, "k2-unsealed-publish"); err == nil {
		t.Fatal("D18-K2-PublishRequiresSealedCandidate: a publish of an unsealed build succeeded")
	}
	assertUnchanged(t, ctx, inst, CheckoutR1, before, inv, "the refused publish of an unsealed build")
	assertServesF(t, ctx, openView(t, ctx, inst, CheckoutR1), f, CheckoutR1, E0)
}

// testK2ExpiredOwner begins a build on one instance, then recovers the same
// files in a new instance (R). The first instance's claim is dead: writes,
// seal and publish through its handle refuse, and the recovered instance
// sees no change.
func testK2ExpiredOwner(t *testing.T, backend Backend) {
	ctx := caseContext(t)
	f := NewFixtureF(t)
	dir := t.TempDir()
	old := newChainIn(t, ctx, backend, dir, nil, f, CheckoutR1)
	publishVersion(t, ctx, old, f, CheckoutR1, E0)
	build, err := old.Begin(ctx, BuildRequest{Checkout: CheckoutR1, Inputs: f.Inputs(CheckoutR1, E1), Operation: "k2-expired"})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := build.Write(ctx, f.Payload("r", E1)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close the expired owner: %v", err)
	}
	recovered := backend.Open(t, dir, nil)
	before, inv := current(t, ctx, recovered, CheckoutR1), inventory(t, ctx, recovered)
	if err := build.Write(ctx, extraPayload("r")); err == nil {
		t.Error("a write through the expired handle succeeded")
	}
	if err := build.Seal(ctx); err == nil {
		t.Error("a seal through the expired handle succeeded")
	}
	if err := build.Publish(ctx, before, "k2-expired-publish"); err == nil {
		t.Error("a publish through the expired handle succeeded")
	}
	assertUnchanged(t, ctx, recovered, CheckoutR1, before, inv, "the writes through the expired handle")
	assertServesF(t, ctx, openView(t, ctx, recovered, CheckoutR1), f, CheckoutR1, E0)
}

// --- K3 (lifecycle_k3.go) ---

// testK3FailedWriteRefusesSeal stops the E1 write at its first fault point on
// a live instance: the attempt is incomplete, so S and P refuse and E0 stays.
func testK3FailedWriteRefusesSeal(t *testing.T, backend Backend) {
	ctx, f, inst, s, build, base := k3Building(t, backend)
	dry := &stopper{}
	_, _, dryBuild, _ := k3BuildingWith(t, backend, f, dry)
	dry.arm(stop{})
	if err := dryBuild.Write(ctx, f.Payload("r", E1)); err != nil {
		t.Fatalf("the unstopped write: %v", err)
	}
	arrivals, _ := dry.disarm()
	first := stopsOf(t, inst.Declaration().FaultPoints, arrivals)[0]
	s.arm(first)
	err := build.Write(ctx, f.Payload("r", E1))
	if _, stopped := s.disarm(); !stopped || err == nil {
		t.Fatalf("the write did not stop at %s (error %v)", first, err)
	}
	k3AssertIncompleteRefuses(t, ctx, inst, f, build, base, "a write stopped at "+first.String(), "D18-K3-FailedWriteRefusesSealAndPublish")
}

// testK3CancelledWriteRefusesSeal writes E1 with a cancelled context: the
// write fails, so S and P refuse and E0 stays.
func testK3CancelledWriteRefusesSeal(t *testing.T, backend Backend) {
	ctx, f, inst, _, build, base := k3Building(t, backend)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := build.Write(cancelled, f.Payload("r", E1)); err == nil {
		t.Fatal("a write with a cancelled context succeeded")
	}
	k3AssertIncompleteRefuses(t, ctx, inst, f, build, base, "a cancelled write", "D18-K3-CancelledWriteRefusesSealAndPublish")
}

func k3Building(t *testing.T, backend Backend) (context.Context, *FixtureF, Instance, *stopper, Build, Publication) {
	t.Helper()
	f := NewFixtureF(t)
	s := &stopper{}
	ctx, inst, build, base := k3BuildingWith(t, backend, f, s)
	return ctx, f, inst, s, build, base
}

// k3BuildingWith publishes E0 and begins the E1 build, unarmed.
func k3BuildingWith(t *testing.T, backend Backend, f *FixtureF, s *stopper) (context.Context, Instance, Build, Publication) {
	t.Helper()
	ctx := caseContext(t)
	inst := newChainIn(t, ctx, backend, t.TempDir(), s.hook, f, CheckoutR1)
	publishVersion(t, ctx, inst, f, CheckoutR1, E0)
	build, err := inst.Begin(ctx, BuildRequest{Checkout: CheckoutR1, Inputs: f.Inputs(CheckoutR1, E1), Operation: "k3-incomplete"})
	if err != nil {
		t.Fatalf("begin E1: %v", err)
	}
	return ctx, inst, build, current(t, ctx, inst, CheckoutR1)
}

// k3AssertIncompleteRefuses checks that S and P of an incomplete attempt
// refuse, E0 stays published and complete, and the attempt is not sealed.
func k3AssertIncompleteRefuses(t *testing.T, ctx context.Context, inst Instance, f *FixtureF, build Build, base Publication, what, marker string) {
	t.Helper()
	sealErr := build.Seal(ctx)
	publishErr := build.Publish(ctx, base, "k3-incomplete-publish")
	if sealErr == nil || publishErr == nil {
		t.Fatalf("%s: after %s: seal returned %v and publish returned %v; both must refuse", marker, what, sealErr, publishErr)
	}
	if got := current(t, ctx, inst, CheckoutR1); got != base {
		t.Fatalf("after %s the publication moved from %+v to %+v", what, base, got)
	}
	if state := inventory(t, ctx, inst)[build.Version()]; state == StateSealed {
		t.Errorf("after %s the attempt %q is sealed", what, build.Version())
	}
	assertServesF(t, ctx, openView(t, ctx, inst, CheckoutR1), f, CheckoutR1, E0)
}

// --- K4: the whole case ---

// K4 Lost response/repeat (design-c.md 1.8 and 1.4). Given durable P with no
// returned reply, when the same operation ID is inspected or repeated, then
// its witness returns E1 without another publication. Wrong inputs and
// unknown pruned IDs refuse replay.
//
// FR-5 binding: the case repeats the publish by its operation ID. A publish
// is durable without a reply when it stops at a declared fault point and a
// new instance over the same files serves the candidate; the case finds those
// stops instead of naming a backend's point, then reruns each one.

const k4Operation = "k4-publish"

// testK4LostReply runs the replay checks for every stop that leaves P
// durable without a reply.
func testK4LostReply(t *testing.T, backend Backend) {
	f := NewFixtureF(t)
	dry := &stopper{}
	k4Publish(t, backend, f, dry, stop{})
	arrivals, _ := dry.disarm()
	declared := backend.Open(t, t.TempDir(), nil).Declaration().FaultPoints
	durable := 0
	for _, at := range stopsOf(t, declared, arrivals) {
		s := &stopper{}
		probe := k4Publish(t, backend, f, s, at)
		if err := probe.inst.Crash(); err != nil {
			t.Fatalf("end the lost-reply probe: %v", err)
		}
		s.disarm()
		if current(t, caseContext(t), backend.Open(t, probe.dir, nil), CheckoutR1).Version != probe.build.Version() {
			continue
		}
		durable++
		t.Run(at.String(), func(t *testing.T) { k4AssertReplay(t, backend, f, at) })
	}
	if durable == 0 {
		t.Fatalf("no stop of the publish left it durable without a reply (stops %v)", stopsOf(t, declared, arrivals))
	}
}

// k4Lost is one publish stopped at a fault point.
type k4Lost struct {
	dir   string
	inst  Instance
	build Build
	base  Publication
}

// k4Publish publishes E0 and seals the E1 candidate unarmed, then publishes
// the candidate armed at at.
func k4Publish(t *testing.T, backend Backend, f *FixtureF, s *stopper, at stop) k4Lost {
	t.Helper()
	ctx := caseContext(t)
	dir := t.TempDir()
	inst := newChainIn(t, ctx, backend, dir, s.hook, f, CheckoutR1)
	publishVersion(t, ctx, inst, f, CheckoutR1, E0)
	build := sealedBuild(t, ctx, inst, CheckoutR1, f.Inputs(CheckoutR1, E1), "k4-build", f.Payload("r", E1))
	base := current(t, ctx, inst, CheckoutR1)
	s.arm(at)
	err := build.Publish(ctx, base, k4Operation)
	if at.point == "" && err != nil {
		t.Fatalf("the unstopped publish: %v", err)
	}
	if at.point != "" && !errors.Is(err, errStopped) {
		t.Fatalf("the publish did not stop at %s (error %v)", at, err)
	}
	return k4Lost{dir: dir, inst: inst, build: build, base: base}
}

// k4AssertReplay loses the reply at at, then inspects and repeats the
// operation on the live instance and on a recovered one, and tries wrong
// inputs and an unknown ID. None of them publishes again.
func k4AssertReplay(t *testing.T, backend Backend, f *FixtureF, at stop) {
	s := &stopper{}
	lost := k4Publish(t, backend, f, s, at)
	s.disarm()
	ctx := caseContext(t)
	candidate := lost.build.Version()
	published := current(t, ctx, lost.inst, CheckoutR1)
	inv := inventory(t, ctx, lost.inst)

	if err := lost.build.Publish(ctx, lost.base, k4Operation); err != nil {
		t.Fatalf("D18-K4-LostReplyReturnsRecordedResult: repeat %q with its original guard: %v; want its recorded result", k4Operation, err)
	}
	if got, err := lost.inst.Operation(ctx, k4Operation); err != nil || got != candidate {
		t.Errorf("inspect %q on the live instance = %q, %v; want the candidate %q", k4Operation, got, err, candidate)
	}
	assertUnchanged(t, ctx, lost.inst, CheckoutR1, published, inv, "repeating the completed operation")

	if err := lost.inst.Crash(); err != nil {
		t.Fatalf("end the live replay owner before recovery: %v", err)
	}
	recovered := backend.Open(t, lost.dir, nil)
	if got, err := recovered.Operation(ctx, k4Operation); err != nil || got != candidate {
		t.Errorf("inspect %q after recovery = %q, %v; want the candidate %q", k4Operation, got, err, candidate)
	}
	other := sealedBuild(t, ctx, recovered, CheckoutR1, altInputs(f, CheckoutR1, E1), "k4-other", f.Payload("r", E1))
	inv = inventory(t, ctx, recovered)
	if err := other.Publish(ctx, published, k4Operation); err == nil {
		t.Error("replaying the operation ID with another build's inputs and a fresh guard succeeded")
	}
	if got, err := recovered.Operation(ctx, "k4-never-recorded"); err == nil {
		t.Errorf("inspect an unknown operation ID = %q; want a refusal", got)
	}
	assertUnchanged(t, ctx, recovered, CheckoutR1, published, inv, "the refused replays")
	assertServesF(t, ctx, openView(t, ctx, recovered, CheckoutR1), f, CheckoutR1, E1)
}

// --- K5 (lifecycle_k5.go) ---

// testK5SealedOrphanAdopted begins the sealed orphan's inputs on the
// recovered instance: it adopts the orphan, which publishes as complete E1
// without another write or seal.
func testK5SealedOrphanAdopted(t *testing.T, backend Backend) {
	ctx, f, inst, _, sealed, _ := k5Orphans(t, backend)
	build, err := inst.Begin(ctx, BuildRequest{Checkout: CheckoutR1, Inputs: f.Inputs(CheckoutR1, E1), Operation: "k5-adopt"})
	if err != nil {
		t.Fatalf("adopt the sealed orphan: %v", err)
	}
	if build.Version() != sealed {
		t.Fatalf("D18-K5-SealedOrphanIsAdopted: a begin with the sealed orphan's inputs returned %q, want the orphan %q", build.Version(), sealed)
	}
	if err := build.Publish(ctx, current(t, ctx, inst, CheckoutR1), "k5-adopt-publish"); err != nil {
		t.Fatalf("publish the adopted orphan: %v", err)
	}
	assertServesF(t, ctx, openView(t, ctx, inst, CheckoutR1), f, CheckoutR1, E1)
}

// testK5UnsealedOrphanNotAdopted begins the unsealed orphan's inputs on the
// recovered instance: only sealed work is adopted, so the build is new.
func testK5UnsealedOrphanNotAdopted(t *testing.T, backend Backend) {
	ctx, f, inst, _, _, unsealed := k5Orphans(t, backend)
	build, err := inst.Begin(ctx, BuildRequest{Checkout: CheckoutR1, Inputs: altInputs(f, CheckoutR1, E1), Operation: "k5-retry"})
	if err != nil {
		t.Fatalf("begin the unsealed orphan's inputs: %v", err)
	}
	if build.Version() == unsealed {
		t.Fatalf("D18-K5-UnsealedOrphanIsNotAdopted: a begin after recovery adopted the unsealed orphan %q", unsealed)
	}
}

// testK5LiveOwnerRefusesAdoption begins the same inputs under another
// operation while the first owner is live: adoption never transfers a live
// writer's attempt, so it refuses.
func testK5LiveOwnerRefusesAdoption(t *testing.T, backend Backend) {
	ctx, f, inst := newK1Chain(t, backend)
	publishVersion(t, ctx, inst, f, CheckoutR1, E0)
	owner, err := inst.Begin(ctx, BuildRequest{Checkout: CheckoutR1, Inputs: f.Inputs(CheckoutR1, E1), Operation: "k5-owner"})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := owner.Write(ctx, f.Payload("r", E1)); err != nil {
		t.Fatalf("write: %v", err)
	}
	other, err := inst.Begin(ctx, BuildRequest{Checkout: CheckoutR1, Inputs: f.Inputs(CheckoutR1, E1), Operation: "k5-other"})
	if err == nil {
		t.Fatalf("D18-K5-LiveOwnerRefusesAdoption: another operation took over the live owner's attempt (version %q, owner's %q)", other.Version(), owner.Version())
	}
	again, err := inst.Begin(ctx, BuildRequest{Checkout: CheckoutR1, Inputs: f.Inputs(CheckoutR1, E1), Operation: "k5-owner"})
	if err != nil || again.Version() != owner.Version() {
		t.Fatalf("repeating the owner's operation = %v, %v; want its attempt %q", again, err, owner.Version())
	}
}

// --- K6 (lifecycle_k6.go) ---

// K6 selection race: O needs a stop between its selection and its pin, and T
// one between its check and its fence. A backend declares them as these fault
// points; the race's hook blocks there instead of failing.
const (
	pointOpenSelected   = "open_selected"
	pointRetireSelected = "retire_selected"
)

// testK6RetireKeepsPreviousPublication retires the previous publication: it
// refuses and stays sealed.
func testK6RetireKeepsPreviousPublication(t *testing.T, backend Backend) {
	ctx, f, inst := newK1Chain(t, backend)
	e0 := publishVersion(t, ctx, inst, f, CheckoutR1, E0)
	publishVersion(t, ctx, inst, f, CheckoutR1, E1)
	if err := inst.Retire(ctx, e0); err == nil {
		t.Fatalf("D18-K6-RetireKeepsPreviousPublication: retiring the previous publication %q succeeded", e0)
	}
	if state := inventory(t, ctx, inst)[e0]; state != StateSealed {
		t.Errorf("the previous publication %q is %q after the refused retirement, want %q", e0, state, StateSealed)
	}
	assertServesF(t, ctx, openView(t, ctx, inst, CheckoutR1), f, CheckoutR1, E1)
}

// testK6SelectionRace parks O on E0 at its selection and T on E0 at its
// check, then releases them in both orders.
func testK6SelectionRace(t *testing.T, backend Backend) {
	declared := backend.Open(t, t.TempDir(), nil).Declaration().FaultPoints
	for _, point := range []string{pointOpenSelected, pointRetireSelected} {
		if !slices.Contains(declared, point) {
			t.Fatalf("D18-K6-SelectionRace: the race needs the %q stop, which the backend does not declare (it declares %v)", point, declared)
		}
	}
	for _, openFirst := range []bool{true, false} {
		name := "TFirst"
		if openFirst {
			name = "OFirst"
		}
		t.Run(name, func(t *testing.T) { k6Race(t, backend, openFirst) })
	}
}

func k6Race(t *testing.T, backend Backend, openFirst bool) {
	ctx := caseContext(t)
	f := NewFixtureF(t)
	parked := map[string]chan struct{}{pointOpenSelected: make(chan struct{}), pointRetireSelected: make(chan struct{})}
	release := map[string]chan struct{}{pointOpenSelected: make(chan struct{}), pointRetireSelected: make(chan struct{})}
	armed := make(chan struct{})
	once := map[string]*sync.Once{pointOpenSelected: {}, pointRetireSelected: {}}
	hook := func(point string) error {
		select {
		case <-armed:
		default:
			return nil
		}
		if p, ok := parked[point]; ok {
			once[point].Do(func() { close(p) })
			select {
			case <-release[point]:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	inst := newChainIn(t, ctx, backend, t.TempDir(), hook, f, CheckoutR1)
	e0 := publishVersion(t, ctx, inst, f, CheckoutR1, E0)
	close(armed)
	type opened struct {
		view View
		err  error
	}
	openDone, retireDone := make(chan opened, 1), make(chan error, 1)
	go func() { v, err := inst.Open(ctx, CheckoutR1); openDone <- opened{v, err} }()
	waitParked(t, ctx, parked[pointOpenSelected], "O")
	k6PublishPast(t, ctx, inst, f)
	go func() { retireDone <- inst.Retire(ctx, e0) }()
	waitParked(t, ctx, parked[pointRetireSelected], "T")

	first, second := pointRetireSelected, pointOpenSelected
	if openFirst {
		first, second = second, first
	}
	close(release[first])
	var o opened
	var retireErr error
	if openFirst {
		o = <-openDone
	} else {
		retireErr = <-retireDone
	}
	close(release[second])
	if openFirst {
		retireErr = <-retireDone
	} else {
		o = <-openDone
	}
	_, kept := inventory(t, ctx, inst)[e0]
	var openedVersion Version
	if o.err == nil {
		t.Cleanup(o.view.Release)
		openedVersion = o.view.Version()
	}
	if err := k6RaceProblem(openFirst, e0, openedVersion, o.err, retireErr, kept); err != nil {
		t.Fatal(err)
	}
	if o.err == nil {
		assertServesF(t, ctx, o.view, f, CheckoutR1, E0)
	}
}

func waitParked(t *testing.T, ctx context.Context, parked <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-parked:
	case <-ctx.Done():
		t.Fatalf("%s never reached its selection stop: %v", what, ctx.Err())
	}
}

// --- K9: catalog and route recovery ---

// K9 Catalog/route recovery (design-c.md 1.8, 1.6 and 5.1). Given
// track/link/untrack transitions over R and S, interrupt every C boundary. R
// restores the complete old or new guarded set, including index-state
// provenance and cleanup. A stale multi-record guard leaves all records
// unchanged. Missing active payload invokes whole-vector fallback or visible
// unavailability, not "ready" catalog rows over absent bytes.
//
// FR-5 binding: the transitions stop at every arrival at every declared fault
// point, then a new instance recovers the files.

// testK9MissingPayloadIsVisible removes the payload of R1's active E0 and
// keeps its catalog record. With no other complete version to fall back to,
// opening R1 must refuse; it must not serve the empty version as ready.
func testK9MissingPayloadIsVisible(t *testing.T, backend Backend) {
	ctx := caseContext(t)
	f := NewFixtureF(t)
	dir := t.TempDir()
	inst := newChainIn(t, ctx, backend, dir, nil, f, CheckoutR1)
	e0 := publishVersion(t, ctx, inst, f, CheckoutR1, E0)
	if err := inst.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	backend.Damage(t, dir, DamageMissingPayload, e0)
	recovered := backend.Open(t, dir, nil)
	view, err := recovered.Open(ctx, CheckoutR1)
	if err == nil {
		t.Cleanup(view.Release)
		t.Fatalf("D18-K9-MissingActivePayloadIsVisible: R1 opened version %q with %d nodes after its payload was removed; want a visible refusal",
			view.Version(), len(view.Reader().AllNodes()))
	}
}

// k9Tracked tracks R1, R2 and S1 and publishes E0 for each.
func k9Tracked(t *testing.T, backend Backend, hook FaultHook) (string, *FixtureF, Instance) {
	t.Helper()
	ctx := caseContext(t)
	f := NewFixtureF(t)
	dir := t.TempDir()
	inst := newChainIn(t, ctx, backend, dir, hook, f, CheckoutR1, CheckoutR2, CheckoutS1)
	for _, c := range f.Checkouts {
		publishVersion(t, ctx, inst, f, c.ID, E0)
	}
	return dir, f, inst
}

// testK9LinkIsGuarded links R to S's current version as one C transition.
// It needs the transition itself first: a backend without one refuses here.
func testK9LinkIsGuarded(t *testing.T, backend Backend) {
	_, f, inst := k9Tracked(t, backend, nil)
	ctx := caseContext(t)
	if err := inst.Link(ctx, CheckoutR1, CheckoutS1); err != nil {
		t.Fatalf("D18-K9-LinkIsAGuardedTransition: link R1 to S1: %v", err)
	}
	assertServesF(t, ctx, openView(t, ctx, inst, CheckoutR1), f, CheckoutR1, E0)
}

// --- K10 (lifecycle_k10.go) ---

// testK10ConflictingDuplicateRefusesSeal writes Run twice with different
// payloads: the build holds a conflict, so S refuses and nothing publishes.
func testK10ConflictingDuplicateRefusesSeal(t *testing.T, backend Backend) {
	ctx, f, inst := newK1Chain(t, backend)
	base, inv := current(t, ctx, inst, CheckoutR1), inventory(t, ctx, inst)
	build, err := inst.Begin(ctx, BuildRequest{Checkout: CheckoutR1, Inputs: f.Inputs(CheckoutR1, E0), Operation: "k10-conflict"})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	want := f.Payload("r", E0)
	if err := build.Write(ctx, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	conflict := fileContribution(want, "r/app/run.go")
	for i, n := range conflict.Nodes {
		copy := *n
		copy.EndLine += 5
		conflict.Nodes[i] = &copy
	}
	// A refusal at Write must still leave an unpublishable attempt.
	_ = build.Write(ctx, conflict)
	if err := build.Seal(ctx); err == nil {
		t.Fatal("D18-K10-ConflictingDuplicateRefusesSeal: a build holding two different payloads for r/app/run.go::Run sealed")
	}
	if err := build.Publish(ctx, base, "k10-conflict-publish"); err == nil {
		t.Fatal("the conflicting build published after its refused seal")
	}
	if got := current(t, ctx, inst, CheckoutR1); got != base {
		t.Fatalf("the refused build moved R1 from %+v to %+v", base, got)
	}
	if state := inventory(t, ctx, inst)[build.Version()]; state == StateSealed {
		t.Fatalf("the conflicting build %q is sealed (inventory before: %v)", build.Version(), inv)
	}
}

// --- K11 (lifecycle_k11.go) ---

// testK11AliasOfLiveStoreRefuses opens the store a live instance owns through
// a second path, a symlink to its directory: R's ownership is exclusive, so
// the second open refuses.
func testK11AliasOfLiveStoreRefuses(t *testing.T, backend Backend) {
	ctx := caseContext(t)
	f := NewFixtureF(t)
	dir := t.TempDir()
	inst := newChainIn(t, ctx, backend, dir, nil, f, CheckoutR1)
	publishVersion(t, ctx, inst, f, CheckoutR1, E0)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Skipf("the platform cannot make a directory symlink: %v", err)
	}
	second, err := backend.TryOpen(t, alias, nil)
	if err == nil {
		_ = second.Close()
		t.Fatal("D18-K11-AliasOfALiveStoreRefuses: a second instance opened the live store through an alias path")
	}
	assertServesF(t, ctx, openView(t, ctx, inst, CheckoutR1), f, CheckoutR1, E0)
}

// testK9UntrackIsAllOrNone stops the untrack of R1 at each declared stop and
// recovers: R1 is either still tracked and serving complete E0, or gone with
// its version recorded for cleanup; R2 and S1 keep serving complete E0.
func testK9UntrackIsAllOrNone(t *testing.T, backend Backend) {
	dry := &stopper{}
	_, _, inst := k9Tracked(t, backend, dry.hook)
	ctx := caseContext(t)
	dry.arm(stop{})
	if err := inst.Untrack(ctx, CheckoutR1, current(t, ctx, inst, CheckoutR1)); err != nil {
		t.Fatalf("D18-K9-UntrackIsAllOrNone: the unstopped untrack of R1: %v", err)
	}
	arrivals, _ := dry.disarm()
	for _, at := range stopsOf(t, inst.Declaration().FaultPoints, arrivals) {
		t.Run(at.String(), func(t *testing.T) {
			s := &stopper{}
			dir, f, inst := k9Tracked(t, backend, s.hook)
			ctx := caseContext(t)
			before := current(t, ctx, inst, CheckoutR1)
			s.arm(at)
			err := inst.Untrack(ctx, CheckoutR1, before)
			if crashErr := inst.Crash(); crashErr != nil {
				t.Fatalf("end the stopped untrack instance: %v", crashErr)
			}
			if _, stopped := s.disarm(); !stopped || err == nil {
				t.Fatalf("the untrack did not stop at %s (error %v)", at, err)
			}
			recovered := backend.Open(t, dir, nil)
			pub, err := recovered.Current(ctx, CheckoutR1)
			state := inventory(t, ctx, recovered)[before.Version]
			if problem := k9UntrackProblem(before, pub, err, state); problem != nil {
				t.Fatal(problem)
			}
			if err == nil {
				assertServesF(t, ctx, openView(t, ctx, recovered, CheckoutR1), f, CheckoutR1, E0)
			}
			for _, c := range []string{CheckoutR2, CheckoutS1} {
				assertServesF(t, ctx, openView(t, ctx, recovered, c), f, c, E0)
			}
		})
	}
}

// testK9StaleUntrackChangesNothing untracks R1 with a guard an E1 publish
// made stale: the multi-record transition refuses and every checkout keeps
// its publication and answers.
func testK9StaleUntrackChangesNothing(t *testing.T, backend Backend) {
	_, f, inst := k9Tracked(t, backend, nil)
	ctx := caseContext(t)
	stale := current(t, ctx, inst, CheckoutR1)
	publishVersion(t, ctx, inst, f, CheckoutR1, E1)
	routes := map[string]Publication{}
	for _, c := range f.Checkouts {
		routes[c.ID] = current(t, ctx, inst, c.ID)
	}
	inv := inventory(t, ctx, inst)
	if err := inst.Untrack(ctx, CheckoutR1, stale); !errors.Is(err, ErrStaleGuard) {
		t.Fatalf("D18-K9-StaleUntrackGuardChangesNothing: an untrack with a stale guard returned %v, want a stale-guard refusal", err)
	}
	for _, c := range f.Checkouts {
		assertUnchanged(t, ctx, inst, c.ID, routes[c.ID], inv, "the refused untrack")
	}
	assertServesF(t, ctx, openView(t, ctx, inst, CheckoutR1), f, CheckoutR1, E1)
}

// testK13RemoveAndReAdd untracks R1 and tracks it again: the authored records
// stay as they were, and the re-added checkout serves nothing until a new
// publish, so no old route or intent is revived.
func testK13RemoveAndReAdd(t *testing.T, backend Backend) {
	ctx, f, inst := newK1Chain(t, backend)
	for _, record := range []AuthoredRecord{f.Note, f.Memory} {
		if err := inst.PutAuthored(ctx, "r", record); err != nil {
			t.Fatalf("write %s: %v", record.ID, err)
		}
	}
	publishVersion(t, ctx, inst, f, CheckoutR1, E0)
	if err := inst.Untrack(ctx, CheckoutR1, current(t, ctx, inst, CheckoutR1)); err != nil {
		t.Fatalf("D18-K13-RemoveAndReAddKeepsAuthoredAndRevivesNothing: untrack R1: %v", err)
	}
	if err := inst.Track(ctx, f.Checkouts[0]); err != nil {
		t.Fatalf("track R1 again: %v", err)
	}
	if got := current(t, ctx, inst, CheckoutR1); got.Version != "" {
		t.Fatalf("the re-added R1 serves %q before any new publish", got.Version)
	}
	assertAuthored(t, ctx, inst, f, "after remove and re-add")
	publishVersion(t, ctx, inst, f, CheckoutR1, E0)
	assertServesF(t, ctx, openView(t, ctx, inst, CheckoutR1), f, CheckoutR1, E0)
}

func k6RaceProblem(openFirst bool, e0, opened Version, openErr, retireErr error, kept bool) error {
	if openErr == nil {
		if opened != e0 || !kept {
			return fmt.Errorf("open returned %q (E0 %q, still stored %v, retire returned %v)", opened, e0, kept, retireErr)
		}
		return nil
	}
	if !openFirst && retireErr == nil && !kept {
		return nil
	}
	return fmt.Errorf("open refused (%v), open-first %v, retire returned %v, E0 stored %v", openErr, openFirst, retireErr, kept)
}

func k9UntrackProblem(before, pub Publication, currentErr error, state State) error {
	if currentErr != nil && !errors.Is(currentErr, ErrNotTracked) {
		return fmt.Errorf("read R1 after untrack recovery: %w", currentErr)
	}
	if currentErr == nil {
		// A nil error means the route is still there. R1 served E0 before
		// the untrack, so a route that serves nothing is a
		// half-applied untrack.
		if pub.Version == "" {
			return fmt.Errorf("R1 is still tracked but serves nothing, want the untouched %+v or no route", before)
		}
		if pub != before {
			return fmt.Errorf("R1 is still tracked at %+v, want the untouched %+v", pub, before)
		}
		return nil
	}
	if state != "" && state != StateRetiring {
		return fmt.Errorf("R1 is gone but its version %q is %q", before.Version, state)
	}
	return nil
}

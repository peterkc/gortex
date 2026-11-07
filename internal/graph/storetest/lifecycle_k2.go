package storetest

import (
	"errors"
	"testing"
)

// K2 Stale guard and owner (design-c.md 1.8). Given two builds based on E0,
// when one publishes and the other uses its old guard, then the second
// refuses without any catalog mutation. Writes through expired, sealed,
// published or retiring handles refuse; S requires building state. P accepts
// only a sealed eligible candidate with a live claim and fresh guard. The
// winner and existing readers remain intact.

// testK2StaleGuard races two sealed builds from E0: the second publish
// refuses with a stale guard and changes nothing; the winner and a reader
// opened before the race keep their versions.
func testK2StaleGuard(t *testing.T, backend Backend) {
	ctx, f, inst := newK1Chain(t, backend)
	publishVersion(t, ctx, inst, f, CheckoutR1, E0)
	reader := openView(t, ctx, inst, CheckoutR1)
	base := current(t, ctx, inst, CheckoutR1)
	winner := sealedBuild(t, ctx, inst, CheckoutR1, f.Inputs(CheckoutR1, E1), "k2-winner", f.Payload("r", E1))
	loser := sealedBuild(t, ctx, inst, CheckoutR1, altInputs(f, CheckoutR1, E1), "k2-loser", f.Payload("r", E1))
	if winner.Version() == loser.Version() {
		t.Fatalf("builds with different inputs share version %q", winner.Version())
	}
	if err := winner.Publish(ctx, base, "k2-publish-winner"); err != nil {
		t.Fatalf("the first publish over %+v: %v", base, err)
	}
	won := current(t, ctx, inst, CheckoutR1)
	inv := inventory(t, ctx, inst)
	err := loser.Publish(ctx, base, "k2-publish-loser")
	if !errors.Is(err, ErrStaleGuard) {
		t.Fatalf("the second publish with the old guard %+v returned %v, want a stale-guard refusal", base, err)
	}
	assertUnchanged(t, ctx, inst, CheckoutR1, won, inv, "the refused publish")
	assertServesF(t, ctx, reader, f, CheckoutR1, E0)
	assertServesF(t, ctx, openView(t, ctx, inst, CheckoutR1), f, CheckoutR1, E1)
}

// testK2EndedHandles writes through a sealed and through a published handle:
// both refuse, and the versions they address keep exactly F's contents.
func testK2EndedHandles(t *testing.T, backend Backend) {
	ctx, f, inst := newK1Chain(t, backend)
	publishVersion(t, ctx, inst, f, CheckoutR1, E0)
	build := sealedBuild(t, ctx, inst, CheckoutR1, f.Inputs(CheckoutR1, E1), "k2-ended", f.Payload("r", E1))
	if err := build.Write(ctx, extraPayload("r")); err == nil {
		t.Fatal("a write through the sealed handle succeeded")
	}
	if err := build.Publish(ctx, current(t, ctx, inst, CheckoutR1), "k2-ended-publish"); err != nil {
		t.Fatalf("publish the sealed build: %v", err)
	}
	if err := build.Write(ctx, extraPayload("r")); err == nil {
		t.Fatal("a write through the published handle succeeded")
	}
	assertServesF(t, ctx, openView(t, ctx, inst, CheckoutR1), f, CheckoutR1, E1)
}

// testK2RetiringHandle stops a retirement of a sealed, unpublished build at
// each fault point in turn until the build's version is retiring. Publish
// through its handle then refuses, and nothing changes. Writes and seal
// already refuse because the candidate was sealed, not because it is retiring.
func testK2RetiringHandle(t *testing.T, backend Backend) {
	f := NewFixtureF(t)
	chain := func(t *testing.T, at stop) (*stopper, Instance, Build, bool) {
		ctx := caseContext(t)
		s := &stopper{}
		inst := newChainIn(t, ctx, backend, t.TempDir(), s.hook, f, CheckoutR1)
		publishVersion(t, ctx, inst, f, CheckoutR1, E0)
		build := sealedBuild(t, ctx, inst, CheckoutR1, f.Inputs(CheckoutR1, E1), "k2-retiring", f.Payload("r", E1))
		s.arm(at)
		err := inst.Retire(ctx, build.Version())
		_, stopped := s.disarm()
		if at.point != "" && !stopped {
			t.Fatalf("the retirement did not reach %s again (it returned %v)", at, err)
		}
		return s, inst, build, stopped
	}
	dry, _, _, _ := chain(t, stop{})
	arrivals, _ := dry.disarm()
	declared := backend.Open(t, t.TempDir(), nil).Declaration().FaultPoints
	for _, at := range stopsOf(t, declared, arrivals) {
		ctx := caseContext(t)
		_, inst, build, _ := chain(t, at)
		if inventory(t, ctx, inst)[build.Version()] != StateRetiring {
			continue
		}
		before := current(t, ctx, inst, CheckoutR1)
		inv := inventory(t, ctx, inst)
		if err := build.Publish(ctx, before, "k2-retiring-publish"); err == nil {
			t.Fatalf("stopped at %s: a publish of the retiring version succeeded", at)
		}
		assertUnchanged(t, ctx, inst, CheckoutR1, before, inv, "the refused writes through the retiring handle")
		assertServesF(t, ctx, openView(t, ctx, inst, CheckoutR1), f, CheckoutR1, E0)
		return
	}
	t.Fatalf("no stop of the retirement left the version retiring (stops %v)", stopsOf(t, declared, arrivals))
}

// testK2SealRequiresBuilding seals a build twice, and seals a published one:
// S requires building state, so both refuse and nothing changes.
func testK2SealRequiresBuilding(t *testing.T, backend Backend) {
	ctx, f, inst := newK1Chain(t, backend)
	publishVersion(t, ctx, inst, f, CheckoutR1, E0)
	build := sealedBuild(t, ctx, inst, CheckoutR1, f.Inputs(CheckoutR1, E1), "k2-seal", f.Payload("r", E1))
	before, inv := current(t, ctx, inst, CheckoutR1), inventory(t, ctx, inst)
	if err := build.Seal(ctx); err == nil {
		t.Fatal("a second seal of a sealed build succeeded")
	}
	assertUnchanged(t, ctx, inst, CheckoutR1, before, inv, "the refused second seal")
	if err := build.Publish(ctx, before, "k2-seal-publish"); err != nil {
		t.Fatalf("publish the sealed build: %v", err)
	}
	after, inv := current(t, ctx, inst, CheckoutR1), inventory(t, ctx, inst)
	if err := build.Seal(ctx); err == nil {
		t.Fatal("a seal of a published build succeeded")
	}
	assertUnchanged(t, ctx, inst, CheckoutR1, after, inv, "the refused seal of a published build")
	assertServesF(t, ctx, openView(t, ctx, inst, CheckoutR1), f, CheckoutR1, E1)
}

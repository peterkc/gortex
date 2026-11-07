package storetest

import (
	"context"
	"testing"
)

// K6 Pin/retire race (design-c.md 1.8 and 1.5). Given O and T stopped at
// their selection boundary, release them in both orders. Either O pins a
// retained readable version or new O refuses the fenced version. T never
// deletes an active pin, parent, active or previous publication or shared
// referenced piece.
//
// Parents and shared pieces need a layered build, which the neutral Build
// does not express yet.

// k6PinnedPast publishes E0 and pins it, then publishes past it: E0 is then
// neither active nor previous, and only the pin holds it.
func k6PinnedPast(t *testing.T, ctx context.Context, inst Instance, f *FixtureF) (Version, View) {
	t.Helper()
	e0 := publishVersion(t, ctx, inst, f, CheckoutR1, E0)
	view := openView(t, ctx, inst, CheckoutR1)
	k6PublishPast(t, ctx, inst, f)
	return e0, view
}

// k6PublishPast publishes E1 under other inputs and E1, so the version O
// selected is neither active nor previous.
func k6PublishPast(t *testing.T, ctx context.Context, inst Instance, f *FixtureF) {
	t.Helper()
	alt := sealedBuild(t, ctx, inst, CheckoutR1, altInputs(f, CheckoutR1, E1), "k6-race-alt", f.Payload("r", E1))
	if err := alt.Publish(ctx, current(t, ctx, inst, CheckoutR1), "k6-race-alt-publish"); err != nil {
		t.Fatalf("publish E1 under other inputs: %v", err)
	}
	publishVersion(t, ctx, inst, f, CheckoutR1, E1)
}

// testK6RetireKeepsActivePublication retires the active publication: it
// refuses, and the publication stays sealed and complete.
func testK6RetireKeepsActivePublication(t *testing.T, backend Backend) {
	ctx, f, inst := newK1Chain(t, backend)
	e0 := publishVersion(t, ctx, inst, f, CheckoutR1, E0)
	if err := inst.Retire(ctx, e0); err == nil {
		t.Errorf("retiring the active publication %q succeeded", e0)
	}
	if state := inventory(t, ctx, inst)[e0]; state != StateSealed {
		t.Errorf("the active publication %q is %q after the refused retirement, want %q", e0, state, StateSealed)
	}
	assertServesF(t, ctx, openView(t, ctx, inst, CheckoutR1), f, CheckoutR1, E0)
}

// testK6PinDefersRetire retires a version that only a pin holds: the pinned
// view stays complete, and once released the version can be reclaimed.
func testK6PinDefersRetire(t *testing.T, backend Backend) {
	ctx, f, inst := newK1Chain(t, backend)
	e0, view := k6PinnedPast(t, ctx, inst, f)
	retireErr := inst.Retire(ctx, e0)
	assertServesF(t, ctx, view, f, CheckoutR1, E0)
	if retireErr == nil {
		if _, kept := inventory(t, ctx, inst)[e0]; !kept {
			t.Fatalf("retiring the pinned %q reclaimed it under the pin", e0)
		}
	}
	view.Release()
	err := inst.Retire(ctx, e0)
	if _, kept := inventory(t, ctx, inst)[e0]; kept {
		t.Fatalf("after the pin's release %q is still stored (retire returned %v)", e0, err)
	}
	assertServesF(t, ctx, openView(t, ctx, inst, CheckoutR1), f, CheckoutR1, E1)
}

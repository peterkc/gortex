package storetest

import (
	"context"
	"testing"
)

// K5 Orphan recovery (design-c.md 1.8, 1.2 and 1.5). Given an unsealed orphan
// and a sealed unrouted orphan, when R runs, then neither is served. Matching
// abandoned sealed work can be validated and adopted; active-owner or
// mismatched-input adoption refuses. Reclaim touches only proven unreferenced
// inventory.

// k5Orphans publishes E0 for R1, leaves a sealed E1 orphan and an unsealed
// orphan with other inputs, and recovers the files in a new instance.
func k5Orphans(t *testing.T, backend Backend) (context.Context, *FixtureF, Instance, Version, Version, Version) {
	t.Helper()
	ctx := caseContext(t)
	f := NewFixtureF(t)
	dir := t.TempDir()
	dead := newChainIn(t, ctx, backend, dir, nil, f, CheckoutR1)
	e0 := publishVersion(t, ctx, dead, f, CheckoutR1, E0)
	sealed := sealedBuild(t, ctx, dead, CheckoutR1, f.Inputs(CheckoutR1, E1), "k5-sealed", f.Payload("r", E1))
	unsealed, err := dead.Begin(ctx, BuildRequest{Checkout: CheckoutR1, Inputs: altInputs(f, CheckoutR1, E1), Operation: "k5-unsealed"})
	if err != nil {
		t.Fatalf("begin the unsealed orphan: %v", err)
	}
	if err := unsealed.Write(ctx, f.Payload("r", E1)); err != nil {
		t.Fatalf("write the unsealed orphan: %v", err)
	}
	if err := dead.Crash(); err != nil {
		t.Fatalf("end the orphan owner: %v", err)
	}
	for _, build := range []Build{sealed, unsealed} {
		if err := build.Write(ctx, extraPayload("r")); err == nil {
			t.Fatalf("a late write through the crashed owner of %q succeeded", build.Version())
		}
	}
	return ctx, f, backend.Open(t, dir, nil), e0, sealed.Version(), unsealed.Version()
}

// testK5OrphansNotServed recovers both orphans: R1 still serves complete E0,
// the sealed orphan stays sealed and the unsealed one is not sealed.
func testK5OrphansNotServed(t *testing.T, backend Backend) {
	ctx, f, inst, e0, sealed, unsealed := k5Orphans(t, backend)
	if got := current(t, ctx, inst, CheckoutR1).Version; got != e0 {
		t.Fatalf("after recovery R1 serves %q, want E0 %q", got, e0)
	}
	assertServesF(t, ctx, openView(t, ctx, inst, CheckoutR1), f, CheckoutR1, E0)
	inv := inventory(t, ctx, inst)
	if inv[sealed] != StateSealed {
		t.Errorf("the sealed orphan %q is %q after recovery, want %q", sealed, inv[sealed], StateSealed)
	}
	if state, ok := inv[unsealed]; ok && state == StateSealed {
		t.Errorf("the unsealed orphan %q is sealed after recovery", unsealed)
	}
}

// testK5MismatchedInputsNotAdopted begins other inputs on the recovered
// instance: the sealed orphan is not adopted and stays sealed.
func testK5MismatchedInputsNotAdopted(t *testing.T, backend Backend) {
	ctx, f, inst, _, sealed, _ := k5Orphans(t, backend)
	inputs := f.Inputs(CheckoutR1, E1)
	inputs.Resolver = "resolver-other"
	build, err := inst.Begin(ctx, BuildRequest{Checkout: CheckoutR1, Inputs: inputs, Operation: "k5-mismatch"})
	if err == nil && build.Version() == sealed {
		t.Fatalf("a begin with other inputs adopted the sealed orphan %q", sealed)
	}
	if state := inventory(t, ctx, inst)[sealed]; state != StateSealed {
		t.Fatalf("after the mismatched begin the sealed orphan is %q, want %q", state, StateSealed)
	}
}

// testK5ReclaimOnlyUnreferenced retires both orphans on the recovered
// instance and tries the published E0: the orphans go, E0 refuses and stays
// complete.
func testK5ReclaimOnlyUnreferenced(t *testing.T, backend Backend) {
	ctx, f, inst, e0, sealed, unsealed := k5Orphans(t, backend)
	for _, orphan := range []Version{sealed, unsealed} {
		if err := inst.Retire(ctx, orphan); err != nil {
			t.Errorf("reclaim the orphan %q: %v", orphan, err)
		}
	}
	if err := inst.Retire(ctx, e0); err == nil {
		t.Errorf("retiring the published E0 %q succeeded", e0)
	}
	inv := inventory(t, ctx, inst)
	if want := (map[Version]State{e0: StateSealed}); len(inv) != 1 || inv[e0] != StateSealed {
		t.Errorf("after reclaim the inventory is %v, want %v", inv, want)
	}
	assertServesF(t, ctx, openView(t, ctx, inst, CheckoutR1), f, CheckoutR1, E0)
}

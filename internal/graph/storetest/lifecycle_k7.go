package storetest

import (
	"context"
	"errors"
	"testing"
)

// K7 Interrupted retire (design-c.md 1.8 and 1.5). Given a fenced,
// unreferenced version and partial deletion, when cancellation or a crash
// interrupts T, then R resumes only after checking the fence, roots and pins
// again. Repeated cleanup is bounded and idempotent. New reference attempts
// and late writes visibly refuse.
//
// FR-5 binding: the case stops T at every arrival at every declared fault
// point, opens a new instance over the same files and resumes there; it also
// cancels T at each of those arrivals and resumes on the same instance. The
// version T retires is a sealed E1 candidate nobody published.

// k7Chain publishes E0 for R1, seals the unpublished candidate unarmed and
// retires the candidate armed at at. With cancelAtStop, the stop cancels the
// retirement's context instead of returning errStopped, so whether the stop
// was reached comes from the stopper.
func k7Chain(t *testing.T, backend Backend, f *FixtureF, s *stopper, at stop, cancelAtStop bool) (string, Instance, Build, Publication, error) {
	t.Helper()
	ctx := caseContext(t)
	dir := t.TempDir()
	inst := newChainIn(t, ctx, backend, dir, s.hook, f, CheckoutR1)
	publishVersion(t, ctx, inst, f, CheckoutR1, E0)
	candidate := sealedBuild(t, ctx, inst, CheckoutR1, f.Inputs(CheckoutR1, E1), "k7-candidate", f.Payload("r", E1))
	base := current(t, ctx, inst, CheckoutR1)
	retireCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if cancelAtStop {
		s.cancel = cancel
	}
	s.arm(at)
	err := inst.Retire(retireCtx, candidate.Version())
	stopped := errors.Is(err, errStopped)
	if cancelAtStop {
		_, stopped = s.disarm()
	}
	if at.point == "" && err != nil {
		t.Fatalf("the unstopped retirement: %v", err)
	}
	if at.point != "" && !stopped {
		t.Fatalf("the retirement did not reach %s again (it returned %v)", at, err)
	}
	return dir, inst, candidate, base, err
}

// testK7InterruptedRetire crashes T at each stop and resumes it in a new
// instance.
func testK7InterruptedRetire(t *testing.T, backend Backend) {
	f := NewFixtureF(t)
	for _, at := range k7Stops(t, backend, f) {
		t.Run(at.String(), func(t *testing.T) {
			s := &stopper{}
			defer s.disarm()
			dir, old, candidate, base, err := k7Chain(t, backend, f, s, at, false)
			if !errors.Is(err, errStopped) {
				t.Fatalf("the retirement stopped at %s returned %v, want the stop", at, err)
			}
			stoppedState := inventory(t, caseContext(t), old)[candidate.Version()]
			if err := old.Crash(); err != nil {
				t.Fatalf("end the interrupted retire instance: %v", err)
			}
			// Disarm so late publish tests the closed owner, not the stopper.
			// A late write would only test that the candidate is sealed.
			s.disarm()
			ctx := caseContext(t)
			if err := candidate.Publish(ctx, base, "k7-crashed-publish"); err == nil {
				t.Fatal("a late publish through the crashed retire instance succeeded")
			}
			recovered := backend.Open(t, dir, nil)
			state := inventory(t, ctx, recovered)[candidate.Version()]
			if state != stoppedState {
				t.Fatalf("after a crash at %s the candidate %q changed state from %q to %q", at, candidate.Version(), stoppedState, state)
			}
			if state != StateSealed {
				_, err := recovered.Begin(ctx, BuildRequest{
					Checkout: CheckoutR1, Inputs: f.Inputs(CheckoutR1, E1),
					Parent: candidate.Version(), Operation: "k7-new-reference",
				})
				if state == StateRetiring && !errors.Is(err, ErrFenced) {
					t.Fatalf("after a crash at %s a new reference to the fenced candidate %q returned %v, want %v", at, candidate.Version(), err, ErrFenced)
				}
				if err == nil {
					t.Fatalf("after a crash at %s a new reference to the removed candidate %q succeeded", at, candidate.Version())
				}
				if got := current(t, ctx, recovered, CheckoutR1); got != base {
					t.Fatalf("the refused reference moved R1 from %+v to %+v", base, got)
				}
			}
			k7AssertResumes(t, recovered, f, candidate, base, "after a crash at "+at.String())
		})
	}
}

// testK7CancelledRetire cancels T at each stop and resumes it on the same
// instance.
func testK7CancelledRetire(t *testing.T, backend Backend) {
	f := NewFixtureF(t)
	for _, at := range k7Stops(t, backend, f) {
		t.Run(at.String(), func(t *testing.T) {
			_, inst, candidate, base, _ := k7Chain(t, backend, f, &stopper{}, at, true)
			k7AssertResumes(t, inst, f, candidate, base, "after a cancellation at "+at.String())
		})
	}
}

func k7Stops(t *testing.T, backend Backend, f *FixtureF) []stop {
	t.Helper()
	dry := &stopper{}
	// k7Chain fails the test itself when the unstopped retirement returns an error.
	_, _, _, _, _ = k7Chain(t, backend, f, dry, stop{}, false)
	arrivals, _ := dry.disarm()
	return stopsOf(t, backend.Open(t, t.TempDir(), nil).Declaration().FaultPoints, arrivals)
}

// k7AssertResumes checks inst after an interrupted retirement of candidate:
// E0 is untouched; a fenced candidate refuses a late publish; T resumes to
// the end; and repeating it changes nothing.
func k7AssertResumes(t *testing.T, inst Instance, f *FixtureF, candidate Build, base Publication, what string) {
	t.Helper()
	ctx := caseContext(t)
	version := candidate.Version()
	if got := current(t, ctx, inst, CheckoutR1); got != base {
		t.Fatalf("%s R1's publication is %+v, want the untouched %+v", what, got, base)
	}
	state, stored := inventory(t, ctx, inst)[version]
	if stored && state != StateSealed && state != StateRetiring {
		t.Fatalf("%s the candidate %q is %q", what, version, state)
	}
	if !stored || state == StateRetiring {
		if err := candidate.Publish(ctx, base, "k7-late-publish"); err == nil {
			t.Fatalf("%s a late publish of the fenced candidate %q succeeded", what, version)
		}
		if err := candidate.Write(ctx, extraPayload("r")); err == nil {
			t.Fatalf("%s a late write to the fenced candidate %q succeeded", what, version)
		}
		if got := current(t, ctx, inst, CheckoutR1); got != base {
			t.Fatalf("%s the refused late publish moved R1 from %+v to %+v", what, base, got)
		}
	}
	if err := inst.Retire(ctx, version); stored && err != nil {
		t.Fatalf("%s resuming the retirement of %q: %v", what, version, err)
	}
	inv := inventory(t, ctx, inst)
	if _, kept := inv[version]; kept {
		t.Fatalf("%s the resumed retirement left %q stored: %v", what, version, inv)
	}
	// A finished retirement may refuse the unknown version; either way the
	// repeat must change nothing.
	_ = inst.Retire(ctx, version)
	assertUnchanged(t, ctx, inst, CheckoutR1, base, inv, what+", repeating the finished retirement")
	assertServesF(t, ctx, openView(t, ctx, inst, CheckoutR1), f, CheckoutR1, E0)
}

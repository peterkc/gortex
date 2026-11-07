package storetest

import (
	"context"
	"testing"
)

// K3 Interrupted publish (design-c.md 1.8, 1.4 and 5.1). Given E0 and
// candidate E1, stop at each durable boundary the backend declares. R yields
// E0 or complete E1, never mixed payloads. Fail or cancel before S refuses P.
// No acknowledged publication is lost.
//
// FR-5 binding: the case stops at every arrival at every declared fault
// point of the E1 chain (begin, write, seal, publish), then opens a new
// instance over the same files and checks the old or new complete state.
// Torn and reordered sector writes (revision 1) apply to sealed files; a
// backend that declares none has no file to tear.

// k3Chain publishes E0 for R1 unarmed, then runs the E1 chain armed at at.
// It returns the instance's directory, the E0 version, the candidate's
// version (empty if begin did not return) and the chain's error.
func k3Chain(t *testing.T, backend Backend, f *FixtureF, s *stopper, at stop) (string, Version, Version, error) {
	t.Helper()
	ctx := caseContext(t)
	dir := t.TempDir()
	inst := newChainIn(t, ctx, backend, dir, s.hook, f, CheckoutR1)
	e0 := publishVersion(t, ctx, inst, f, CheckoutR1, E0)
	base := current(t, ctx, inst, CheckoutR1)
	s.arm(at)
	defer s.disarm()
	defer func() {
		if err := inst.Crash(); err != nil {
			t.Fatalf("end the E1 instance before recovery: %v", err)
		}
	}()
	build, err := inst.Begin(ctx, BuildRequest{Checkout: CheckoutR1, Inputs: f.Inputs(CheckoutR1, E1), Operation: "k3-build"})
	if err != nil {
		return dir, e0, "", err
	}
	if err := build.Write(ctx, f.Payload("r", E1)); err != nil {
		return dir, e0, build.Version(), err
	}
	if err := build.Seal(ctx); err != nil {
		return dir, e0, build.Version(), err
	}
	return dir, e0, build.Version(), build.Publish(ctx, base, "k3-publish")
}

// testK3InterruptedPublish stops the E1 chain at each declared stop and
// recovers: the new instance serves complete E0 or complete E1.
func testK3InterruptedPublish(t *testing.T, backend Backend) {
	f := NewFixtureF(t)
	dry := &stopper{}
	if _, _, _, err := k3Chain(t, backend, f, dry, stop{}); err != nil {
		t.Fatalf("the unstopped E1 chain: %v", err)
	}
	arrivals, _ := dry.disarm()
	declared := backend.Open(t, t.TempDir(), nil).Declaration().FaultPoints
	for _, at := range stopsOf(t, declared, arrivals) {
		t.Run(at.String(), func(t *testing.T) {
			s := &stopper{}
			dir, e0, candidate, err := k3Chain(t, backend, f, s, at)
			if _, stopped := s.disarm(); !stopped || err == nil {
				t.Fatalf("the chain did not stop at %s (stopped %v, error %v)", at, stopped, err)
			}
			assertRecoversOldOrNew(t, caseContext(t), backend.Open(t, dir, nil), f, e0, candidate)
		})
	}
}

// testK3AcknowledgedPublishSurvives recovers after an unstopped E1 chain: the
// acknowledged publication is the one the new instance serves.
func testK3AcknowledgedPublishSurvives(t *testing.T, backend Backend) {
	f := NewFixtureF(t)
	dir, _, candidate, err := k3Chain(t, backend, f, &stopper{}, stop{})
	if err != nil {
		t.Fatalf("the E1 chain: %v", err)
	}
	ctx := caseContext(t)
	recovered := backend.Open(t, dir, nil)
	if got := current(t, ctx, recovered, CheckoutR1).Version; got != candidate {
		t.Fatalf("after the acknowledged publish of %q the recovered instance serves %q", candidate, got)
	}
	assertServesF(t, ctx, openView(t, ctx, recovered, CheckoutR1), f, CheckoutR1, E1)
}

// assertRecoversOldOrNew checks a recovered instance: R1 serves complete E0,
// or complete E1 from candidate, and nothing else.
func assertRecoversOldOrNew(t *testing.T, ctx context.Context, recovered Instance, f *FixtureF, e0, candidate Version) {
	t.Helper()
	got := current(t, ctx, recovered, CheckoutR1).Version
	switch {
	case got == e0:
		assertServesF(t, ctx, openView(t, ctx, recovered, CheckoutR1), f, CheckoutR1, E0)
	case got != "" && got == candidate:
		assertServesF(t, ctx, openView(t, ctx, recovered, CheckoutR1), f, CheckoutR1, E1)
	default:
		t.Fatalf("the recovered instance serves %q; want E0 %q or the candidate %q", got, e0, candidate)
	}
}

// Each declared sealed kind must reject torn or reordered candidate bytes;
// recovery must keep complete E0, never publish the damaged candidate.
func testK3DamagedSealedCandidate(t *testing.T, backend Backend) {
	kinds := backend.Open(t, t.TempDir(), nil).Declaration().SealedFileKinds
	for _, kind := range kinds {
		for _, damage := range []Damage{DamageSealedTorn, DamageSealedReordered} {
			t.Run(kind+"/"+string(damage), func(t *testing.T) {
				ctx := caseContext(t)
				f := NewFixtureF(t)
				dir := t.TempDir()
				inst := newChainIn(t, ctx, backend, dir, nil, f, CheckoutR1)
				publishVersion(t, ctx, inst, f, CheckoutR1, E0)
				base := current(t, ctx, inst, CheckoutR1)
				req := BuildRequest{Checkout: CheckoutR1, Inputs: f.Inputs(CheckoutR1, E1), Operation: "k3-sealed-" + kind}
				build, err := inst.PrepareSealedFile(ctx, kind, req, f.Payload("r", E1))
				if err != nil {
					t.Fatalf("prepare declared %s: %v", kind, err)
				}
				if err := inst.Close(); err != nil {
					t.Fatal(err)
				}
				if backend.DamageSealedFile == nil {
					t.Fatal("declared sealed files without a damage operation")
				}
				if err := backend.DamageSealedFile(ctx, dir, build.Version(), kind, damage); err != nil {
					t.Fatalf("damage declared %s: %v", kind, err)
				}
				recovered := backend.Open(t, dir, nil)
				// Rejoining may refuse invalid orphan work. If it returns a
				// handle, P must still refuse the damaged candidate.
				if resumed, err := recovered.Begin(ctx, req); err == nil {
					if err := resumed.Publish(ctx, base, "k3-damaged-publish"); err == nil {
						t.Fatal("damaged sealed candidate published")
					}
				}
				if got := current(t, ctx, recovered, CheckoutR1); got != base {
					t.Fatalf("damaged candidate moved R1 from %+v to %+v", base, got)
				}
				assertServesF(t, ctx, openView(t, ctx, recovered, CheckoutR1), f, CheckoutR1, E0)
			})
		}
	}
}

func testK3SealedDigestAtFirstMap(t *testing.T, backend Backend) {
	for _, kind := range backend.Open(t, t.TempDir(), nil).Declaration().SealedFileKinds {
		t.Run(kind, func(t *testing.T) { testSealedFileRefusal(t, backend, kind, DamageSealedDigest) })
	}
}

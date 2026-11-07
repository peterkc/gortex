package storetest

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// Shared machinery of the interruption cases K2, K3, K4 and K7: a fault hook
// the case arms around the operation it interrupts, the list of stops a dry
// run reaches, and small chain helpers.

// errStopped is what a stopper returns at its stop and at every later point:
// the instance behaves as a process that crashed there.
var errStopped = errors.New("storetest: stopped at a fault point")

// stop names the arrival-th arrival (from 0) at one fault point.
type stop struct {
	point   string
	arrival int
}

func (s stop) String() string { return fmt.Sprintf("%s#%d", s.point, s.arrival) }

// stopper is the fault hook of one instance. It does nothing until armed;
// armed, it counts arrivals at each point and stops at its stop.
type stopper struct {
	mu       sync.Mutex
	armed    bool
	at       stop
	arrivals map[string]int
	stopped  bool
	// cancel, when set, makes the stop cancel the operation's context and
	// let it continue, instead of crashing it.
	cancel context.CancelFunc
}

func (s *stopper) hook(point string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.armed {
		return nil
	}
	if s.stopped && s.cancel == nil {
		return fmt.Errorf("%w: %s, after the stop at %s", errStopped, point, s.at)
	}
	n := s.arrivals[point]
	s.arrivals[point] = n + 1
	if s.stopped || point != s.at.point || n != s.at.arrival {
		return nil
	}
	s.stopped = true
	if s.cancel != nil {
		s.cancel()
		return nil
	}
	return fmt.Errorf("%w: %s", errStopped, s.at)
}

// arm starts counting from zero and stops at at; a zero stop never stops.
func (s *stopper) arm(at stop) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armed, s.at, s.stopped, s.arrivals = true, at, false, map[string]int{}
}

// disarm stops counting and reports the arrivals and whether the stop was
// reached.
func (s *stopper) disarm() (map[string]int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armed = false
	return maps.Clone(s.arrivals), s.stopped
}

// stopsOf lists every stop a dry run reached, in the backend's declared point
// order and then by arrival. A point the backend did not declare fails.
func stopsOf(t *testing.T, declared []string, arrivals map[string]int) []stop {
	t.Helper()
	for point := range arrivals {
		if !slices.Contains(declared, point) {
			t.Fatalf("the hook was called at %q, which the backend does not declare (it declares %v)", point, declared)
		}
	}
	var out []stop
	for _, point := range declared {
		for i := range arrivals[point] {
			out = append(out, stop{point: point, arrival: i})
		}
	}
	if len(out) == 0 {
		t.Fatalf("the operation reached none of the declared fault points %v", declared)
	}
	return out
}

// newChainIn opens an instance over dir with hook and tracks checkouts.
func newChainIn(t *testing.T, ctx context.Context, backend Backend, dir string, hook FaultHook, f *FixtureF, checkouts ...string) Instance {
	t.Helper()
	inst := backend.Open(t, dir, hook)
	for _, c := range f.Checkouts {
		if !slices.Contains(checkouts, c.ID) {
			continue
		}
		if err := inst.Track(ctx, c); err != nil {
			t.Fatalf("track %s: %v", c.ID, err)
		}
	}
	return inst
}

// sealedBuild begins a build of checkout with inputs, writes payload and
// seals it, without publishing.
func sealedBuild(t *testing.T, ctx context.Context, inst Instance, checkout string, inputs Inputs, operation string, payload Payload) Build {
	t.Helper()
	build, err := inst.Begin(ctx, BuildRequest{Checkout: checkout, Inputs: inputs, Operation: operation})
	if err != nil {
		t.Fatalf("%s: begin: %v", operation, err)
	}
	if err := build.Write(ctx, payload); err != nil {
		t.Fatalf("%s: write: %v", operation, err)
	}
	if err := build.Seal(ctx); err != nil {
		t.Fatalf("%s: seal: %v", operation, err)
	}
	return build
}

// current is the checkout's publication, failing the case on error.
func current(t *testing.T, ctx context.Context, inst Instance, checkout string) Publication {
	t.Helper()
	pub, err := inst.Current(ctx, checkout)
	if err != nil {
		t.Fatalf("%s: current publication: %v", checkout, err)
	}
	return pub
}

// inventory is the instance's inventory as a map, failing on a version listed
// twice.
func inventory(t *testing.T, ctx context.Context, inst Instance) map[Version]State {
	t.Helper()
	stored, err := inst.Inventory(ctx)
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	out := make(map[Version]State, len(stored))
	for _, s := range stored {
		if _, dup := out[s.Version]; dup {
			t.Fatalf("the inventory lists version %q twice: %+v", s.Version, stored)
		}
		out[s.Version] = s.State
	}
	return out
}

// assertUnchanged checks that checkout still has publication want and the
// inventory is still inv, after what.
func assertUnchanged(t *testing.T, ctx context.Context, inst Instance, checkout string, want Publication, inv map[Version]State, what string) {
	t.Helper()
	if got := current(t, ctx, inst, checkout); got != want {
		t.Fatalf("%s moved %s's publication from %+v to %+v", what, checkout, want, got)
	}
	if got := inventory(t, ctx, inst); !maps.Equal(got, inv) {
		t.Fatalf("%s changed the inventory from %v to %v", what, inv, got)
	}
}

// altInputs is F's input identity of checkout at epoch with another config:
// a different build of the same source.
func altInputs(f *FixtureF, checkout string, epoch Epoch) Inputs {
	inputs := f.Inputs(checkout, epoch)
	inputs.Config = "config-f-alt"
	return inputs
}

// extraPayload is a contribution F never holds, so a write that should have
// refused shows up in any later read of the version.
func extraPayload(prefix string) Payload {
	return Payload{Nodes: []*graph.Node{{
		ID: prefix + "/extra.go::Extra", Kind: graph.KindFunction, Name: "Extra",
		FilePath: prefix + "/extra.go", StartLine: 1, EndLine: 1, Language: "go", RepoPrefix: prefix,
	}}}
}

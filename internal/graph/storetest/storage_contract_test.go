package storetest_test

import (
	"errors"
	"testing"

	"github.com/zzet/gortex/internal/graph/storetest"
	"github.com/zzet/gortex/internal/graph/storetest/backendtest"
)

// Given a checkout without a route, Current wraps ErrNotTracked on every
// listed backend. A failed catalog read must remain a different error, not
// appear to be an untrack.
func TestStorageContractCurrentDistinguishesAbsentRouteFromReadFailure(t *testing.T) {
	for _, name := range storetest.Backends() {
		t.Run(name, func(t *testing.T) {
			backend, ok := backendtest.Lookup(name)
			if !ok {
				t.Fatalf("backend %q is listed but backendtest has no adapter for it", name)
			}
			inst := backend.Open(t, t.TempDir(), nil)
			if _, err := inst.Current(t.Context(), "never-tracked"); !errors.Is(err, storetest.ErrNotTracked) {
				t.Fatalf("missing route: got %v, want ErrNotTracked", err)
			}
			if err := inst.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := inst.Current(t.Context(), "never-tracked"); err == nil || errors.Is(err, storetest.ErrNotTracked) {
				t.Fatalf("failed catalog read: got %v, want a non-ErrNotTracked error", err)
			}
		})
	}
}

// TestStorageContract runs the lifecycle and catalog suite on every backend in
// the backend list. A listed backend without an adapter fails.
func TestStorageContract(t *testing.T) {
	for _, name := range storetest.Backends() {
		t.Run(name, func(t *testing.T) {
			backend, ok := backendtest.Lookup(name)
			if !ok {
				t.Fatalf("backend %q is listed but backendtest has no adapter for it", name)
			}
			storetest.RunStorageContract(t, backend, backendtest.Lookup)
		})
	}
}

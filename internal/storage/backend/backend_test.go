package backend

import (
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestOpenRawKeepsNativeStoreAndMemoryPath(t *testing.T) {
	for _, name := range []string{"", "sqlite", " SQLITE3 "} {
		t.Run(name, func(t *testing.T) {
			store, closeStore, err := OpenRaw(name, ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			native, ok := store.(*store_sqlite.Store)
			if !ok || native == nil {
				t.Fatalf("not a native store: %T", store)
			}
			if native.Path() != ":memory:" {
				t.Fatalf("raw path changed: %q", native.Path())
			}
			if err := closeStore(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOpenRawKeepsOpenErrorAndNilResults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store")
	// A directory is not a database. Neither opener adds daemon diagnostics.
	_, nativeErr := store_sqlite.Open(filepath.Dir(path))
	store, closeStore, err := OpenRaw("sqlite", filepath.Dir(path))
	if err == nil || nativeErr == nil || err.Error() != nativeErr.Error() {
		t.Fatalf("raw error = %v, native error = %v", err, nativeErr)
	}
	if store != nil || closeStore != nil {
		t.Fatal("failed open returned resources")
	}
}

func TestOpenRawRefusesNamesBeforeOpening(t *testing.T) {
	for _, name := range []string{"memory", "mem", "in-memory", "other"} {
		store, closeStore, err := OpenRaw(name, ":memory:")
		if err == nil || store != nil || closeStore != nil {
			t.Fatalf("accepted %q: %T, %v", name, store, err)
		}
		if check := CheckName(name); check == nil || check.Error() != err.Error() {
			t.Fatalf("name check differs: %v", check)
		}
	}
}

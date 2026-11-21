package backend

import (
	"errors"
	"os"
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

func TestPebbleBuildOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private")
	for _, rawName := range []string{"pebble", " PEBBLE "} {
		if err := CheckName(rawName); !errors.Is(err, errors.ErrUnsupported) {
			t.Fatalf("production name was not refused: %v", err)
		}
		s, closeStore, err := OpenRaw(rawName, path)
		if s != nil || closeStore != nil || !errors.Is(err, errors.ErrUnsupported) {
			t.Fatalf("production open = %v, %v, %v", s, closeStore == nil, err)
		}
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("production refusal touched the store: %v", err)
	}
	b, err := OpenPebbleBuild(path, "full")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Error(err)
		}
	})
}

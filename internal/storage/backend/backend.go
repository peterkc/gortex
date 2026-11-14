// Package backend selects the storage implementation without applying caller path
// or diagnostic policy.
package backend

import (
	"errors"
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/storage"
)

// OpenOptions are for the managed open after the caller resolves the path.
type OpenOptions struct {
	// AllowRebuild permits rebuilds and upgrade backups. The caller must
	// hold the exclusive store lock before setting it.
	AllowRebuild      bool
	MigrationObserver storage.MigrationObserver
	UpgradeHook       UpgradeHook
}

// UpgradeHook is an optional per-open hook set only in acceptance builds.
type UpgradeHook struct{ option store_sqlite.Option }

type registration struct {
	open    func(string, OpenOptions) (graph.Store, func() error, error)
	offline any
}

var backends = map[string]registration{
	"sqlite": {open: openSQLite, offline: store_sqlite.OfflineMaintenance{}},
}

// RepoIndexStateBaseViewGen is the scope of the SQLite freshness probe.
const RepoIndexStateBaseViewGen = store_sqlite.RepoIndexStateBaseViewGen

// Offline returns the selected backend's file-only capabilities, or nil when
// unavailable. It never opens a store. An empty name selects the default.
func Offline(name string) any {
	selected, _ := selectBackend(name)
	return selected.offline
}

// CheckName validates a backend name before any filesystem work.
func CheckName(name string) error {
	_, err := selectBackend(name)
	return err
}

func selectBackend(name string) (registration, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	switch key {
	case "", "sqlite3":
		key = "sqlite"
	case "memory", "mem", "in-memory":
		return registration{}, errors.New("the in-memory backend is retired; use --backend sqlite --backend-path <path> " +
			"(a throwaway path gives the old ephemeral behaviour)")
	}
	selected, ok := backends[key]
	if !ok {
		return registration{}, fmt.Errorf("unknown backend %q (expected: sqlite)", name)
	}
	return selected, nil
}

// OpenRaw passes path unchanged to the selected backend. It returns the native
// store and its close function, without default paths, rebuilds or error hints.
func OpenRaw(name, path string) (graph.Store, func() error, error) {
	return Open(name, path, OpenOptions{})
}

// Open passes the resolved path and requested options to the selected backend.
// It returns the native store, without wrapping its capabilities or errors.
func Open(name, path string, options OpenOptions) (graph.Store, func() error, error) {
	selected, err := selectBackend(name)
	if err != nil {
		return nil, nil, err
	}
	return selected.open(path, options)
}

func openSQLite(path string, options OpenOptions) (graph.Store, func() error, error) {
	var opts []store_sqlite.Option
	if options.MigrationObserver != nil {
		opts = append(opts, store_sqlite.WithMigrationObserver(options.MigrationObserver))
	}
	if options.AllowRebuild {
		opts = append(opts, store_sqlite.WithRebuild(), store_sqlite.WithUpgradeBackup())
	}
	if options.UpgradeHook.option != nil {
		opts = append(opts, options.UpgradeHook.option)
	}
	store, err := store_sqlite.Open(path, opts...)
	if err != nil {
		return nil, nil, err
	}
	return store, store.Close, nil
}

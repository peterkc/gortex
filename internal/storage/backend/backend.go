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

type constructor func(string, OpenOptions) (graph.Store, func() error, error)

var constructors = map[string]constructor{
	"sqlite": openSQLite,
}

// CheckName validates a backend name before any filesystem work.
func CheckName(name string) error {
	_, err := selectConstructor(name)
	return err
}

func selectConstructor(name string) (constructor, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	switch key {
	case "", "sqlite3":
		key = "sqlite"
	case "memory", "mem", "in-memory":
		return nil, errors.New("the in-memory backend is retired; use --backend sqlite --backend-path <path> " +
			"(a throwaway path gives the old ephemeral behaviour)")
	}
	open, ok := constructors[key]
	if !ok {
		return nil, fmt.Errorf("unknown backend %q (expected: sqlite)", name)
	}
	return open, nil
}

// OpenRaw passes path unchanged to the selected backend. It returns the native
// store and its close function, without default paths, rebuilds or error hints.
func OpenRaw(name, path string) (graph.Store, func() error, error) {
	return Open(name, path, OpenOptions{})
}

// Open passes the resolved path and requested options to the selected backend.
// It returns the native store, without wrapping its capabilities or errors.
func Open(name, path string, options OpenOptions) (graph.Store, func() error, error) {
	open, err := selectConstructor(name)
	if err != nil {
		return nil, nil, err
	}
	return open(path, options)
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

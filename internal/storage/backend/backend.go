// Package backend selects the storage implementation without applying caller path
// or diagnostic policy.
package backend

import (
	"errors"
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type constructor func(string) (graph.Store, func() error, error)

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
	open, err := selectConstructor(name)
	if err != nil {
		return nil, nil, err
	}
	return open(path)
}

func openSQLite(path string) (graph.Store, func() error, error) {
	store, err := store_sqlite.Open(path)
	if err != nil {
		return nil, nil, err
	}
	return store, store.Close, nil
}

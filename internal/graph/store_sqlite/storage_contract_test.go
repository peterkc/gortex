package store_sqlite_test

import (
	"testing"

	"github.com/zzet/gortex/internal/graph/storetest"
	"github.com/zzet/gortex/internal/graph/storetest/backendtest"
)

// TestStorageContract runs the shared lifecycle and catalog suite on SQLite.
// storetest's own TestStorageContract runs it on every listed backend.
func TestStorageContract(t *testing.T) {
	storetest.RunStorageContract(t, backendtest.SQLite(), backendtest.Lookup)
}

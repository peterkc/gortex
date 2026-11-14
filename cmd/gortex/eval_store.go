package main

import (
	"os"
	"path/filepath"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/storage/backend"
)

// newEvalStore opens a throwaway SQLite graph store in a temp directory
// for one eval run. The eval harnesses index a whole repository and then
// measure the retrieval stack on top of it, so they must run against the
// same store the daemon serves: SQLite maintains the symbol FTS and the
// vector index transactionally, and the indexer picks its search backend
// from what the store implements. Measuring anything else would report
// numbers no user can reproduce.
//
// The returned cleanup closes the store and removes the temp directory;
// callers MUST call it once they are done reading the store.
func newEvalStore(name string) (graph.Store, func(), error) {
	return newEvalStoreForBackend(name, "")
}

func newEvalStoreForBackend(name, backendName string) (graph.Store, func(), error) {
	tmpDir, err := os.MkdirTemp("", "gortex-eval-"+name+"-*")
	if err != nil {
		return nil, nil, err
	}
	st, closeStore, err := backend.OpenRaw(backendName, filepath.Join(tmpDir, name+".sqlite"))
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, nil, err
	}
	cleanup := func() {
		_ = closeStore()
		_ = os.RemoveAll(tmpDir)
	}
	return st, cleanup, nil
}

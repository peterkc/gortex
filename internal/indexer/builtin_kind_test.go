package indexer

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Given two files that use the Go builtins append and len, and a file that
// calls into one of them, when an incremental update deletes the called file or
// adds the first use of another builtin, then the stored builtin rows, metadata included, equal
// a clean index of the same tree in both resolution modes.
func TestBuiltinKindIncrementalMatchesCleanIndex(t *testing.T) {
	tree := map[string]string{
		"go.mod":  "module example.com/fixture\n\ngo 1.22\n",
		"grow.go": "package fixture\n\nfunc Grow(xs []int) []int { return append(xs, len(xs)) }\n",
		"keep.go": "package fixture\n\nfunc Keep(xs []int) []int { return append(xs, len(xs)+1) }\n",
		"use.go":  "package fixture\n\nfunc Use() []int { return Grow(nil) }\n",
	}
	for _, deferred := range []bool{false, true} {
		t.Run(fmt.Sprintf("deferred=%t", deferred), func(t *testing.T) {
			dir := builderTempDir(t, "builtin-kind")
			builderWriteTree(t, dir, tree)
			store := builderOpenStore(t, "builtin-kind")
			idx := externalLifetimeIndexer(t, store)
			_, err := idx.Index(dir)
			require.NoError(t, err)
			require.NotNil(t, store.GetNode(builderRepoPrefix+"::builtin::go::append"), "fixture must materialize append")
			update := func(path string) *IndexResult {
				t.Helper()
				var result *IndexResult
				var err error
				if deferred {
					result, err = idx.IncrementalReindexPaths(dir, []string{path})
				} else {
					result, err = idx.incrementalReindexPathsMode(dir, []string{path}, incrementalPathMode{detectDeletions: true})
				}
				require.NoError(t, err)
				require.Empty(t, result.FailedFiles)
				return result
			}
			compare := func() {
				t.Helper()
				clean := builderOpenStore(t, "builtin-kind-clean")
				_, err := externalLifetimeIndexer(t, clean).Index(dir)
				require.NoError(t, err)
				externalLifetimeRows(t, "nodes", builderRenderNodes(store.AllNodes()), builderRenderNodes(clean.AllNodes()))
				externalLifetimeRows(t, "edges", builderRenderEdges(store.AllEdges()), builderRenderEdges(clean.AllEdges()))
			}
			grow := filepath.Join(dir, "grow.go")
			require.NoError(t, os.Remove(grow))
			require.Equal(t, 1, update(grow).DeletedFileCount)
			compare()
			use := filepath.Join(dir, "use.go")
			bumpMtime(t, use, "package fixture\n\nfunc Use() []int { return Keep(nil) }\n\nfunc Size(xs []int) int { return cap(xs) }\n")
			update(use)
			require.NotNil(t, store.GetNode(builderRepoPrefix+"::builtin::go::cap"), "the edit must materialize cap")
			compare()
		})
	}
}

package store_sqlite

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestResetCloneCorpusIsAtomic(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "marker_delete_failure"
		}
		t.Run(name, func(t *testing.T) {
			store, err := Open(filepath.Join(t.TempDir(), "clone.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			for _, prefix := range []string{"repo", "other"} {
				id := prefix + "/main.go::F"
				store.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, RepoPrefix: prefix})
				require.NoError(t, store.ReplaceCloneCorpus(prefix, []graph.CloneCorpusRow{{
					NodeID: id, Shingles: []uint64{1, 2}, Signature: "sig", Finalized: true,
				}}))
			}
			if fail {
				_, err := store.writerDB.Exec(`CREATE TRIGGER fail_clone_marker_delete
BEFORE DELETE ON clone_corpus_state BEGIN SELECT RAISE(ABORT, 'injected marker delete failure'); END`)
				require.NoError(t, err)
			}

			err = store.ResetCloneCorpus("repo")
			if fail {
				require.ErrorContains(t, err, "injected marker delete failure")
			} else {
				require.NoError(t, err)
			}
			for _, prefix := range []string{"repo", "other"} {
				wantPreserved := fail || prefix == "other"
				initialized, err := store.CloneCorpusInitialized(prefix)
				require.NoError(t, err)
				require.Equal(t, wantPreserved, initialized, prefix)
				rows, err := store.CloneCorpusPage(prefix, "", 10)
				require.NoError(t, err)
				node := store.GetNode(prefix + "/main.go::F")
				require.NotNil(t, node)
				if wantPreserved {
					require.Len(t, rows, 1)
					require.Equal(t, []uint64{1, 2}, rows[0].Shingles)
					require.Equal(t, "sig", node.Meta["clone_sig"])
				} else {
					require.Empty(t, rows)
					require.Empty(t, node.Meta["clone_sig"])
				}
			}
		})
	}
}

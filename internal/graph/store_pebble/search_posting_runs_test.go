package store_pebble

import (
	"encoding/binary"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestSearchPostingRunsReferenceDocuments(t *testing.T) {
	for _, tc := range []struct {
		name  string
		exact searchExactSource
	}{
		{"legacy", exactFromNodes},
		{"staged", exactFromDocuments},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			b := newBuild(t, FullProfile)
			for _, id := range []string{"d", "c", "b", "a"} {
				path := "repo/src/" + id + ".go"
				if id == "b" {
					path = "repo/src/nested/b.go"
				}
				b.AddNode(&graph.Node{ID: id, Name: id, Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: path})
			}
			require.NoError(t, b.UpsertSymbolFTS("a", "ab ab ac"))
			require.NoError(t, b.UpsertSymbolFTS("b", "ab z"))
			require.NoError(t, b.UpsertSymbolFTS("c", "..."))
			gold := sortedRuns{dir: t.TempDir(), name: "search", options: b.options, format: b.db.FormatMajorVersion().MaxTableFormat(), limit: goldBufferBytes}
			require.NoError(t, b.writeSymbols(ctx, &gold, tc.exact))

			// Read the actual scratch posting runs, before their owner removes them.
			paths, err := filepath.Glob(filepath.Join(gold.dir, "posts-*.sst"))
			require.NoError(t, err)
			require.NotEmpty(t, paths)
			posts := sortedRuns{options: b.options, paths: paths}
			var keys []string
			require.NoError(t, posts.merge(ctx, func(r row) error {
				require.Empty(t, r.value, "a scratch posting must not carry a copy of its document")
				keys = append(keys, string(r.key))
				return nil
			}))
			key := func(term, path string, id uint64) string {
				return string(binary.BigEndian.AppendUint64(searchKey("post", "repo", term, path), id))
			}
			require.Equal(t, []string{
				key("ab", "src/a.go", 1),
				key("ab", "src/nested/b.go", 2),
				key("ac", "src/a.go", 1),
				key("z", "src/nested/b.go", 2),
			}, keys)

			var docs []string
			require.NoError(t, gold.merge(ctx, func(r row) error {
				family, _, err := readKeyString(r.key[1:])
				if err != nil || family != "doc" {
					return err
				}
				doc, err := searchReadDoc(r.value)
				if err != nil {
					return err
				}
				docs = append(docs, doc.ID)
				return nil
			}))
			require.Equal(t, []string{"a", "b", "c"}, docs)
		})
	}
}

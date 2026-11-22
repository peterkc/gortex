package store_pebble

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestRebuildImportDirectoriesNative(t *testing.T) {
	for _, spillAt := range []int{1, 1 << 20} {
		t.Run(fmt.Sprint(spillAt), func(t *testing.T) {
			b := newBuild(t, FullProfile)
			native, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := native.Close(); err != nil {
					t.Error(err)
				}
			})
			file := func(id, p, repo, workspace, lang string) *graph.Node {
				return &graph.Node{Kind: graph.KindFile, ID: id, Name: id, FilePath: p, RepoPrefix: repo, WorkspaceID: workspace, Language: lang}
			}
			nodes := []*graph.Node{
				file("z-one", "a/pkg/shared.go", "a", "west", "go"),
				file("a-two", "a/pkg/shared.go", "a", "east", "rust"),
				file("zz-first", "a/pkg/0.go", "a", "west", "go"),
				file("b-middle", "z/pkg/shared.go", "z", "west", "python"),
				file("shallow", "pkg/entry.go", "pkg", "central", "go"),
				file("root", "root.cfg", "", "", "json"),
				file("empty-path", "", "", "", "yaml"),
				file("absolute-root", "/absolute.cfg", "", "", "json"),
				file("neighbor", "a/pkg2/entry.go", "a", "north", "go"),
				file("replace", "gone/pkg/dead.go", "z", "before", "cpp"),
				file("same-key", "z/pkg/same.go", "z", "before", "cpp"),
				{ID: "nonfile", Kind: graph.KindFunction, FilePath: "not-files/pkg/entry.go", RepoPrefix: "z"},
			}
			if spillAt == 1<<20 {
				// Vary payloads without changing directory/path ordering. This
				// exceeds a 1 MiB sort window with fewer than 128 files.
				for i := range 112 {
					id := fmt.Sprintf("spill-%03d", i)
					nodes = append(nodes, file(id, "large/spill/"+id, "large", id+strings.Repeat("w", 9<<10), "go"))
				}
			}
			b.AddBatch(nodes, nil)
			if err := native.AddBatchChecked(nodes, nil); err != nil {
				t.Fatal(err)
			}
			replacements := []*graph.Node{
				file("replace", "z/pkg/updated.go", "z", "after", "java"),
				file("same-key", "z/pkg/same.go", "z", "after", "go"),
			}
			b.AddBatch(replacements, nil)
			if err := native.AddBatchChecked(replacements, nil); err != nil {
				t.Fatal(err)
			}
			wantDirs, wantLast := nativeImportDirectoryBuckets(native)
			if len(wantDirs["pkg"]) != 1 || len(wantLast["pkg"]) != 6 || len(wantDirs["."]) != 2 || len(wantDirs["/"]) != 1 {
				t.Fatalf("native fixture did not produce directory tie, last-component and root witnesses")
			}
			wantRows := 18
			if spillAt == 1<<20 {
				wantRows += 224
			}
			lease := t.TempDir()
			stats, err := b.RebuildView().WithImportDirectories(t.Context(), lease, spillAt, func(index *RebuildImportDirectories) error {
				// Exact z/pkg precedes the independent last-component bucket even
				// though a/pkg is its first row. The reducer owns the tier choice.
				first := func(visit func(context.Context, string, func(graph.FileNodeIdentity) (bool, error)) error, key string, want graph.FileNodeIdentity) error {
					var seen int
					err := visit(t.Context(), key, func(file graph.FileNodeIdentity) (bool, error) {
						seen++
						if file != want {
							t.Errorf("first file in %q = %+v, want %+v", key, file, want)
						}
						return false, nil
					})
					if seen != 1 {
						t.Errorf("early stop in %q visited %d rows, want 1", key, seen)
					}
					return err
				}
				if err := first(index.VisitExactDirectory, "z/pkg", wantDirs["z/pkg"][0]); err != nil {
					return err
				}
				if err := first(index.VisitLastDirectoryComponent, "pkg", wantLast["pkg"][0]); err != nil {
					return err
				}
				// A later working row must not turn a frozen negative into a hit.
				b.AddNode(file("later", "missing/new.go", "", "", "go"))
				for key, want := range wantDirs {
					assertImportDirectoryFiles(t, index.VisitExactDirectory, key, want)
				}
				for key, want := range wantLast {
					assertImportDirectoryFiles(t, index.VisitLastDirectoryComponent, key, want)
				}
				for _, key := range []string{"", "missing", "gone/pkg", "not-files/pkg", "entry.go", "p"} {
					assertImportDirectoryFiles(t, index.VisitExactDirectory, key, nil)
					assertImportDirectoryFiles(t, index.VisitLastDirectoryComponent, key, nil)
				}
				for _, key := range []string{".", "/", "a/pkg"} {
					assertImportDirectoryFiles(t, index.VisitLastDirectoryComponent, key, nil)
				}
				return nil
			})
			if err != nil || stats.Files != len(nodes)-1 || stats.Rows != wantRows || stats.SpillBytes <= 0 {
				t.Fatalf("directory census = %+v, %v; want %d files and %d rows with spill", stats, err, len(nodes)-1, wantRows)
			}
			assertImportDirectoryCleanup(t, lease)
		})
	}
}

// Oracle: SQLite's native file projection and pinned resolver.go:2182–2199,
// resolve_all_pending.go:411–417. The oracle shares no ordering codec with the SST.
func nativeImportDirectoryBuckets(s graph.Store) (map[string][]graph.FileNodeIdentity, map[string][]graph.FileNodeIdentity) {
	exact := make(map[string][]graph.FileNodeIdentity)
	last := make(map[string][]graph.FileNodeIdentity)
	for file := range graph.FileNodeIdentitiesSeq(s, nil) {
		dir := path.Dir(filepath.ToSlash(file.FilePath))
		exact[dir] = append(exact[dir], file)
		parts := strings.Split(dir, "/")
		component := parts[len(parts)-1]
		if component != "" && component != dir {
			last[component] = append(last[component], file)
		}
	}
	for _, buckets := range []map[string][]graph.FileNodeIdentity{exact, last} {
		for _, files := range buckets {
			sort.SliceStable(files, func(i, j int) bool {
				if files[i].FilePath != files[j].FilePath {
					return files[i].FilePath < files[j].FilePath
				}
				return files[i].ID < files[j].ID
			})
		}
	}
	return exact, last
}

func assertImportDirectoryFiles(t *testing.T, visit func(context.Context, string, func(graph.FileNodeIdentity) (bool, error)) error, key string, want []graph.FileNodeIdentity) {
	t.Helper()
	var got []graph.FileNodeIdentity
	err := visit(t.Context(), key, func(file graph.FileNodeIdentity) (bool, error) {
		got = append(got, file)
		return true, nil
	})
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("bucket %q: got %d rows (%v), want %d equal ordered identities", key, len(got), err, len(want))
		for i := 0; i < min(len(got), len(want)); i++ {
			if got[i] != want[i] {
				t.Logf("first difference at %d: ID/path %q/%q vs %q/%q", i, got[i].ID, got[i].FilePath, want[i].ID, want[i].FilePath)
				break
			}
		}
	}
}

func TestRebuildImportDirectoriesRefusals(t *testing.T) {
	stop := errors.New("stop directory visit")
	for _, tc := range []struct {
		name  string
		cause error
	}{
		{"oversized-source", errInvalidRecord},
		{"oversized-identity", errInvalidRecord},
		{"canceled-visit", context.Canceled},
		{"visitor-failure", stop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			lease := t.TempDir()
			b.AddBatch([]*graph.Node{
				{ID: "a", Kind: graph.KindFile, FilePath: "nested/pkg/a"},
				{ID: "b", Kind: graph.KindFile, FilePath: "nested/pkg/b"},
			}, nil)
			if tc.name == "oversized-source" {
				// A saved value beyond the raw record cap follows valid rows and
				// must be refused before any decode, even outside KindFile.
				if err := b.pending.Set(pendingNodeKey("z"), make([]byte, linkRecordBytes), pebble.NoSync); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "oversized-identity" {
				b.AddNode(&graph.Node{ID: "z", Kind: graph.KindFile, FilePath: strings.Repeat("x", importDirRowBytes)})
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			visits := 0
			stats, err := b.RebuildView().WithImportDirectories(ctx, lease, 1, func(index *RebuildImportDirectories) error {
				if tc.cause == errInvalidRecord {
					t.Fatal("lent a partial directory index")
				}
				return index.VisitLastDirectoryComponent(ctx, "pkg", func(graph.FileNodeIdentity) (bool, error) {
					visits++
					if tc.cause == context.Canceled {
						cancel()
						return false, nil
					}
					return false, stop
				})
			})
			wantVisits := 1
			if tc.cause == errInvalidRecord {
				wantVisits = 0
			}
			if !errors.Is(err, ErrRebuildImportDirectories) || !errors.Is(err, tc.cause) || stats != (RebuildImportDirectoryStats{}) || visits != wantVisits {
				t.Fatalf("refusal = %+v, %v (%d visits), want %v (%d visits)", stats, err, visits, tc.cause, wantVisits)
			}
			assertImportDirectoryCleanup(t, lease)
		})
	}
}

func TestRebuildImportDirectoriesDiskBound(t *testing.T) {
	lease := t.TempDir()
	file := filepath.Join(lease, "cap.sst")
	remaining := int64(1)
	w, err := newImportDirectoryTable(t.Context(), file, &remaining)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(w.Set([]byte("key"), []byte("value")), w.Close()); !errors.Is(err, errInvalidRecord) {
		t.Fatalf("over-budget SST write = %v", err)
	}
	info, err := os.Stat(file)
	if err != nil || info.Size() > 1 {
		t.Fatalf("over-budget SST on disk = %v, %v", info, err)
	}
}

func assertImportDirectoryCleanup(t *testing.T, lease string) {
	t.Helper()
	entries, err := os.ReadDir(lease)
	if err != nil || len(entries) != 0 {
		t.Fatalf("directory census left scratch %v, %v", entries, err)
	}
}

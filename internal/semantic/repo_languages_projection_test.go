package semantic

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"go.uber.org/zap"
)

type managerProjectionStore struct {
	graph.Store
	calls    int
	prefixes []string
	rows     []graph.RepoLanguageFileCount
}

func (s *managerProjectionStore) RepoLanguageFileCounts(prefixes []string) []graph.RepoLanguageFileCount {
	s.calls++
	s.prefixes = append([]string(nil), prefixes...)
	return append([]graph.RepoLanguageFileCount(nil), s.rows...)
}

func (s *managerProjectionStore) RepoStats() map[string]graph.GraphStats {
	panic("repoLanguages must use the compact projection, not RepoStats")
}

func TestManagerRepoLanguagesUsesOneCompactProjection(t *testing.T) {
	store := &managerProjectionStore{
		Store: graph.New(),
		rows: []graph.RepoLanguageFileCount{
			{RepoPrefix: "a", FilePath: "src/main.go", Language: "go", Count: 3},
			{RepoPrefix: "a", FilePath: "generated/parser.c", Language: "c", Count: 10},
			{RepoPrefix: "b", FilePath: "src/main.py", Language: "python", Count: 2},
			{RepoPrefix: "other", FilePath: "src/lib.rs", Language: "rust", Count: 7},
		},
	}
	manager := &Manager{}
	present, repoCounts, languageCounts, err := manager.repoLanguages(store, map[string]string{
		"b": "/repos/b",
		"a": "/repos/a",
	})

	if err != nil {
		t.Fatal(err)
	}
	if store.calls != 1 {
		t.Fatalf("projection calls = %d, want 1", store.calls)
	}
	if got, want := store.prefixes, []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("projection prefixes = %#v, want %#v", got, want)
	}
	if got, want := present, map[string]bool{"go": true, "python": true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("present = %#v, want %#v", got, want)
	}
	if got, want := repoCounts, map[string]int{"a": 3, "b": 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("repo counts = %#v, want %#v", got, want)
	}
	if got, want := languageCounts, map[string]int{"go": 3, "python": 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("language counts = %#v, want %#v", got, want)
	}
}

func TestManagerRejectsAggregateOnlyLanguageCensus(t *testing.T) {
	for _, mode := range []string{"all", "checkout"} {
		for _, supported := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/exact=%t", mode, supported), func(t *testing.T) {
				g := graph.New()
				g.AddBatch([]*graph.Node{
					{ID: "r/main.go::F", Kind: graph.KindFunction, RepoPrefix: "r", FilePath: "r/main.go", Language: "go"},
					{ID: "r/parser.c::P", Kind: graph.KindFunction, RepoPrefix: "r", FilePath: "r/generated/parser.c", Language: "c"},
					{ID: "r/testdata/a.ts::F", Kind: graph.KindFunction, RepoPrefix: "r", FilePath: "r/testdata/a.ts", Language: "typescript"},
				}, nil)
				manager := NewManager(Config{Enabled: true}, zap.NewNop())
				t.Cleanup(func() { _ = manager.Close() })
				var goCalls, cCalls atomic.Int32
				manager.RegisterProvider(&mockProvider{name: "go", languages: []string{"go"}, available: true, enrichFunc: func(graph.Store, string) (*EnrichResult, error) {
					goCalls.Add(1)
					return &EnrichResult{Language: "go"}, nil
				}})
				manager.RegisterProvider(&mockProvider{name: "c", languages: []string{"c"}, available: true, enrichFunc: func(graph.Store, string) (*EnrichResult, error) {
					cCalls.Add(1)
					return &EnrichResult{Language: "c"}, nil
				}})
				var store graph.Store = g
				if !supported {
					store = struct{ graph.Store }{g}
				}
				root := t.TempDir()
				// Given file-aware exclusions matter, aggregate totals cannot select or spawn providers.
				var err error
				if mode == "all" {
					_, _, err = manager.EnrichAll(store, map[string]string{"r": root}, EnrichOptions{})
				} else {
					_, err = manager.EnrichCheckoutContext(context.Background(), store, CheckoutEnrichRequest{RepoPrefix: "r", Root: root, CheckoutID: "checkout"})
				}
				if supported {
					require.NoError(t, err)
					require.Equal(t, int32(1), goCalls.Load())
					require.Zero(t, cCalls.Load())
				} else {
					require.ErrorIs(t, err, errors.ErrUnsupported)
					require.Zero(t, goCalls.Load())
					require.Zero(t, cCalls.Load())
					require.Empty(t, manager.CheckoutWorkspaces().Live(), "refuse before reserving any language workspace")
				}
			})
		}
	}
}

func TestManagerCensusPathlessRowsAreNotFiles(t *testing.T) {
	exact := &managerProjectionStore{Store: graph.New(), rows: []graph.RepoLanguageFileCount{
		{RepoPrefix: "r", FilePath: "r/main.go", Language: "go", Count: 2},
		{RepoPrefix: "r", Language: "c", Count: 30}, {RepoPrefix: "r", Language: "c", Count: 40},
	}}
	// Given an exact provider also projects synthetic rows, no filename is invented for them.
	present, counts, languages, err := (&Manager{}).repoLanguages(exact, map[string]string{"r": "unused"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(present, map[string]bool{"go": true}) || !reflect.DeepEqual(counts, map[string]int{"r": 2}) || !reflect.DeepEqual(languages, map[string]int{"go": 2}) {
		t.Fatalf("pathless rows influenced source census: %v/%v/%v", present, counts, languages)
	}
}

func TestManagerRepoCensusNativeFileIdentity(t *testing.T) {
	for _, backend := range []string{"graph", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store graph.Store = graph.New()
			if backend == "sqlite" {
				disk, err := store_sqlite.Open(filepath.Join(t.TempDir(), "semantic-census.sqlite"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, disk.Close()) })
				store = disk
			}
			store.AddBatch([]*graph.Node{
				{ID: "g1", Kind: graph.KindFunction, RepoPrefix: "r", FilePath: "r/main.go", Language: "go"},
				{ID: "g2", Kind: graph.KindFunction, RepoPrefix: "r", FilePath: "r/main.go", Language: "go"},
				{ID: "py", Kind: graph.KindFunction, RepoPrefix: "r", FilePath: "r/main.py", Language: "python"},
				{ID: "c", Kind: graph.KindFunction, RepoPrefix: "r", FilePath: "r/generated/parser.c", Language: "c"},
				{ID: "ts", Kind: graph.KindFunction, RepoPrefix: "r", FilePath: "r/testdata/case.ts", Language: "typescript"},
				{ID: "synthetic", Kind: graph.KindFunction, RepoPrefix: "r", Language: "cpp"},
				{ID: "module", Kind: graph.KindModule, RepoPrefix: "r", FilePath: "r/go.mod", Language: "go"},
				{ID: "other", Kind: graph.KindFunction, RepoPrefix: "other", FilePath: "other/main.rs", Language: "rust"},
			}, nil)
			// Given native exact rows, only tracked non-generated source files determine composition.
			for range 2 {
				present, counts, languages, err := (&Manager{}).repoLanguages(store, map[string]string{"r": "unused"})
				require.NoError(t, err)
				require.Equal(t, map[string]bool{"go": true, "python": true}, present)
				require.Equal(t, map[string]int{"r": 3}, counts)
				require.Equal(t, map[string]int{"go": 2, "python": 1}, languages)
			}
		})
	}
	// An empty requested set has no proof to require and must not fall through to RepoStats.
	exact := &managerProjectionStore{Store: graph.New()}
	present, counts, languages, err := (&Manager{}).repoLanguages(struct{ graph.Store }{exact}, nil)
	require.NoError(t, err)
	require.Empty(t, present)
	require.Empty(t, counts)
	require.Empty(t, languages)
	require.Zero(t, exact.calls)
}

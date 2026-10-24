package resolver

import (
	"fmt"
	"iter"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type importLanguageCountingStore struct {
	graph.Store
	languageWalks atomic.Int64
	summaryRepos  [][]string
	summaryRows   int
}

func (s *importLanguageCountingStore) FileLanguageNodesSeq() iter.Seq[graph.FileLanguageNode] {
	s.languageWalks.Add(1)
	return graph.FileLanguageNodesSeq(s.Store)
}

func (s *importLanguageCountingStore) NodesLightByKindsInScopeSeq(repos, paths []string, kinds ...graph.NodeKind) iter.Seq[*graph.Node] {
	s.summaryRepos = append(s.summaryRepos, append([]string(nil), repos...))
	return func(yield func(*graph.Node) bool) {
		for node := range graph.NodesLightByKindsInScopeSeq(s.Store, repos, paths, kinds...) {
			s.summaryRows++
			if !yield(node) {
				return
			}
		}
	}
}

func TestResolveAllScopedImportLanguagesDoNotWalkPerImport(t *testing.T) {
	const files, imports = 4000, 400
	g := graph.New()
	for i := 0; i < files; i++ {
		file := fmt.Sprintf("repo/pkg%d/file.go", i)
		g.AddNode(&graph.Node{ID: file, Kind: graph.KindFile, FilePath: file, Language: "go", RepoPrefix: "repo"})
	}
	for i := 0; i < imports; i++ {
		file := fmt.Sprintf("repo/app%d/main.go", i)
		g.AddNode(&graph.Node{ID: file, Kind: graph.KindFile, FilePath: file, Language: "go", RepoPrefix: "repo"})
		g.AddEdge(&graph.Edge{From: file, To: fmt.Sprintf("unresolved::import::example.com/repo/pkg%d", i), Kind: graph.EdgeImports, FilePath: file, Line: 3})
	}
	g.AddNode(&graph.Node{ID: "other/ignored.go", Kind: graph.KindFile, FilePath: "other/ignored.go", Language: "go", RepoPrefix: "other"})
	store := &importLanguageCountingStore{Store: g}
	r := New(store)
	r.SetScope(map[string]struct{}{"repo": {}})
	r.SetWorkspaceMembership(func(string) string { return "" })
	stats := r.ResolveAll()
	t.Logf("imports=%d languageWalks=%d scopedFileRows=%d", imports, store.languageWalks.Load(), store.summaryRows)
	if stats.Resolved != imports {
		t.Fatalf("resolved %d imports, want %d", stats.Resolved, imports)
	}
	for i := 0; i < imports; i++ {
		edges := g.GetOutEdges(fmt.Sprintf("repo/app%d/main.go", i))
		want := fmt.Sprintf("repo/pkg%d/file.go", i)
		if len(edges) != 1 || edges[0].To != want {
			t.Fatalf("import %d: edges = %#v, want target %q", i, edges, want)
		}
	}
	// Attribution passes still walk languages a constant number of times.
	// Import resolution must not add one whole-store walk per import.
	if got := store.languageWalks.Load(); got > 5 {
		t.Fatalf("whole-store language walks = %d for %d imports, want at most 5", got, imports)
	}
	if store.summaryRows != files+imports || !reflect.DeepEqual(store.summaryRepos, [][]string{{"repo"}}) {
		t.Fatalf("scoped language reads = %v / %d rows, want repo only / %d rows", store.summaryRepos, store.summaryRows, files+imports)
	}
	if r.importFileLanguages != nil || r.dirIndex != nil || r.lastDirIndex != nil {
		t.Fatal("ResolveAll retained directory or language indexes after the pass")
	}
}

func TestResolveAllPendingImportLanguagesFollowDirectoryPrefixes(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "a/file.go", Kind: graph.KindFile, FilePath: "a/file.go", Language: "go", RepoPrefix: "a"},
		{ID: "b/file.py", Kind: graph.KindFile, FilePath: "b/file.py", Language: "python", RepoPrefix: "b"},
		{ID: "other/file.ts", Kind: graph.KindFile, FilePath: "other/file.ts", Language: "typescript", RepoPrefix: "other"},
	}, nil)
	store := &importLanguageCountingStore{Store: g}
	r := New(store)
	p := newPendingFrontierPassIndexes(r)
	defer p.close()
	p.ensureDir([]string{"a"})
	p.ensureDir([]string{"a", "b"})
	p.ensureDir([]string{"b"})
	want := map[string]string{"a/file.go": "go", "b/file.py": "python"}
	if !reflect.DeepEqual(r.importFileLanguages, want) {
		t.Fatalf("file languages = %v, want %v", r.importFileLanguages, want)
	}
	if store.summaryRows != 2 || !reflect.DeepEqual(store.summaryRepos, [][]string{{"a"}, {"b"}}) || store.languageWalks.Load() != 0 {
		t.Fatalf("language reads: scopes=%v rows=%d global=%d, want a/b once each and no global walk", store.summaryRepos, store.summaryRows, store.languageWalks.Load())
	}
	p.close()
	if r.importFileLanguages != nil || r.dirIndex != nil || r.lastDirIndex != nil {
		t.Fatal("closing pending pass retained directory or language indexes")
	}
	g.AddNode(&graph.Node{ID: "a/file.go", Kind: graph.KindFile, FilePath: "a/file.go", Language: "rust", RepoPrefix: "a"})
	p = newPendingFrontierPassIndexes(r)
	defer p.close()
	p.ensureDir([]string{"a"})
	if got := r.importFileLanguages["a/file.go"]; got != "rust" || len(r.importFileLanguages) != 1 {
		t.Fatalf("new pass languages = %v, want only updated a/file.go", r.importFileLanguages)
	}
}

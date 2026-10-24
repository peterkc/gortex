package indexer

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
)

func TestImportPlacementRepoRootCarriesMatchingNestedFolder(t *testing.T) {
	g := graph.New()
	for _, file := range []string{"repo/app/main.ts", "repo/index.ts", "repo/repo/index.ts"} {
		g.AddNode(&graph.Node{
			ID: file, Kind: graph.KindFile, FilePath: file,
			Language: "typescript", RepoPrefix: "repo",
		})
	}
	edge := &graph.Edge{
		From: "repo/app/main.ts", To: "unresolved::import::repo",
		Kind: graph.EdgeImports, FilePath: "repo/app/main.ts", Line: 1,
	}
	g.AddEdge(edge)
	resolver.New(g).ResolveAll()
	if edge.To != "repo/repo/index.ts" {
		t.Fatalf("import bound to %q, want matching nested folder", edge.To)
	}

	walk := &closureWalk{req: BuildRequest{Base: g, RepoPrefix: "repo"}}
	assertImportPlacement(t, walk, "repo", []string{"app/main.ts"}, []string{"repo/index.ts", "repo/repo/index.ts"})
}

// A nested Go module can own the prefix-named package even with a file at the
// outer repository root. Certified ownership must keep that valid binding.
func TestImportPlacementRepoRootCertifiedNested(t *testing.T) {
	g := graph.New()
	for _, file := range []string{"repo/app/main.go", "repo/root.go", "repo/repo/pkg.go"} {
		g.AddNode(&graph.Node{
			ID: file, Kind: graph.KindFile, FilePath: file,
			Language: "go", RepoPrefix: "repo",
		})
	}
	edge := &graph.Edge{From: "repo/app/main.go", To: "unresolved::import::repo",
		Kind: graph.EdgeImports, FilePath: "repo/app/main.go", Line: 3}
	g.AddEdge(edge)
	r := resolver.New(g)
	r.SetGoPackageOwnership(func(c resolver.GoImportCandidate) resolver.GoPackageOwnershipResult {
		if c.CandidateFilePath == "repo/repo/pkg.go" {
			return resolver.GoPackageOwnershipExact
		}
		return resolver.GoPackageOwnershipDifferent
	})
	r.ResolveAll()
	if edge.To != "repo/repo/pkg.go" {
		t.Fatalf("certified import bound to %q, want nested package", edge.To)
	}
	walk := &closureWalk{req: BuildRequest{Base: g, RepoPrefix: "repo"}}
	assertImportPlacement(t, walk, "repo", []string{"app/main.go"},
		[]string{"repo/root.go", "repo/repo/pkg.go"})
}

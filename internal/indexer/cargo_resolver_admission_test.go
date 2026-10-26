package indexer

import (
	"iter"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"go.uber.org/zap"
)

type cargoAdmissionStore struct {
	graph.Store
	scans int
}

func (s *cargoAdmissionStore) EdgesLightSeq(kinds ...graph.EdgeKind) iter.Seq[*graph.Edge] {
	return func(yield func(*graph.Edge) bool) {
		if len(kinds) == 1 && kinds[0] == graph.EdgeImports {
			s.scans++
		}
		graph.EdgesLightSeq(s.Store, kinds...)(yield)
	}
}

func TestCargoResolverSingleRootAdmission(t *testing.T) {
	for _, withCall := range []bool{false, true} {
		name := "imports_only"
		if withCall {
			name = "cross_repo_call"
		}
		t.Run(name, func(t *testing.T) {
			first, second := t.TempDir(), t.TempDir()
			writeWorkspaceFile(t, first, "Cargo.toml", "[package]\nname='app'\n")
			g := &cargoAdmissionStore{Store: graph.New()}
			file, target := "first/src/lib.rs", "second/src/lib.rs"
			for _, n := range []*graph.Node{
				{ID: file, FilePath: file, Kind: graph.KindFile, Language: "rust", RepoPrefix: "first", WorkspaceID: "shared"},
				{ID: target, FilePath: target, Kind: graph.KindFile, Language: "rust", RepoPrefix: "second", WorkspaceID: "shared"},
				{ID: target + "::Run", FilePath: target, Name: "Run", Kind: graph.KindFunction, Language: "rust", RepoPrefix: "second", WorkspaceID: "shared"},
			} {
				g.AddNode(n)
			}
			g.AddEdge(&graph.Edge{From: file, To: target, FilePath: file, Kind: graph.EdgeImports, Line: 1, CrossRepo: true})
			if withCall {
				g.AddEdge(&graph.Edge{From: file, To: "unresolved::Run", FilePath: file, Kind: graph.EdgeCalls, Line: 2})
			}
			mi := &MultiIndexer{graph: g, logger: zap.NewNop(), repos: map[string]*RepoMetadata{"first": {RootPath: first}, "second": {RootPath: second}}}
			mi.newCrossRepoResolver().ResolveAll()
			wantScans := 2 // Existing npm and Python import discovery on the minimal Store wrapper.
			if withCall {
				wantScans++
			} // Reachability for the pending call.
			require.Equal(t, wantScans, g.scans, "Cargo must not consume another import scan below two roots")
			edges := g.GetOutEdges(file)
			require.Contains(t, edges, &graph.Edge{From: file, To: target, FilePath: file, Kind: graph.EdgeImports, Line: 1, CrossRepo: true})
			if withCall {
				require.Contains(t, edges, &graph.Edge{From: file, To: target + "::Run", FilePath: file, Kind: graph.EdgeCalls, Line: 2, CrossRepo: true})
			}
		})
	}
}

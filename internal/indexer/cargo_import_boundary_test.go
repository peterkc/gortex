package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
)

func TestCargoCrateLookupUsesInstalledManifestTree(t *testing.T) {
	for _, workspace := range []bool{false, true} {
		t.Run(map[bool]string{false: "root", true: "workspace"}[workspace], func(t *testing.T) {
			provider, consumer, providerSnapshot, consumerSnapshot := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
			writeWorkspaceFile(t, provider, "Cargo.toml", "[package]\nname = 'wrong-checkout'\n")
			writeWorkspaceFile(t, consumer, "Cargo.toml", "[package]\nname = 'wrong-app'\n")
			providerDir, consumerDir := "", ""
			dependency := "[dependencies]\nalias = { package = 'other-crate', version = '1' }\n"
			if workspace {
				providerDir, consumerDir = "crates/engine/", "apps/app/"
				writeWorkspaceFile(t, providerSnapshot, "Cargo.toml", "[workspace]\nmembers = ['crates/*']\n")
				writeWorkspaceFile(t, consumerSnapshot, "Cargo.toml", "[workspace]\nmembers = ['apps/*']\nexclude = ['apps/excluded']\n[workspace.dependencies]\nalias = { package = 'other-crate', version = '1' }\n")
				writeWorkspaceFile(t, consumerSnapshot, "apps/excluded/Cargo.toml", "[package]\nname = 'excluded'\n")
				dependency = "[dependencies]\nalias = { workspace = true }\n"
			}
			writeWorkspaceFile(t, providerSnapshot, providerDir+"Cargo.toml", "[package]\nname = 'other-crate'\n")
			writeWorkspaceFile(t, consumerSnapshot, consumerDir+"Cargo.toml", "[package]\nname = 'app'\n"+dependency)
			g := graph.New()
			providerIdx, consumerIdx := sideChannelIndexer(t, g, provider), sideChannelIndexer(t, g, consumer)
			providerIdx.SetContentSource(snapshotOf(t, providerSnapshot))
			consumerIdx.SetContentSource(snapshotOf(t, consumerSnapshot))
			mi := &MultiIndexer{graph: g,
				repos:    map[string]*RepoMetadata{"provider": {RootPath: provider}, "consumer": {RootPath: consumer}},
				indexers: map[string]*Indexer{"provider": providerIdx, "consumer": consumerIdx}}
			lookup := mi.cargoCrateLookup()
			require.NotNil(t, lookup)
			require.Equal(t, []resolver.CargoCrate{{Root: "provider/" + providerDir + "src/lib.rs", Name: "other_crate"}}, lookup("consumer/"+consumerDir+"src/lib.rs", "alias"))
			if workspace {
				require.Empty(t, lookup("consumer/apps/excluded/src/lib.rs", "alias"))
			}
		})
	}
}

func TestCargoRestubIgnoresMetadataOnlyTargets(t *testing.T) {
	g := graph.New()
	target := "provider/src/meta.rs"
	edge := &graph.Edge{From: "consumer/src/lib.rs", FilePath: "consumer/src/lib.rs", To: target, Kind: graph.EdgeImports, CrossRepo: true,
		Meta: map[string]any{resolver.CargoImportStubMeta: "external::other_crate/meta"}}
	g.AddNode(&graph.Node{ID: target, Kind: graph.KindFile, RepoPrefix: "provider"})
	g.AddEdge(edge)
	incoming := g.GetInEdgesByNodeIDs([]string{target})
	require.Empty(t, restubTrackedCargoImportsFromView(g, map[string]struct{}{"provider/src/changed.rs": {}}, incoming, false))
	require.Equal(t, target, edge.To)
	require.True(t, edge.CrossRepo)
}

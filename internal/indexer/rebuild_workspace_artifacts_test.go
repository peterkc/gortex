package indexer

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"go.uber.org/zap"
)

func TestRebuildWorkspaceArtifactsNativeOrderAndFrozenReplay(t *testing.T) {
	const (
		rootID = "pkgws::npm:."
		a      = "packages/a/package.json"
		b      = "packages/b/package.json"
	)
	for _, tc := range []struct{ name, first, last string }{
		{"alias_last", "packages/a", "packages/./a"},
		{"literal_last", "packages/./a", "packages/a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, src := rebuildInputFixture(t)
			writeRebuildTSAliasFixtureFile(t, root, "package.json",
				fmt.Sprintf(`{"name":"root","workspaces":[%q,"packages/b",%q]}`, tc.first, tc.last))
			writeRebuildTSAliasFixtureFile(t, root, a, `{"name":"a","version":"1.0.0"}`)
			writeRebuildTSAliasFixtureFile(t, root, b, `{"name":"b"}`)
			seed := []*graph.Node{{ID: a, Kind: graph.KindFile, Name: "package.json", FilePath: a, Language: "json",
				StartLine: 1, EndLine: 1, Meta: map[string]any{
					"content_hash": "5bbc06fcf2fe3ce8d231db85e9a3065557f81b5d5b52b116e5b79a533396b915",
					"size_bytes":   30,
				}}}
			native := rebuildNativeWorkspaceGraph(root, seed)
			// Two spellings count as two members but upsert one edge key.
			require.Len(t, native.AllNodes(), 3)
			require.Equal(t, &graph.Node{ID: rootID, Kind: graph.KindPackage, Name: "workspace:npm",
				FilePath: "package.json", Language: "json", Meta: map[string]any{
					"package_workspace": true, "ecosystem": "npm", "manifest": "package.json", "member_count": 3,
				}}, native.GetNode(rootID))
			for _, member := range []string{a, b} {
				require.Equal(t, &graph.Node{ID: member, Kind: graph.KindFile, Name: "package.json", FilePath: member,
					Language: "json"}, native.GetNode(member))
			}
			require.ElementsMatch(t, []*graph.Edge{
				{From: rootID, To: a, Kind: graph.EdgePackageWorkspaceMember, FilePath: "package.json",
					Origin: graph.OriginASTResolved, Meta: map[string]any{"ecosystem": "npm", "member_dir": tc.last}},
				{From: rootID, To: b, Kind: graph.EdgePackageWorkspaceMember, FilePath: "package.json",
					Origin: graph.OriginASTResolved, Meta: map[string]any{"ecosystem": "npm", "member_dir": "packages/b"}},
			}, native.AllEdges())

			withRebuildModuleTestInputs(t, src, "", func(lease string, digest rebuildDigest, _ []rebuildInput) {
				// Drop the alias from the declaration and remove a different
				// member: either live read would change frozen replay.
				writeRebuildTSAliasFixtureFile(t, root, "package.json", `{"name":"root","workspaces":["packages/a"]}`)
				require.NoError(t, os.Remove(filepath.Join(root, b)))
				cold := rebuildNativeWorkspaceGraph(root, nil)
				require.Len(t, cold.AllEdges(), 1)
				require.Nil(t, cold.GetNode(b))
				build := openRebuildModuleTestStore(t, seed)
				before, err := build.RebuildView().Node(t.Context(), a)
				require.NoError(t, err)
				require.Equal(t, seed[0], before)
				require.NoError(t, runRebuildPackageWorkspace(t.Context(), lease, digest, build))
				requireRebuildModuleGraph(t, native, build.RebuildView())
			})
		})
	}
}

func TestRebuildWorkspaceArtifactsMetadataOnlyPnpm(t *testing.T) {
	const rootID, member = "pkgws::pnpm:.", "packages/a/package.json"
	for _, tc := range []struct{ name, npm string }{
		{"pnpm", `{"name":"root"}`},
		{"npm_no_members_precedes_pnpm", `{"name":"root","workspaces":["empty"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, src := rebuildInputFixture(t)
			writeRebuildTSAliasFixtureFile(t, root, "package.json", tc.npm)
			writeRebuildTSAliasFixtureFile(t, root, "pnpm-workspace.yaml", "packages: ['packages/*']\n")
			writeRebuildTSAliasFixtureFile(t, root, member, `{"name":"a"}`)
			require.NoError(t, os.Mkdir(filepath.Join(root, "empty"), 0o700))
			native := rebuildNativeWorkspaceGraph(root, nil)
			if tc.name == "pnpm" {
				require.Len(t, native.AllNodes(), 2)
				require.Equal(t, &graph.Node{ID: rootID, Kind: graph.KindPackage, Name: "workspace:pnpm",
					FilePath: "pnpm-workspace.yaml", Language: "yaml", Meta: map[string]any{
						"package_workspace": true, "ecosystem": "pnpm", "manifest": "pnpm-workspace.yaml", "member_count": 1,
					}}, native.GetNode(rootID))
				require.Equal(t, &graph.Node{ID: member, Kind: graph.KindFile, Name: "package.json", FilePath: member,
					Language: "json"}, native.GetNode(member))
				require.Equal(t, []*graph.Edge{{From: rootID, To: member, Kind: graph.EdgePackageWorkspaceMember,
					FilePath: "pnpm-workspace.yaml", Origin: graph.OriginASTResolved,
					Meta: map[string]any{"ecosystem": "pnpm", "member_dir": "packages/a"}}}, native.AllEdges())
			} else {
				require.Empty(t, native.AllNodes())
				require.Empty(t, native.AllEdges())
			}
			withRebuildModuleTestInputs(t, src, "", func(lease string, digest rebuildDigest, inputs []rebuildInput) {
				for _, in := range inputs {
					require.NotContains(t, []string{"go", "javascript", "typescript"}, in.Language)
				}
				moved := filepath.Join(t.TempDir(), "relocated[1]")
				require.NoError(t, os.Rename(root, moved))
				_, err := os.Stat(root)
				require.ErrorIs(t, err, os.ErrNotExist)
				build := openRebuildModuleTestStore(t, nil)
				require.NoError(t, runRebuildPackageWorkspace(t.Context(), lease, digest, build))
				requireRebuildModuleGraph(t, native, build.RebuildView())
			})
		})
	}
}

func TestRebuildWorkspaceArtifactsRefusalLeavesGraphUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name        string
		files       map[string]string
		wantTargets []string
		wantErr     error
	}{
		{"denied_manifest", map[string]string{
			"package.json":             `{"workspaces":["packages/a","packages/b"]}`,
			"packages/a/package.json":  `{"name":"a"}`,
			"packages/b/package.json":  `{"name":"b"}`,
			"packages/b/.gortexignore": "package.json\n",
		}, []string{"packages/a/package.json", "packages/b/package.json"}, errRebuildResolverInputDenied},
		{"cargo_with_real_member", map[string]string{
			"Cargo.toml":          "[workspace]\nmembers = [\"crates/a\"]\n",
			"crates/a/Cargo.toml": "[package]\nname = \"a\"\nversion = \"0.1.0\"\n",
		}, []string{"crates/a/Cargo.toml"}, errRebuildWorkspaceOutput},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, src := rebuildInputFixture(t)
			for file, body := range tc.files {
				writeRebuildTSAliasFixtureFile(t, root, file, body)
			}
			native := rebuildNativeWorkspaceGraph(root, nil)
			var targets []string
			for _, edge := range native.AllEdges() {
				require.Equal(t, graph.EdgePackageWorkspaceMember, edge.Kind)
				targets = append(targets, edge.To)
			}
			require.ElementsMatch(t, tc.wantTargets, targets)
			withRebuildModuleTestInputs(t, src, "", func(lease string, digest rebuildDigest, _ []rebuildInput) {
				build := openRebuildModuleTestStore(t, nil)
				err := runRebuildPackageWorkspace(t.Context(), lease, digest, build)
				require.ErrorIs(t, err, tc.wantErr)
				if tc.wantErr == errRebuildResolverInputDenied {
					require.ErrorContains(t, err, "packages/b/package.json")
				}
				requireRebuildModuleGraph(t, graph.New(), build.RebuildView())
			})
		})
	}
}

// Use the pinned workspace pass on Graph, not the frozen builder or SQLite.
func rebuildNativeWorkspaceGraph(root string, seed []*graph.Node) *graph.Graph {
	g := graph.New()
	g.AddBatch(seed, nil)
	idx := New(g, parser.NewRegistry(), config.Default().Index, zap.NewNop())
	defer idx.Close()
	idx.SetRootPath(root)
	idx.extractPackageWorkspace()
	return g
}

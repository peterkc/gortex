package indexer

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

func TestRebuildModulesNativeOrderAndFrozenReplay(t *testing.T) {
	const goMod = "module example.org/root\nrequire (\n\texample.org/lib %s%s\n\texample.org/lib %s%s\n)\nreplace example.org/lib => ./local\n"
	const goImport, blockImport, sharedImport = "main.go::lib", "main.ts::block", "main.ts::shared"
	imports := []*graph.Node{
		{ID: goImport, Kind: graph.KindImport, FilePath: "main.go", Language: "go", StartLine: 2, Meta: map[string]any{"path": "example.org/lib/sub"}},
		{ID: blockImport, Kind: graph.KindImport, FilePath: "main.ts", Language: "typescript", StartLine: 1, Meta: map[string]any{"path": "block/sub"}},
		{ID: sharedImport, Kind: graph.KindImport, FilePath: "main.ts", Language: "typescript", StartLine: 2, Meta: map[string]any{"path": "shared"}},
	}
	for _, tc := range []struct {
		name, first, firstComment, second, secondComment string
	}{
		{"original_require_order", "v1.2.0", " // indirect", "v1.1.0", ""},
		{"reversed_require_order", "v1.1.0", "", "v1.2.0", " // indirect"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, src := rebuildInputFixture(t)
			for file, content := range map[string]string{
				"go.mod": fmt.Sprintf(goMod, tc.first, tc.firstComment, tc.second, tc.secondComment),
				"package.json": `{"name":"root","dependencies":{"block":"1.0.0"},
  "devDependencies":{"block":"1.0.0"},"optionalDependencies":{"block":"9.0.0"}}`,
				"package-lock.json": `{"lockfileVersion":3,"packages":{
  "node_modules/shared":{"version":"4.0.0","dev":true},"node_modules/npm-only":{"version":"2.0.0","optional":true}}}`,
				"yarn.lock":             "shared@^4:\n  version \"4.0.0\"\n\nyarn-only@^3:\n  version \"3.0.0\"\n",
				"pnpm-lock.yaml":        "packages:\n  /shared@4.0.0:\n  /pnpm-only@5.0.0:\n",
				"nested/pnpm-lock.yaml": "packages:\n  /nested-only@6.0.0:\n",
				"main.go":               "package root\nimport \"example.org/lib/sub\"\n",
				"main.ts":               "import 'block/sub'\nimport 'shared'\n",
			} {
				writeRebuildTSAliasFixtureFile(t, root, file, content)
			}
			native := rebuildNativeModuleGraph(root, imports)
			// Ensure the oracle contains all five manifests, eight unique modules,
			// and all manifest and import edges after native deduplication.
			require.Len(t, native.AllNodes(), len(imports)+5+8)
			require.Len(t, native.AllEdges(), 13)
			require.Nil(t, native.GetNode("nested/pnpm-lock.yaml"))
			require.Nil(t, native.GetNode("module::npm:nested-only@6.0.0"))
			first, second := "module::go:example.org/lib@"+tc.first, "module::go:example.org/lib@"+tc.second
			firstNode := native.GetNode(first)
			require.NotNil(t, firstNode)
			require.Equal(t, map[string]any{
				"ecosystem": "go", "path": "example.org/lib", "version": tc.first,
				"indirect": tc.firstComment != "", "replace": "./local",
			}, firstNode.Meta)
			require.Contains(t, native.AllEdges(), &graph.Edge{From: "go.mod", To: first, Kind: graph.EdgeDependsOnModule,
				FilePath: "go.mod", Line: 3, Origin: graph.OriginASTResolved})
			require.Contains(t, native.AllEdges(), &graph.Edge{From: "go.mod", To: second, Kind: graph.EdgeDependsOnModule,
				FilePath: "go.mod", Line: 4, Origin: graph.OriginASTResolved})
			const block, shared = "module::npm:block@1.0.0", "module::npm:shared@4.0.0"
			for _, n := range []*graph.Node{
				{ID: block, Kind: graph.KindModule, Name: "block", FilePath: "package.json", Language: "javascript",
					Meta: map[string]any{"ecosystem": "npm", "path": "block", "version": "1.0.0", "indirect": false}},
				{ID: shared, Kind: graph.KindModule, Name: "shared", FilePath: "pnpm-lock.yaml", Language: "javascript",
					Meta: map[string]any{"ecosystem": "npm", "path": "shared", "version": "4.0.0", "indirect": false}},
			} {
				require.Equal(t, n, native.GetNode(n.ID))
			}
			require.Equal(t, []string{first}, rebuildModuleTestTargets(native, goImport))
			require.Equal(t, []string{block}, rebuildModuleTestTargets(native, blockImport))
			require.Equal(t, []string{shared}, rebuildModuleTestTargets(native, sharedImport))

			withRebuildModuleTestInputs(t, src, "", func(lease string, digest rebuildDigest, inputs []rebuildInput) {
				// A fresh native run must observe both the edit and the deletion.
				writeRebuildTSAliasFixtureFile(t, root, "go.mod", "module example.org/root\nrequire example.org/lib v1.9.0\n")
				require.NoError(t, os.Remove(filepath.Join(root, "package-lock.json")))
				edited := rebuildNativeModuleGraph(root, imports)
				require.Equal(t, []string{"module::go:example.org/lib@v1.9.0"}, rebuildModuleTestTargets(edited, goImport))
				require.Nil(t, edited.GetNode("package-lock.json"))
				require.NotNil(t, native.GetNode("package-lock.json"))

				build := openRebuildModuleTestStore(t, imports)
				require.NoError(t, runRebuildModules(t.Context(), lease, digest, build, inputs))
				requireRebuildModuleGraph(t, native, build.RebuildView())
			})
		})
	}
}

func TestRebuildModulesEmptyAndMissingFact(t *testing.T) {
	for _, tc := range []struct{ name, omit string }{
		{"present_empty_and_absent", ""},
		{"missing_present_empty_row", "yarn.lock"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, src := rebuildInputFixture(t)
			writeRebuildTSAliasFixtureFile(t, root, "yarn.lock", "# No dependencies\n")
			writeRebuildTSAliasFixtureFile(t, root, "nested/pnpm-lock.yaml", "packages:\n  /nested-only@6.0.0:\n")
			native := rebuildNativeModuleGraph(root, nil)
			require.Equal(t, []*graph.Node{{ID: "yarn.lock", Kind: graph.KindFile, Name: "yarn.lock",
				FilePath: "yarn.lock", Language: "yarn"}}, native.AllNodes())
			require.Empty(t, native.AllEdges())
			withRebuildModuleTestInputs(t, src, tc.omit, func(lease string, digest rebuildDigest, inputs []rebuildInput) {
				build := openRebuildModuleTestStore(t, nil)
				err := runRebuildModules(t.Context(), lease, digest, build, inputs)
				if tc.omit != "" {
					require.ErrorIs(t, err, errRebuildModuleInput)
					requireRebuildModuleGraph(t, graph.New(), build.RebuildView())
					return
				}
				require.NoError(t, err)
				requireRebuildModuleGraph(t, native, build.RebuildView())
			})
		})
	}
}

// Use the native root dispatch, including its order. Do not seed or reduce
// module artifacts through the rebuild implementation on the oracle arm.
func rebuildNativeModuleGraph(root string, imports []*graph.Node) *graph.Graph {
	g := graph.New()
	g.AddBatch(imports, nil)
	idx := New(g, parser.NewRegistry(), config.Default().Index, zap.NewNop())
	defer idx.Close()
	idx.SetRootPath(root)
	for _, manifest := range rootManifests() {
		idx.extractOneModuleManifest(manifest.path, manifest.parse, manifest.ownPathFromSrc)
	}
	return g
}

func rebuildModuleTestTargets(g *graph.Graph, from string) []string {
	var targets []string
	for _, edge := range g.AllEdges() {
		if edge.From == from && edge.Kind == graph.EdgeDependsOnModule {
			targets = append(targets, edge.To)
		}
	}
	return targets
}

func withRebuildModuleTestInputs(t *testing.T, src *source.FilesystemSource, omit string, use func(string, rebuildDigest, []rebuildInput)) {
	t.Helper()
	ctx := t.Context()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := New(graph.New(), reg, config.Default().Index, zap.NewNop())
	defer idx.Close()
	req := rebuildBuildRequest{Root: src.FrozenRoot().Name(), Witness: store_pebble.Witness{Recipe: store_pebble.HeadDigest{1}}}
	err := withRebuildCheckoutBuildInputs(ctx, t.TempDir(), src, req, nil, false,
		func(key string, mode fs.FileMode, member rebuildGitMember, raw *int64, emit func(rebuildSidecarRow) error) (rebuildCheckoutLeaf, error) {
			return idx.freezeRebuildCheckoutLeaf(ctx, src.FrozenRoot(), key, mode, member, nil, 4096, raw, emit)
		}, func(epoch rebuildCheckoutInputs, raw *int64, emit func(rebuildSidecarRow) error) (string, error) {
			dropped := 0
			name, err := captureRebuildCheckoutFacts(ctx, src, epoch, true, true, raw, func(row rebuildSidecarRow) error {
				if row.Kind == rebuildModuleFact && string(row.Path) == omit {
					dropped++
					return nil
				}
				return emit(row)
			})
			if omit != "" {
				require.Equal(t, 1, dropped, "drop one required fact before sidecar hashing")
			}
			return name, err
		}, func(lease string, bound rebuildBuildRequest, inputs []rebuildInput) error {
			digest := rebuildDigest(bound.Witness.FrozenInputs)
			if omit != "" {
				require.NoError(t, visitRebuildInputSidecar(ctx, lease, digest, func(rebuildSidecarRow) error { return nil }))
			}
			use(lease, digest, inputs)
			return nil
		})
	require.NoError(t, err)
}

func openRebuildModuleTestStore(t *testing.T, imports []*graph.Node) *store_pebble.BuildStore {
	t.Helper()
	build, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "working"), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, build.Close()) })
	build.AddBatch(imports, nil)
	return build
}

func requireRebuildModuleGraph(t *testing.T, native *graph.Graph, view store_pebble.RebuildView) {
	t.Helper()
	var nodes []*graph.Node
	require.NoError(t, view.VisitNodes(t.Context(), func(node *graph.Node) error {
		nodes = append(nodes, node)
		return nil
	}))
	require.ElementsMatch(t, native.AllNodes(), nodes)
	var edges []*graph.Edge
	require.NoError(t, view.VisitEdges(t.Context(), func(edge *graph.Edge) error {
		edges = append(edges, edge)
		return nil
	}))
	require.ElementsMatch(t, native.AllEdges(), edges)
}

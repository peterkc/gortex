package indexer

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/parser/tsalias"
	"go.uber.org/zap"
)

func TestRebuildFrozenMain(t *testing.T) {
	for _, denied := range []bool{false, true} {
		name := "frozen fallback uses the literal caller scope"
		if denied {
			name = "denied first target does not fall through"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			root, src := rebuildInputFixture(t)
			const callerDir = "cafe\u0301"
			const callerRaw = callerDir + "/main.ts"
			const callerKey = "caf\u00e9/main.ts"
			const configRaw = callerDir + "/tsconfig.json"
			const configKey = "caf\u00e9/tsconfig.json"
			const target = "shared/x.ts"
			const spec = "@lib/x"
			const pending = "unresolved::import::" + spec
			writeRebuildTSAliasFixtureFile(t, root, callerRaw, `import "@lib/x"`)
			writeRebuildTSAliasFixtureFile(t, root, configRaw, `{"compilerOptions":{"paths":{"@lib/*":["../first/*","../shared/*"]}}}`)
			writeRebuildTSAliasFixtureFile(t, root, target, "export {}")
			var flat *excludes.Matcher
			if denied {
				writeRebuildTSAliasFixtureFile(t, root, "first/x.ts", "export {}")
				flat = excludes.New([]string{"first/"})
			} else {
				native := tsalias.Load(root)
				require.Equal(t, "shared/x", tsalias.Resolve(native.FindForFile(callerRaw), spec))
			}

			cfg := config.Default().Index
			reg := parser.NewRegistry()
			languages.RegisterAll(reg)
			idx := New(graph.New(), reg, cfg, zap.NewNop())
			t.Cleanup(idx.Close)
			req := rebuildBuildRequest{Root: root, Witness: store_pebble.Witness{Recipe: store_pebble.HeadDigest{1}}}
			t.Setenv("GORTEX_TEST_REBUILD_BUILD_CHILD", "1")
			link := rebuildTestLinkOptions(t)
			err := withRebuildCheckoutBuildInputs(ctx, t.TempDir(), src, req, flat, false,
				func(key string, mode fs.FileMode, member rebuildGitMember, raw *int64, emit func(rebuildSidecarRow) error) (rebuildCheckoutLeaf, error) {
					return idx.freezeRebuildCheckoutLeaf(ctx, src.FrozenRoot(), key, mode, member, nil, 1024, raw, emit)
				}, func(epoch rebuildCheckoutInputs, raw *int64, emit func(rebuildSidecarRow) error) (string, error) {
					return captureRebuildCheckoutResolverInputs(ctx, src, epoch, true, raw, emit)
				}, func(lease string, bound rebuildBuildRequest, inputs []rebuildInput) error {
					require.Len(t, inputs, 3)
					langs := make(map[string]string, len(inputs))
					for _, input := range inputs {
						langs[input.Path] = input.Language
					}
					require.Equal(t, map[string]string{callerRaw: "typescript", configRaw: "json", target: "typescript"}, langs)
					if !denied {
						require.NoError(t, os.Remove(filepath.Join(root, target)))
						writeRebuildTSAliasFixtureFile(t, root, "first/x.ts", "export {}")
						native := tsalias.Load(root)
						require.Equal(t, "first/x", tsalias.Resolve(native.FindForFile(callerRaw), spec))
					}
					build, err := store_pebble.OpenBuild(filepath.Join(lease, "working"), store_pebble.FullProfile)
					require.NoError(t, err)
					defer func() { require.NoError(t, build.Close()) }()
					build.AddNode(&graph.Node{ID: callerKey, Kind: graph.KindFile, FilePath: callerKey, Language: "typescript"})
					build.AddNode(&graph.Node{ID: target, Kind: graph.KindFile, FilePath: target, Language: "typescript"})
					build.AddNode(&graph.Node{ID: configKey, Kind: graph.KindFile, FilePath: configKey, Language: "json"})
					edge := graph.Edge{From: callerKey, To: pending, Kind: graph.EdgeImports, FilePath: callerKey, Line: 3}
					build.AddEdge(&edge)
					census := &rebuildExtractorCensus{admitted: 3,
						byPath:            langs,
						originByGraphPath: map[string]string{callerKey: callerRaw, configKey: configRaw, target: target},
					}
					err = runRebuildFrozenMain(ctx, lease, rebuildDigest(bound.Witness.FrozenInputs), build, census, link)
					if denied {
						require.ErrorIs(t, err, errRebuildResolverInputDenied)
						require.Equal(t, buildRefusal(err).Refusal, buildRefusal(errRebuildResolverInputDenied).Refusal)
					} else {
						require.NoError(t, err)
						edge.To = target
					}
					var edges []graph.Edge
					require.NoError(t, build.RebuildView().VisitEdges(ctx, func(e *graph.Edge) error {
						edges = append(edges, *e)
						return nil
					}))
					require.Equal(t, []graph.Edge{edge}, edges)
					return nil
				})
			require.NoError(t, err)
		})
	}
}

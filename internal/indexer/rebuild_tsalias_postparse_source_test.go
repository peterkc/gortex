//go:build unix

package indexer

import (
	"io/fs"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

func TestRebuildTSAliasPostParseSource(t *testing.T) {
	ctx := t.Context()
	root, src := rebuildInputFixture(t)
	const caller, target, spec = "app/main.ts", "shared/x.ts", "@lib/x"
	writeRebuildTSAliasFixtureFile(t, root, "tsconfig.json", `{"compilerOptions":{"paths":{"@lib/*":["missing/*","shared/*"]}}}`)
	writeRebuildTSAliasFixtureFile(t, root, caller, "// Import site\nimport \"@lib/x\"\n")
	writeRebuildTSAliasFixtureFile(t, root, target, "export const chosen = 1\n")

	cfg := config.Default().Index
	reg := parser.NewRegistry()
	reg.Register(languages.NewTypeScriptExtractor())
	native := graph.New()
	idx := New(native, reg, cfg, zap.NewNop())
	t.Cleanup(idx.Close)
	t.Cleanup(func() { forgetTSAliasCache(root) })
	idx.installRebuildParsePolicy(freezeRebuildParsePolicy(root, cfg))
	idx.SetRootPath(root)
	t.Setenv("GORTEX_TEST_REBUILD_BUILD_CHILD", "1")
	link := rebuildTestLinkOptions(t)
	req := rebuildBuildRequest{Root: root, Witness: store_pebble.Witness{Recipe: store_pebble.HeadDigest{1}}}
	err := withRebuildCheckoutBuildInputs(ctx, t.TempDir(), src, req, nil, false,
		func(key string, mode fs.FileMode, member rebuildGitMember, raw *int64, emit func(rebuildSidecarRow) error) (rebuildCheckoutLeaf, error) {
			return idx.freezeRebuildCheckoutLeaf(ctx, src.FrozenRoot(), key, mode, member, nil, 1024, raw, emit)
		}, func(epoch rebuildCheckoutInputs, raw *int64, emit func(rebuildSidecarRow) error) (string, error) {
			return captureRebuildCheckoutResolverInputs(ctx, src, epoch, true, raw, emit)
		}, func(lease string, bound rebuildBuildRequest, inputs []rebuildInput) error {
			store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
			recipe := rebuildDigest{1}
			census := &rebuildExtractorCensus{
				admitted: uint64(len(inputs)), byPath: make(map[string]string), originByGraphPath: make(map[string]string),
			}
			for _, input := range inputs {
				facts, reused, err := idx.loadRebuildFile(ctx, src, store, input, recipe, rebuildTransformBounds{})
				require.NoError(t, err)
				require.False(t, reused)
				census.byPath[input.Path] = facts.EffectiveLanguage
				census.originByGraphPath[input.Path] = input.Path
			}
			require.Equal(t, map[string]string{caller: "typescript", target: "typescript"}, census.byPath)
			job := rebuildFileStages{inputs: inputs, recipes: map[string]rebuildDigest{"typescript": recipe}}
			receipts := idx.rebuildDataflowReceipts(ctx, job, store, census)
			build, err := store_pebble.OpenBuild(filepath.Join(lease, "working"), store_pebble.FullProfile)
			require.NoError(t, err)
			defer func() { require.NoError(t, build.Close()) }()
			for facts, err := range receipts {
				require.NoError(t, err)
				require.NoError(t, build.AddParsedFile("", facts))
			}
			view := build.RebuildView()
			imports := func() []*graph.Edge {
				var rows []*graph.Edge
				require.NoError(t, view.VisitEdges(ctx, func(e *graph.Edge) error {
					if e.Kind == graph.EdgeImports {
						rows = append(rows, e)
					}
					return nil
				}))
				return rows
			}
			raw := graph.Edge{From: caller, To: "unresolved::import::" + spec, Kind: graph.EdgeImports, FilePath: caller, Line: 2}
			require.Equal(t, []*graph.Edge{&raw}, imports())
			require.Equal(t, "shared/x", idx.resolvePathAliasImport(caller, spec))

			// The earlier target appears after parsing. Keep the old target
			// on disk so the change isolates the order of existence probes.
			writeRebuildTSAliasFixtureFile(t, root, "missing/x.ts", "export const earlier = 2\n")
			require.Equal(t, "missing/x", idx.resolvePathAliasImport(caller, spec))

			// Decode the same receipts afresh for the unsourced Indexer's
			// real repo-scoped callbacks. The new target has no old receipt.
			for facts, err := range receipts {
				require.NoError(t, err)
				native.AddBatch(facts.Nodes, facts.Edges)
			}
			require.Equal(t, []*graph.Edge{&raw}, slices.Collect(native.EdgesByKind(graph.EdgeImports)))
			stats, err := idx.resolver.ResolveAllContext(ctx)
			require.NoError(t, err)
			require.False(t, stats.Incomplete)
			live := raw
			live.To = "external::" + spec
			require.Equal(t, []*graph.Edge{&live}, slices.Collect(native.EdgesByKind(graph.EdgeImports)))

			require.NoError(t, runRebuildFrozenMain(ctx, lease, rebuildDigest(bound.Witness.FrozenInputs), build, census, link, bound.SkipPlaceholderSources))
			settled := raw
			settled.To = target
			require.Equal(t, []*graph.Edge{&settled}, imports())
			return nil
		})
	require.NoError(t, err)
}

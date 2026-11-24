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
	const cPath = "src/main.c"
	writeRebuildTSAliasFixtureFile(t, root, cPath, `void c_leaf(void) {}
void c_entry(void) {
    c_leaf();
}
`)
	writeRebuildTSAliasFixtureFile(t, root, "tsconfig.json", `{"compilerOptions":{"paths":{"@lib/*":["missing/*","shared/*"]}}}`)
	writeRebuildTSAliasFixtureFile(t, root, caller, "// Import sites\nimport \"@lib/x\"\nimport \"../shared/x\"\n")
	writeRebuildTSAliasFixtureFile(t, root, target, "export function chosen() {}\n")

	cfg := config.Default().Index
	reg := parser.NewRegistry()
	reg.Register(languages.NewTypeScriptExtractor())
	reg.Register(languages.NewCExtractor())
	idx := New(graph.New(), reg, cfg, zap.NewNop())
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
			require.Equal(t, map[string]string{cPath: "c", caller: "typescript", target: "typescript"}, census.byPath)
			job := rebuildFileStages{inputs: inputs, recipes: map[string]rebuildDigest{"c": recipe, "typescript": recipe}}
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
			relative := raw
			relative.To, relative.Line = "unresolved::import::../shared/x", 3
			require.ElementsMatch(t, []*graph.Edge{&raw, &relative}, imports())
			cRaw := graph.Edge{From: cPath + "::c_entry", To: "unresolved::c_leaf", Kind: graph.EdgeCalls, FilePath: cPath, Line: 3}
			cOld := graph.EdgeIdentityFor(&cRaw)
			cBound := cRaw
			cBound.To, cBound.Origin, cBound.Confidence = cPath+"::c_leaf", graph.OriginASTResolved, 0.9
			cNew := graph.EdgeIdentityFor(&cBound)
			before, err := view.Edge(ctx, cOld)
			require.NoError(t, err)
			require.Equal(t, &cRaw, before)
			absent, err := view.Edge(ctx, cNew)
			require.NoError(t, err)
			require.Nil(t, absent)
			var deferredDecls []*graph.Node
			for _, id := range []string{cOld.From, cNew.To} {
				n, err := view.Node(ctx, id)
				require.NoError(t, err)
				require.NotNil(t, n, id)
				require.Equal(t, graph.KindFunction, n.Kind)
				require.Equal(t, "c", n.Language)
				deferredDecls = append(deferredDecls, n)
			}

			// Decode the exact receipts afresh for each native arm. Keep the
			// ordinary resolver order and all declarations, including C.
			replayNative := func() *graph.Graph {
				t.Helper()
				native := graph.New()
				oracle := New(native, reg, cfg, zap.NewNop())
				defer oracle.Close()
				oracle.SetSemanticManager(nil)
				oracle.SetResolverLSPHelper(nil)
				oracle.SetRootPath(root)
				for facts, err := range receipts {
					require.NoError(t, err)
					native.AddBatch(facts.Nodes, facts.Edges)
				}
				require.ElementsMatch(t, []*graph.Edge{&raw, &relative}, slices.Collect(native.EdgesByKind(graph.EdgeImports)))
				require.Equal(t, []*graph.Edge{before}, slices.Collect(native.EdgesByKind(graph.EdgeCalls)))
				stats, err := oracle.resolver.ResolveAllContext(ctx)
				require.NoError(t, err)
				require.False(t, stats.Incomplete)
				return native
			}
			require.Equal(t, "shared/x", idx.resolvePathAliasImport(caller, spec))
			native := replayNative()
			require.Equal(t, []*graph.Edge{&cBound}, slices.Collect(native.EdgesByKind(graph.EdgeCalls)))
			settled, settledRelative := raw, relative
			settled.To, settledRelative.To = target, target
			nativeImports := slices.Collect(native.EdgesByKind(graph.EdgeImports))
			require.ElementsMatch(t, []*graph.Edge{&settled, &settledRelative}, nativeImports)

			// The earlier target appears after parsing. Keep the old target
			// on disk so the change isolates the order of existence probes.
			writeRebuildTSAliasFixtureFile(t, root, "missing/x.ts", "export const earlier = 2\n")
			require.Equal(t, "missing/x", idx.resolvePathAliasImport(caller, spec))

			// A live replay has no receipt for the new target and loses just
			// the aliased bind. The same frozen sidecar still has the old probe.
			live := raw
			live.To = "external::" + spec
			require.ElementsMatch(t, []*graph.Edge{&live, &settledRelative}, slices.Collect(replayNative().EdgesByKind(graph.EdgeImports)))

			require.NoError(t, runRebuildFrozenMain(ctx, lease, rebuildDigest(bound.Witness.FrozenInputs), build, census, link, bound.SkipPlaceholderSources))
			privateImports := imports()
			require.Equal(t, []*graph.Edge{&settled, &settledRelative}, privateImports, "private complete edge-key order")
			require.ElementsMatch(t, nativeImports, privateImports, "complete retained Imports family in this fixture")
			pending, err := view.Edge(ctx, cOld)
			require.NoError(t, err)
			require.Equal(t, before, pending, "the whole deferred C before-value must survive")
			absent, err = view.Edge(ctx, cNew)
			require.NoError(t, err)
			require.Nil(t, absent)
			for _, before := range deferredDecls {
				n, err := view.Node(ctx, before.ID)
				require.NoError(t, err)
				require.Equal(t, before, n, "deferred declaration %q", before.ID)
			}
			return nil
		})
	require.NoError(t, err)
}

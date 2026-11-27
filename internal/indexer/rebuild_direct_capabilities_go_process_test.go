package indexer

import (
	"fmt"
	"io/fs"
	"iter"
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

func TestRebuildDirectCapabilitiesGoProcess(t *testing.T) {
	const path, caller, line = "launch.go", "launch.go::Launch", 5
	const mechanism, process = "exec.Command", "string::process::exec.Command"
	t.Setenv("GORTEX_TEST_REBUILD_BUILD_CHILD", "1")
	link := rebuildTestLinkOptions(t)
	for _, tc := range []struct {
		name, importPath, target, role string
	}{
		{"stdlib", "os/exec", "stdlib::os/exec::Command", "stdlib"},
		{"lookalike", "example.test/exec", "dep::example.test/exec::Command", "dep"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			root, src := rebuildInputFixture(t)
			writeRebuildTSAliasFixtureFile(t, root, "go.mod", "module example.test/witness\n")
			writeRebuildTSAliasFixtureFile(t, root, path, fmt.Sprintf(
				"package witness\n\nimport run %q\n\nfunc Launch() { _ = run.Command(\"true\") }\n", tc.importPath))
			cfg := config.Default().Index
			reg := parser.NewRegistry()
			reg.Register(languages.NewGoExtractor())
			idx := New(graph.New(), reg, cfg, zap.NewNop())
			t.Cleanup(idx.Close)
			idx.installRebuildParsePolicy(freezeRebuildParsePolicy(root, cfg))
			idx.SetRootPath(root)

			req := rebuildBuildRequest{Root: root, Witness: store_pebble.Witness{Recipe: store_pebble.HeadDigest{1}}}
			err := withRebuildCheckoutBuildInputs(ctx, t.TempDir(), src, req, nil, false,
				func(key string, mode fs.FileMode, member rebuildGitMember, raw *int64, emit func(rebuildSidecarRow) error) (rebuildCheckoutLeaf, error) {
					return idx.freezeRebuildCheckoutLeaf(ctx, src.FrozenRoot(), key, mode, member, nil, 1024, raw, emit)
				}, func(epoch rebuildCheckoutInputs, raw *int64, emit func(rebuildSidecarRow) error) (string, error) {
					return captureRebuildCheckoutResolverInputs(ctx, src, epoch, true, raw, emit)
				}, func(lease string, bound rebuildBuildRequest, inputs []rebuildInput) error {
					require.Len(t, inputs, 1)
					require.Equal(t, path, inputs[0].Path)
					require.Equal(t, "go", inputs[0].Language)
					store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
					recipe := rebuildDigest{1}
					_, reused, err := idx.loadRebuildFile(ctx, src, store, inputs[0], recipe, rebuildTransformBounds{})
					require.NoError(t, err)
					require.False(t, reused)
					census := &rebuildExtractorCensus{
						admitted: 1, byPath: map[string]string{path: "go"}, originByGraphPath: map[string]string{path: path},
					}
					job := rebuildFileStages{inputs: inputs, recipes: map[string]rebuildDigest{"go": recipe}}
					receipts := idx.rebuildDataflowReceipts(ctx, job, store, census)
					build, err := store_pebble.OpenBuild(filepath.Join(lease, "working"), store_pebble.FullProfile)
					require.NoError(t, err)
					defer func() { require.NoError(t, build.Close()) }()

					call := graph.EdgeIdentity{From: caller, To: "unresolved::extern::" + tc.importPath + "::Command",
						Kind: graph.EdgeCalls, FilePath: path, Line: line}
					var rawCalls []graph.EdgeIdentity
					for facts, err := range receipts {
						require.NoError(t, err)
						for _, e := range facts.Edges {
							if e.Kind == graph.EdgeCalls {
								rawCalls = append(rawCalls, graph.EdgeIdentityFor(e))
							}
						}
						require.NoError(t, build.AddParsedFile("", facts))
					}
					require.Equal(t, []graph.EdgeIdentity{call}, rawCalls)
					require.NoError(t, runRebuildFrozenMain(ctx, lease, rebuildDigest(bound.Witness.FrozenInputs), build, census, link, bound.SkipPlaceholderSources))

					view := build.RebuildView()
					priorProcess, err := view.Node(ctx, process)
					require.NoError(t, err)
					require.Nil(t, priorProcess)
					symbol, err := view.Node(ctx, tc.target)
					require.NoError(t, err)
					require.Equal(t, &graph.Node{ID: tc.target, Kind: graph.KindFunction, Name: "Command", Language: "go",
						Meta: map[string]any{"external": true, "module_path": tc.importPath, "module_role": tc.role}}, symbol)
					var nodes []*graph.Node
					require.NoError(t, view.VisitNodes(ctx, func(n *graph.Node) error {
						nodes = append(nodes, n)
						return nil
					}))
					var edges []*graph.Edge
					var settledCalls []graph.EdgeIdentity
					require.NoError(t, view.VisitEdges(ctx, func(e *graph.Edge) error {
						edges = append(edges, e)
						if e.Kind == graph.EdgeCalls {
							settledCalls = append(settledCalls, graph.EdgeIdentityFor(e))
						}
						return nil
					}))
					call.To = tc.target
					require.Equal(t, []graph.EdgeIdentity{call}, settledCalls)

					// The native matcher folds both settled targets to exec.Command.
					native := rebuildDirectCapabilitiesNative(t, nodes, edges, [3]int{0, 1, 0})
					require.Equal(t, []*graph.Edge{{From: caller, To: process, Kind: graph.EdgeExecutesProcess,
						FilePath: path, Line: line, Origin: graph.OriginASTInferred}}, slices.Collect(native.EdgesByKind(graph.EdgeExecutesProcess)))
					require.Equal(t, &graph.Node{ID: process, Kind: graph.KindString, Name: mechanism,
						Meta: map[string]any{"context": "process", "mechanism": mechanism}}, native.GetNode(process))

					// A deferred receipt at the same site can veto only the full
					// structured extern that it actually records.
					checkedReceipts := receipts
					if tc.role == "stdlib" {
						withDeferred := func(symbol string) iter.Seq2[store_pebble.ParseFacts, error] {
							return func(yield func(store_pebble.ParseFacts, error) bool) {
								for facts, err := range receipts {
									if !yield(facts, err) || err != nil {
										return
									}
								}
								decoy := &graph.Edge{
									From: caller, To: "unresolved::extern::os/exec::" + symbol,
									Kind: graph.EdgeCalls, FilePath: path, Line: line,
								}
								yield(store_pebble.ParseFacts{
									EffectiveLanguage: "python",
									File:              graph.FileMetaRow{FilePath: "decoy.py"},
									Edges:             []*graph.Edge{decoy},
								}, nil)
							}
						}
						err := runRebuildDirectCapabilities(ctx, build, withDeferred("Command"))
						var refusal *store_pebble.RebuildUnknownProvenanceError
						require.ErrorAs(t, err, &refusal)
						require.Equal(t, uint64(1), refusal.Count)
						reply := buildRefusal(err)
						require.Equal(t, uint8(36), reply.Refusal)
						require.Equal(t, uint64(1), reply.UnknownProvenance)
						requireRebuildTestFileRows(t, build, nodes, edges)
						checkedReceipts = withDeferred("CommandContext")
					}
					err = runRebuildDirectCapabilities(ctx, build, checkedReceipts)
					if tc.role != "stdlib" {
						var refusal *store_pebble.RebuildUnknownProvenanceError
						require.ErrorAs(t, err, &refusal)
						require.Equal(t, uint64(1), refusal.Count)
						reply := buildRefusal(err)
						require.Equal(t, uint8(36), reply.Refusal)
						require.Equal(t, uint64(1), reply.UnknownProvenance)
						requireRebuildTestFileRows(t, build, nodes, edges)
						return nil
					}
					require.NoError(t, err)
					want := native.AllEdges()
					slices.SortFunc(want, compareRebuildTestFileEdgeKeys)
					requireRebuildTestFileRows(t, build, native.AllNodes(), want)
					return nil
				})
			require.NoError(t, err)
		})
	}
}

package indexer

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

const rebuildDeferredProbePath = "a.t95deferred"
const rebuildDeferredProbeEnv = "GORTEX_TEST_REBUILD_DEFERRED_PLUGIN"

// TestRebuildDeferredAttribution is a private witness, not a D72 projection
// or a corpus no-effect test. The arms differ by one row, not by extractor.
func TestRebuildDeferredAttribution(t *testing.T) {
	const goPath, deferred = "z.go", "t95deferred"
	const rawLen, builtinLen = "unresolved::len", "builtin::go::len"
	root, src := rebuildInputFixture(t)
	t.Setenv(rebuildDeferredProbeEnv, "1")
	t.Setenv("GORTEX_TEST_REBUILD_BUILD_CHILD", "1")
	link := rebuildTestLinkOptions(t)
	cfg := config.Default().Index
	cfg.ExtractorPlugins = []config.ExtractorPluginSpec{{
		Language: deferred, Extensions: []string{".t95deferred"}, Command: link.Binary,
		Args: []string{"-test.run=^TestRebuildDeferredAttributionPlugin$"},
	}}
	log := zap.NewNop()
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	languages.RegisterExtractorPlugins(reg, cfg.ExtractorPlugins, log)
	idx := New(graph.New(), reg, cfg, log)
	t.Cleanup(idx.Close)
	idx.installRebuildParsePolicy(freezeRebuildParsePolicy(root, cfg))
	idx.SetRootPath(root)

	cache := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	recipe := rebuildDigest{1}
	census := rebuildExtractorCensus{admitted: 2, byPath: map[string]string{}, originByGraphPath: map[string]string{}}
	var receipts [2]store_pebble.ParseFacts
	for i, file := range []struct{ path, language, body string }{
		{rebuildDeferredProbePath, deferred, "custom source\n"},
		// The independent Go call keeps the tail active in both arms.
		{goPath, "go", "package p\nfunc Witness() { Pending() }\n"},
	} {
		require.NoError(t, os.WriteFile(filepath.Join(root, file.path), []byte(file.body), 0o600))
		lang, ok := reg.DetectLanguage(file.path)
		require.True(t, ok)
		require.Equal(t, file.language, lang)
		input, _, err := readRebuildInput(t.Context(), src, file.path, lang, 1024)
		require.NoError(t, err)
		_, reused, err := idx.loadRebuildFile(t.Context(), src, cache, input, recipe, rebuildTransformBounds{})
		require.NoError(t, err)
		require.False(t, reused)
		saved, err := cache.read(input.parseKey(idx.rebuildExtractorRecipe(recipe, input)))
		require.NoError(t, err)
		facts, err := store_pebble.DecodeParseFacts(saved)
		require.NoError(t, err)
		require.Equal(t, file.path, facts.File.FilePath)
		require.Equal(t, file.language, facts.EffectiveLanguage)
		receipts[i] = facts
		census.byPath[facts.File.FilePath] = facts.EffectiveLanguage
		require.NoError(t, recordRebuildGraphPath(census.originByGraphPath, facts.File.FilePath))
	}
	wantRaw := &graph.Edge{From: rawLen, To: rawLen, Kind: graph.EdgeArgOf, FilePath: goPath, Line: 2}
	require.Equal(t, []*graph.Edge{wantRaw}, receipts[0].Edges, "the exact row came from the deferred receipt")
	wantFinal := *wantRaw
	wantFinal.From, wantFinal.To = builtinLen, builtinLen
	for _, arm := range []struct {
		name string
		want *graph.Edge
	}{
		{"row present", &wantFinal},
		{"row withheld", nil},
	} {
		t.Run(arm.name, func(t *testing.T) {
			ctx := t.Context()
			lease := t.TempDir()
			build, err := store_pebble.OpenBuild(filepath.Join(lease, "working"), store_pebble.FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, build.Close()) })
			plugin := receipts[0]
			if arm.want == nil {
				plugin.Edges = nil // the exact singleton asserted above; keep its declarations
			}
			require.NoError(t, build.AddParsedFile("", plugin))
			require.NoError(t, build.AddParsedFile("", receipts[1]))
			view := build.RebuildView()
			var selectedLen []*graph.Edge
			goCalls := 0
			require.NoError(t, view.VisitLinkPendingPages(ctx, 64, 16<<10, func(page []*graph.Edge) error {
				selected, err := census.selectPendingCallers(page)
				for _, e := range selected {
					if e.From == rawLen || e.To == rawLen {
						selectedLen = append(selectedLen, e)
					}
					if e.From == "z.go::Witness" && e.Kind == graph.EdgeCalls && e.To == "unresolved::Pending" {
						goCalls++
					}
				}
				return err
			}))
			require.Equal(t, 1, goCalls, "the genuine Go pending call must keep both tails active")
			if arm.want == nil {
				require.Empty(t, selectedLen)
			} else {
				require.Equal(t, []*graph.Edge{wantRaw}, selectedLen, "recording-path lookup admitted the deferred row as Go")
			}

			require.NoError(t, runRebuildFrozenMain(ctx, lease, rebuildDigest{}, build, &census, link, false))
			decl, err := view.Node(ctx, rebuildDeferredProbePath+"::Deferred")
			require.NoError(t, err)
			require.NotNil(t, decl, "the deferred declaration must survive both arms")
			require.Equal(t, deferred, decl.Language)
			var lenEdges []*graph.Edge
			require.NoError(t, view.VisitEdges(ctx, func(e *graph.Edge) error {
				if e.From == rawLen || e.To == rawLen || e.From == builtinLen || e.To == builtinLen {
					lenEdges = append(lenEdges, e)
				}
				return nil
			}))
			node, err := view.Node(ctx, builtinLen)
			require.NoError(t, err)
			if arm.want == nil {
				require.Empty(t, lenEdges)
				require.Nil(t, node)
				return
			}
			require.Equal(t, []*graph.Edge{arm.want}, lenEdges)
			require.Equal(t, &graph.Node{
				ID: builtinLen, Name: "len", Kind: graph.KindBuiltin, Language: "go",
				Meta: map[string]any{"builtin": true, "builtin_kind": "func"},
			}, node)
		})
	}
}

func TestRebuildDeferredAttributionPlugin(t *testing.T) {
	if os.Getenv(rebuildDeferredProbeEnv) != "1" {
		t.Skip("extractor plugin entry point")
	}
	require.Equal(t, rebuildDeferredProbePath, os.Getenv("GORTEX_FILE_PATH"))
	raw, err := io.ReadAll(os.Stdin)
	require.NoError(t, err)
	require.Equal(t, "custom source\n", string(raw))
	const document = `{"nodes":[{"kind":"function","name":"Deferred"}],"edges":[` +
		`{"from":"unresolved::len","to":"unresolved::len","kind":"arg_of","file_path":"z.go","line":2}]}`
	_, err = os.Stdout.WriteString(document)
	require.NoError(t, err)
	os.Exit(0)
}

package indexer

import (
	"context"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	binary, err := os.Executable()
	require.NoError(t, err)
	link := rebuildTestLinkOptions(t)
	cfg := config.Default().Index
	cfg.ExtractorPlugins = []config.ExtractorPluginSpec{{
		Language: deferred, Extensions: []string{".t95deferred"}, Command: binary,
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
		facts := decodeRebuildTestParseFacts(t, saved)
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
			require.Nil(t, node, "attribution must not materialize the builtin endpoint")
			if arm.want == nil {
				require.Empty(t, lenEdges)
				return
			}
			require.Equal(t, []*graph.Edge{arm.want}, lenEdges)
		})
	}
}

// A settled row from a real plugin receipt must refuse across the worker
// pipe even when the main resolver has no pending caller to select.
func TestRebuildDeferredArgOfRefusal(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	root, src := rebuildInputFixture(t)
	const deferred, goPath = "t95deferred", "z.go"
	t.Setenv(rebuildDeferredProbeEnv, "arg_lift")
	t.Setenv("GORTEX_TEST_REBUILD_BUILD_CHILD", "1")
	binary, err := os.Executable()
	require.NoError(t, err)
	cfg := config.Default().Index
	cfg.ExtractorPlugins = []config.ExtractorPluginSpec{{
		Language: deferred, Extensions: []string{".t95deferred"}, Command: binary,
		Args: []string{"-test.run=^TestRebuildDeferredAttributionPlugin$"},
	}}
	var inputs []rebuildInput
	for _, file := range []struct{ path, language, body string }{
		{rebuildDeferredProbePath, deferred, "custom source\n"},
		{goPath, "go", "package p\nfunc Sink(value int) {}\n"},
	} {
		require.NoError(t, os.WriteFile(filepath.Join(root, file.path), []byte(file.body), 0o600))
		in, _, err := readRebuildInput(ctx, src, file.path, file.language, 1024)
		require.NoError(t, err)
		inputs = append(inputs, in)
	}
	digest, err := rebuildInputRoot(inputs)
	require.NoError(t, err)
	raw, err := hex.DecodeString(digest)
	require.NoError(t, err)
	require.Len(t, raw, 32)
	req := rebuildBuildRequest{
		Root: root, ParseDir: t.TempDir(), ParseLimit: 1 << 20, Incarnation: rebuildTestIncarnation,
		Recipes: map[string]rebuildDigest{"go": {1}, deferred: {2}}, Policy: freezeRebuildParsePolicy(root, cfg),
		Bounds: rebuildTransformBounds{CaptureBytes: 1024, FileBytes: 64 << 10},
		Witness: store_pebble.Witness{Input: store_pebble.HeadDigest(raw), Recipe: store_pebble.HeadDigest{2},
			Profile: store_pebble.FullProfile, Stage: store_pebble.StageManifest},
	}
	leases, headDir := t.TempDir(), t.TempDir()
	var stages []store_pebble.Stage
	err = withRebuildBuildInputs(ctx, leases, src, map[string]*rebuildInput{"go.mod": nil, "package.json": nil}, req, nil,
		func(lease string, bound rebuildBuildRequest) error {
			return runRebuildBuild(ctx, binary, []string{"-test.run=^TestRebuildBuildChildEntry$", "--"}, lease, headDir, bound, cfg, inputs,
				func(_ context.Context, offer store_pebble.StageTables) error {
					stages = append(stages, offer.Witness.Stage)
					return nil
				})
		})
	require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
	require.ErrorContains(t, err, "unknown_provenance=1")
	require.ErrorContains(t, err, `"arg_of","z.go::Sink","z.go",2`)
	require.Equal(t, []store_pebble.Stage{store_pebble.StageManifest, store_pebble.StageFileGraph}, stages)
	entries, err := os.ReadDir(leases)
	require.NoError(t, err)
	require.Empty(t, entries, "the owner must discard the refused working store")
}

func TestRebuildDeferredAttributionPlugin(t *testing.T) {
	mode := os.Getenv(rebuildDeferredProbeEnv)
	if mode != "1" && mode != "arg_lift" {
		t.Skip("extractor plugin entry point")
	}
	require.Equal(t, rebuildDeferredProbePath, os.Getenv("GORTEX_FILE_PATH"))
	raw, err := io.ReadAll(os.Stdin)
	require.NoError(t, err)
	require.Equal(t, "custom source\n", string(raw))
	document := `{"nodes":[{"kind":"function","name":"Deferred"}],"edges":[` +
		`{"from":"unresolved::len","to":"unresolved::len","kind":"arg_of","file_path":"z.go","line":2}]}`
	if mode == "arg_lift" {
		document = `{"nodes":[{"kind":"function","name":"Deferred"}],"edges":[` +
			`{"from":"a.t95deferred::Deferred","to":"z.go::Sink","kind":"arg_of","file_path":"z.go","line":2,"meta":{"arg_position":0}}]}`
	}
	_, err = os.Stdout.WriteString(document)
	require.NoError(t, err)
	os.Exit(0)
}

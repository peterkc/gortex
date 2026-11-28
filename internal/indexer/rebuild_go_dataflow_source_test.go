package indexer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestRunRebuildCheckoutGoDataflowSources(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	root, src := rebuildInputFixture(t)
	const file = "source.go"
	const placeholder = "unresolved::extern::net/http::StatusCreated"
	const packageConst = "unresolved::extern::example.org/widgets/sub::Const"
	const packageConstNode = "sub/value.go::Const"
	require.NoError(t, os.Mkdir(filepath.Join(root, "sub"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "sub/value.go"), []byte("package sub\nconst Const = 7\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.org/widgets\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, file), []byte(`package p
import (
    "net/http"
    "example.org/widgets/sub"
)
func Arg() { Sink(http.StatusCreated, http.StatusCreated) }
func Value() int { return http.StatusCreated }
func ArgLocal() { Sink(sub.Const) }
func ValueLocal() int { return sub.Const }
func BlankArg(x int) { Sink(x._, sub._, _) }
func BlankValue(x int) (int, int, int) { return x._, sub._, _ }
`), 0o600))
	cfg := config.Default().Index
	cfg.Workers = 2
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	native := newFTSStore(t)
	// Direct SQLite keeps the first argument at a repeated edge key. Neither
	// the cold-index shadow nor its streaming chunks are the reference here.
	t.Setenv("GORTEX_SHADOW_MAX_FILES", "0")
	t.Setenv("GORTEX_STREAMING_FLUSH", "0")
	core, logs := observer.New(zap.InfoLevel)
	idx := New(native, reg, cfg, zap.New(core))
	t.Cleanup(idx.Close)
	_, err := idx.Index(root)
	require.NoError(t, err)
	require.False(t, contentFinalizeShadowDecision(t, logs))
	require.Zero(t, logs.FilterMessage("indexer: streaming-flush parse").Len())
	rows := func(edges []*graph.Edge) map[graph.EdgeKind]string {
		out := make(map[graph.EdgeKind]string)
		for _, e := range edges {
			if e.FilePath != file || (e.Kind != graph.EdgeArgOf && e.Kind != graph.EdgeValueFlow) {
				continue
			}
			require.NotContains(t, out, e.Kind, "duplicate %s from %s", e.Kind, e.From)
			if e.From == placeholder && e.Kind == graph.EdgeArgOf {
				position, ok := argPositionFromMeta(e.Meta)
				require.True(t, ok)
				require.Zero(t, position, "direct SQLite keeps the first argument")
			}
			encoded, err := json.Marshal(e)
			require.NoError(t, err)
			out[e.Kind] = string(encoded)
		}
		return out
	}
	require.NotNil(t, native.GetNode(packageConstNode))
	wantModulePlaceholder := rows(native.GetOutEdges(packageConst))
	wantModuleConcrete := rows(native.GetOutEdges(packageConstNode))
	require.Equal(t, 2, len(wantModulePlaceholder)+len(wantModuleConcrete), "native module argument and return source rows")
	req := rebuildBuildRequest{
		Root: root, ParseDir: t.TempDir(), ParseLimit: 1 << 20, Incarnation: rebuildTestIncarnation,
		Policy: freezeRebuildParsePolicy(root, cfg), Recipes: map[string]rebuildDigest{"go": {1}},
		Bounds:  rebuildTransformBounds{CaptureBytes: 1024, FileBytes: 64 << 10},
		Witness: store_pebble.Witness{Recipe: store_pebble.HeadDigest{2}, Profile: store_pebble.FullProfile, Stage: store_pebble.StageManifest},
	}
	exe, err := os.Executable()
	require.NoError(t, err)
	t.Setenv("GORTEX_TEST_REBUILD_BUILD_CHILD", "1")
	headDir := t.TempDir()
	h, err := store_pebble.OpenHead(headDir, req.Incarnation, req.Witness.Profile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	err = idx.runRebuildCheckout(ctx, src, req, excludes.New(nil), false, exe,
		[]string{"-test.run=^TestRebuildBuildChildEntry$", "--"}, t.TempDir(), headDir,
		func(_ context.Context, offer store_pebble.StageTables) error {
			_, err := h.PublishStage(ctx, offer)
			return err
		})
	require.NoError(t, err)
	snap := h.OpenSnapshot()
	defer snap.Close()
	require.NoError(t, snap.RequireStage(store_pebble.StageSearch))
	for _, from := range []string{placeholder, "unresolved::*._", "unresolved::extern::example.org/widgets/sub::_"} {
		want := rows(native.GetOutEdges(from))
		require.Len(t, want, 2, "native argument and return rows from %s", from)
		require.Nil(t, native.GetNode(from), from)
		require.Nil(t, snap.GetNode(from), from)
		require.Equal(t, want, rows(snap.GetOutEdges(from)), from)
	}
	require.Empty(t, rows(native.GetOutEdges("unresolved::_")))
	require.Empty(t, rows(snap.GetOutEdges("unresolved::_")))
	require.Equal(t, wantModulePlaceholder, rows(snap.GetOutEdges(packageConst)))
	require.Equal(t, wantModuleConcrete, rows(snap.GetOutEdges(packageConstNode)))
}

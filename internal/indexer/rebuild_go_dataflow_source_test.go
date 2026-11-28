package indexer

import (
	"context"
	"encoding/json"
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
	"go.uber.org/zap"
)

func TestRunRebuildCheckoutGoDataflowSources(t *testing.T) {
	ctx := t.Context()
	root, src := rebuildInputFixture(t)
	const file = "source.go"
	const placeholder = "unresolved::extern::net/http::StatusCreated"
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.org/widgets\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, file), []byte(`package p
import "net/http"
func Arg() { Sink(http.StatusCreated) }
func Value() int { return http.StatusCreated }
`), 0o600))
	cfg := config.Default().Index
	cfg.Workers = 2
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	native := newFTSStore(t)
	idx := New(native, reg, cfg, zap.NewNop())
	t.Cleanup(idx.Close)
	_, err := idx.Index(root)
	require.NoError(t, err)
	rows := func(edges []*graph.Edge) (out []string) {
		for _, e := range edges {
			if e.FilePath != file || (e.Kind != graph.EdgeArgOf && e.Kind != graph.EdgeValueFlow) {
				continue
			}
			encoded, err := json.Marshal(e)
			require.NoError(t, err)
			out = append(out, string(encoded))
		}
		return out
	}
	want := rows(native.GetOutEdges(placeholder))
	require.Len(t, want, 2, "native keeps both exact qualified dangling sources")
	require.Nil(t, native.GetNode(placeholder))
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
	require.Nil(t, snap.GetNode(placeholder))
	require.ElementsMatch(t, want, rows(snap.GetOutEdges(placeholder)))
}

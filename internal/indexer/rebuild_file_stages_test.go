package indexer

import (
	"encoding/hex"
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

func TestRebuildFileStagesRetainFrozenFactsAcrossEdit(t *testing.T) {
	ctx := t.Context()
	for _, precache := range []bool{true, false} {
		name := "uncached refusal"
		if precache {
			name = "cached completion"
		}
		t.Run(name, func(t *testing.T) {
			root, src := rebuildInputFixture(t)
			var inputs []rebuildInput
			for _, name := range []string{"b.go", "a.go"} {
				require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte("package p\nfunc Frozen() {}\n"), 0o600))
				i, _, err := readRebuildInput(ctx, src, name, "go", 1024)
				require.NoError(t, err)
				inputs = append(inputs, i)
			}
			rootHex, err := rebuildInputRoot(inputs)
			require.NoError(t, err)
			digest, err := hex.DecodeString(rootHex)
			require.NoError(t, err)
			require.Len(t, digest, 32)
			job := rebuildFileStages{
				workDir: t.TempDir(), incarnation: 3,
				inputs: inputs, recipes: map[string]rebuildDigest{"go": {5}},
				witness: store_pebble.Witness{
					Input: store_pebble.HeadDigest(digest), Recipe: store_pebble.HeadDigest{8},
					Profile: store_pebble.FullProfile, Stage: store_pebble.StageManifest,
				},
			}
			reg := parser.NewRegistry()
			reg.Register(languages.NewGoExtractor())
			cfg := config.Default().Index
			idx := New(graph.New(), reg, cfg, zap.NewNop())
			idx.installRebuildParsePolicy(freezeRebuildParsePolicy(root, cfg))
			idx.SetRootPath(root)
			t.Cleanup(idx.Close)
			store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
			if precache {
				_, reused, err := idx.loadRebuildFile(ctx, src, store, inputs[0], job.recipes["go"], rebuildTransformBounds{})
				require.NoError(t, err)
				require.False(t, reused)
			}
			h, err := store_pebble.OpenHead(filepath.Join(t.TempDir(), "head"), 3, store_pebble.FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, h.Close()) })
			var stages []store_pebble.Stage
			private, err := store_pebble.OpenBuild(filepath.Join(job.workDir, "working"), job.witness.Profile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, private.Close()) })
			counts, err := idx.buildRebuildFileStages(ctx, src, store, private, job, func(tables store_pebble.StageTables) error {
				_, err := h.PublishStage(ctx, tables)
				if err != nil {
					return err
				}
				stages = append(stages, tables.Witness.Stage)
				if tables.Witness.Stage == store_pebble.StageManifest {
					return os.WriteFile(filepath.Join(root, "b.go"), []byte("package p\nfunc Edited() {}\n"), 0o600)
				}
				return nil
			})
			s := h.OpenSnapshot()
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			manifestRows := 0
			require.NoError(t, s.ManifestFiles(ctx, func(store_pebble.ManifestFile) error {
				manifestRows++
				return nil
			}))
			require.Equal(t, 2, manifestRows)
			require.EqualValues(t, 1, counts.parsed, "the worker parses a.go first")
			if !precache {
				require.ErrorIs(t, err, ErrBuildInputChanged)
				require.Zero(t, counts.reused)
				require.Equal(t, []store_pebble.Stage{store_pebble.StageManifest}, stages)
				var stagePending *store_pebble.ErrStagePending
				require.ErrorAs(t, s.RequireStage(store_pebble.StageFileGraph), &stagePending)
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 1, counts.reused)
			require.Equal(t, []store_pebble.Stage{store_pebble.StageManifest, store_pebble.StageFileGraph}, stages)
			require.Len(t, s.FindNodesByName("Frozen"), 2)
			require.Empty(t, s.FindNodesByName("Edited"))
		})
	}
}

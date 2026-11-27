package indexer

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"go.uber.org/zap"
)

func TestRebuildGitPolicySidecar(t *testing.T) {
	ctx := t.Context()
	lease := t.TempDir()
	stamp := (rebuildGitInventory{stage: rebuildDigest{1}, version: "git version 2.56.0"}).sidecarRow()
	negated := rebuildSidecarRow{Kind: rebuildGitIgnoreVerdictRow, Path: []byte("dir/keep-\xff.txt"), GitVerdict: rebuildGitIgnoreMatchedNegation}
	ignored := rebuildSidecarRow{Kind: rebuildGitIgnoreVerdictRow, Path: []byte("output/"), GitVerdict: rebuildGitIgnoreIgnored}
	rows := []rebuildSidecarRow{negated, ignored, stamp}
	digest, err := writeRebuildInputSidecar(ctx, lease, func(emit func(rebuildSidecarRow) error) error {
		for _, row := range rows {
			if err := emit(row); err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)
	var proof rebuildGitPolicyProof
	var got []rebuildSidecarRow
	require.NoError(t, visitRebuildInputSidecar(ctx, lease, digest, func(row rebuildSidecarRow) error {
		got = append(got, row)
		return proof.add(row)
	}))
	require.Equal(t, rows, got)
	require.NoError(t, proof.check(true))
	require.ErrorIs(t, proof.check(false), errRebuildInputSidecar)
	require.ErrorIs(t, proof.add(stamp), errRebuildInputSidecar)
	ignored.Path = []byte("output")
	require.ErrorIs(t, proof.add(ignored), errRebuildInputSidecar, "one verdict per listed name")
	for _, row := range []rebuildSidecarRow{
		{Kind: rebuildGitIndexStampRow, Path: []byte("."), Digest: rebuildDigest{1}, GitVersion: "git version 2.56.0\n"},
		{Kind: rebuildGitIgnoreVerdictRow, Path: []byte("out//"), GitVerdict: rebuildGitIgnoreIgnored},
		{Kind: rebuildGitIgnoreVerdictRow, Path: []byte("out"), GitVerdict: 255},
		{Kind: rebuildNamespaceRegularFact, Path: []byte("out"), GitVerdict: rebuildGitIgnoreNoMatch},
	} {
		require.ErrorIs(t, row.check(), errRebuildInputSidecar)
	}
}

func TestRebuildGitPolicyMismatchRefusesBeforeBuild(t *testing.T) {
	ctx := t.Context()
	stamp := (rebuildGitInventory{stage: rebuildDigest{1}, version: "git version 2.56.0"}).sidecarRow()
	verdict := rebuildSidecarRow{Kind: rebuildGitIgnoreVerdictRow, Path: []byte("out/"), GitVerdict: rebuildGitIgnoreIgnored}
	for _, tc := range []struct {
		name   string
		marked bool
		rows   []rebuildSidecarRow
	}{
		{"marker without stamp", true, nil},
		{"unmarked stamp", false, []rebuildSidecarRow{stamp}},
		{"unmarked verdict", false, []rebuildSidecarRow{verdict}},
		{"duplicate stamp", true, []rebuildSidecarRow{stamp, stamp}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, lease := t.TempDir(), t.TempDir()
			digest, err := writeRebuildInputSidecar(ctx, lease, func(emit func(rebuildSidecarRow) error) error {
				for _, row := range tc.rows {
					if err := emit(row); err != nil {
						return err
					}
				}
				return nil
			})
			require.NoError(t, err)
			cfg := config.Default().Index
			emptyRoot, err := rebuildInputRoot(nil)
			require.NoError(t, err)
			input, err := hex.DecodeString(emptyRoot)
			require.NoError(t, err)
			req := rebuildBuildRequest{
				Root: root, ParseDir: t.TempDir(), ParseLimit: 1 << 20, Incarnation: rebuildTestIncarnation,
				BaseRecipe: store_pebble.HeadDigest{1}, ProvidersOff: true,
				Policy: freezeRebuildParsePolicy(root, cfg), Bounds: rebuildTransformBounds{CaptureBytes: 1024, FileBytes: 1024},
				Witness: store_pebble.Witness{
					Input: store_pebble.HeadDigest(input), FrozenInputs: store_pebble.HeadDigest(digest),
					Stage: store_pebble.StageManifest, Profile: store_pebble.DefaultProfile, UsesGitIgnorePolicy: tc.marked,
				},
			}
			req.Witness.PlatformCatalogRevision = graph.PlatformCatalogRevision
			req.Witness.Recipe = req.projectRecipe(cfg.Coverage.ClonesThreshold())
			require.NoError(t, writeRebuildBuildRequest(ctx, lease, req, cfg, nil))
			err = buildRebuildStages(ctx, lease, zap.NewNop(),
				func(string, store_pebble.Profile) (*store_pebble.BuildStore, error) {
					t.Fatal("mismatched Git proof reached private store")
					return nil, nil
				}, store_pebble.RebuildLinkOptions{}, func(store_pebble.StageTables) error {
					t.Fatal("mismatched Git proof reached publication")
					return nil
				})
			require.ErrorIs(t, err, errRebuildInputSidecar)
		})
	}
}

package indexer

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

func TestRebuildModuleInputPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		link, disabled   bool
		mismatch         bool
		wantErr          error
	}{
		{name: "admitted_go_link", path: "go.mod", body: "module example.test/project\nrequire example.test/lib v1.2.3\n", link: true},
		{name: "lock_link_not_followed", path: "yarn.lock", body: "pkg@^1:\n  version \"1.0.0\"\n", link: true, wantErr: errRebuildResolverInputDenied},
		{name: "modules_off_leaves_lock_unconsulted", path: "yarn.lock", body: "ignored", link: true, disabled: true},
		{name: "too_large_lock_refuses", path: "yarn.lock", body: strings.Repeat(" ", rebuildCppEntryLimit+1), wantErr: errRebuildCppCaptureLimit},
		{name: "ambiguous_npm_lock_refuses", path: "package-lock.json", body: `{"lockfileVersion":3,"packages":{"lib":{"version":"1"},"node_modules/lib":{"version":"2"}}}`, wantErr: errRebuildModuleInput},
		{name: "digest_mismatch_refuses", path: "go.mod", body: "module example.test/project\n", mismatch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			root, src := rebuildInputFixture(t)
			const target = "root-manifest-data"
			if tc.link {
				writeRebuildTSAliasFixtureFile(t, root, target, tc.body)
				require.NoError(t, os.Symlink(target, filepath.Join(root, tc.path)))
			} else {
				writeRebuildTSAliasFixtureFile(t, root, tc.path, tc.body)
			}
			reg := parser.NewRegistry()
			languages.RegisterAll(reg)
			cfg := config.Default().Index
			idx := New(graph.New(), reg, cfg, zap.NewNop())
			t.Cleanup(idx.Close)
			req := rebuildBuildRequest{Root: root, Witness: store_pebble.Witness{Recipe: store_pebble.HeadDigest{1}}}
			err := withRebuildCheckoutBuildInputs(ctx, t.TempDir(), src, req, nil, false,
				func(key string, mode fs.FileMode, member rebuildGitMember, raw *int64, emit func(rebuildSidecarRow) error) (rebuildCheckoutLeaf, error) {
					return idx.freezeRebuildCheckoutLeaf(ctx, src.FrozenRoot(), key, mode, member, nil, rebuildCppEntryLimit+1, raw, emit)
				}, func(epoch rebuildCheckoutInputs, raw *int64, emit func(rebuildSidecarRow) error) (string, error) {
					return captureRebuildCheckoutFacts(ctx, src, epoch, true, !tc.disabled && cfg.Coverage.IsEnabled("modules"), raw, func(r rebuildSidecarRow) error {
						if tc.mismatch && r.Kind == rebuildModuleFact && string(r.Path) == tc.path {
							r.Digest[0] ^= 0x80 // valid row, but conflicts with the resolver manifest digest
						}
						return emit(r)
					})
				}, func(lease string, bound rebuildBuildRequest, inputs []rebuildInput) error {
					require.NoError(t, tc.wantErr, "a refused capture must not hand out a lease")
					digest := rebuildDigest(bound.Witness.FrozenInputs)
					if tc.disabled {
						return visitRebuildInputSidecar(ctx, lease, digest, func(r rebuildSidecarRow) error {
							require.NotEqual(t, rebuildModuleFact, r.Kind)
							return nil
						})
					}
					if tc.mismatch {
						_, err := openRebuildModuleFacts(ctx, lease, digest, inputs)
						require.ErrorIs(t, err, errRebuildModuleInput)
						require.ErrorContains(t, err, "mismatched root")
						return nil
					}
					require.NoError(t, os.Remove(filepath.Join(root, target)))
					b, err := store_pebble.OpenWorkerBuild(filepath.Join(lease, "test-build"), store_pebble.FullProfile)
					require.NoError(t, err)
					defer func() { require.NoError(t, b.Close()) }()
					require.NoError(t, runRebuildModules(ctx, lease, digest, b, inputs))
					require.NotNil(t, b.GetNode("module::go:example.test/lib@v1.2.3"))
					return nil
				})
			if tc.wantErr != nil {
				require.ErrorIs(t, err, errRebuildModuleInput)
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

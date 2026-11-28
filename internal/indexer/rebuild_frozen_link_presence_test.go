package indexer

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/parser/tsalias"
	"go.uber.org/zap"
)

// A registered extractor can own an exact basename even when its extension
// suggests another language. The receipt, not the suffix, controls the cut.
type rebuildDeferredIndexExtractor struct{ *languages.JSONExtractor }

func (e rebuildDeferredIndexExtractor) Extensions() []string {
	return append(e.JSONExtractor.Extensions(), "index.ts")
}

func TestRebuildFrozenLinkCandidatePresence(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		name := "supported only"
		if mixed {
			name = "mixed"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			root, src := rebuildInputFixture(t)
			const caller = "app/main.ts"
			const first = "first/entry.json"
			const skipped = "first/big.json"
			const supportedSkip = "first/big.ts"
			const candidate = "packages/lib/index.ts"
			const alternate = "keep/entry.ts"
			for key, data := range map[string]string{
				caller:                      "export {}",
				first:                       `{"deferred": true}`,
				skipped:                     strings.Repeat(" ", 513),
				supportedSkip:               strings.Repeat(" ", 513),
				candidate:                   `{}`,
				alternate:                   "export {}",
				"packages/lib/keep.ts":      "export {}",
				"package.json":              `{"workspaces":["packages/*"],"dependencies":{"member":"npm:remote@1"}}`,
				"packages/lib/package.json": `{"name":"member"}`,
				"tsconfig.json":             `{"compilerOptions":{"paths":{
					"@source":["first/entry.json","keep/entry.ts"],
					"@skip":["first/big.json","keep/entry.ts"],
					"@supportedSkip":["first/big.ts","keep/entry.ts"],
					"@denied":["denied/entry.ts","keep/entry.ts"]}}}`,
				"denied/entry.ts": "export {}",
			} {
				writeRebuildTSAliasFixtureFile(t, root, key, data)
			}
			reg := parser.NewRegistry()
			reg.Register(languages.NewTypeScriptExtractor())
			if mixed {
				reg.Register(rebuildDeferredIndexExtractor{languages.NewJSONExtractor()})
			}
			cfg := config.Default().Index
			cfg.MaxFileSize = 512
			idx := New(graph.New(), reg, cfg, zap.NewNop())
			t.Cleanup(idx.Close)
			idx.installRebuildParsePolicy(freezeRebuildParsePolicy(root, cfg))
			idx.SetRootPath(root)
			req := rebuildBuildRequest{Root: root, Witness: store_pebble.Witness{Recipe: store_pebble.HeadDigest{1}}}
			err := withRebuildCheckoutBuildInputs(ctx, t.TempDir(), src, req, excludes.New([]string{"denied/"}), false,
				func(key string, mode fs.FileMode, member rebuildGitMember, raw *int64, emit func(rebuildSidecarRow) error) (rebuildCheckoutLeaf, error) {
					return idx.freezeRebuildCheckoutLeaf(ctx, src.FrozenRoot(), key, mode, member, nil, 1024, raw, emit)
				}, func(epoch rebuildCheckoutInputs, raw *int64, emit func(rebuildSidecarRow) error) (string, error) {
					return captureRebuildCheckoutResolverInputs(ctx, src, epoch, true, raw, emit)
				}, func(lease string, bound rebuildBuildRequest, inputs []rebuildInput) error {
					census := &rebuildExtractorCensus{admitted: uint64(len(inputs)), byPath: make(map[string]string)}
					store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
					for _, in := range inputs {
						facts, _, err := idx.loadRebuildFile(ctx, src, store, in, rebuildDigest{1}, rebuildTransformBounds{})
						require.NoError(t, err)
						census.byPath[in.Path] = facts.EffectiveLanguage
					}
					if mixed {
						require.Equal(t, "json", census.byPath[first])
						require.Equal(t, "json", census.byPath[candidate])
						require.Equal(t, "json", census.byPath["tsconfig.json"])
						require.Equal(t, "json", census.byPath["package.json"])
					} else {
						require.NotContains(t, census.byPath, first)
						require.NotContains(t, census.byPath, "tsconfig.json")
						require.NotContains(t, census.byPath, "package.json")
						require.Equal(t, "typescript", census.byPath[candidate])
					}
					require.NotContains(t, census.byPath, skipped)
					require.NotContains(t, census.byPath, supportedSkip)
					digest := rebuildDigest(bound.Witness.FrozenInputs)
					classifications := make(map[string]rebuildClassificationFact)
					require.NoError(t, visitRebuildInputSidecar(ctx, lease, digest, func(factRow rebuildSidecarRow) error {
						if factRow.Classification != nil {
							require.NoError(t, idx.checkRebuildClassification(string(factRow.Path), *factRow.Classification, idx.newContentAdmissionGate()))
							classifications[string(factRow.Path)] = *factRow.Classification
						}
						return nil
					}))
					require.Equal(t, mixed, rebuildHoldsOutCheckedSkip(classifications[skipped]))
					require.False(t, rebuildHoldsOutCheckedSkip(classifications[supportedSkip]))
					_, aliases, err := openRebuildResolverConfigFacts(ctx, lease, digest)
					require.NoError(t, err)
					ns, manifests, followed, err := openRebuildNpmManifestFacts(ctx, lease, digest)
					require.NoError(t, err)
					probe := rebuildLinkCandidateProbe(ns, census, classifications)
					for _, key := range []string{first, skipped, candidate, supportedSkip} {
						present, err := ns.probeFile(key)
						require.NoError(t, err)
						require.True(t, present, "candidate physically present: %s", key)
						present, err = probe(key)
						require.NoError(t, err)
						require.Equal(t, !mixed || key == supportedSkip, present, key)
					}
					for spec, original := range map[string]string{"@source": first, "@skip": skipped, "@supportedSkip": "first/big"} {
						before, err := aliases.resolve(ns.probeFile, caller, spec)
						require.NoError(t, err)
						require.Equal(t, tsalias.FrozenAliasStem(original), before)
						want := before
						if mixed && spec != "@supportedSkip" {
							want = "keep/entry"
						}
						got, err := aliases.resolve(probe, caller, spec)
						require.NoError(t, err)
						require.Equal(t, want, got, spec)
					}
					_, err = aliases.resolve(probe, caller, "@denied")
					require.ErrorIs(t, err, errRebuildResolverInputDenied)
					glob, err := newRebuildWorkspaceGlob(ctx, ns)
					require.NoError(t, err)
					npm := newRebuildNpmLookupFromNamespace(ctx, ns, manifests, followed, glob, ns.probeFile)
					before, err := npm.rewrite(caller, "member")
					require.NoError(t, err)
					require.Equal(t, "../packages/lib", before)
					want := before
					if mixed {
						want = "remote"
					}
					npm = newRebuildNpmLookupFromNamespace(ctx, ns, manifests, followed, glob, probe)
					got, err := npm.rewrite(caller, "member")
					require.NoError(t, err)
					require.Equal(t, want, got)
					rewrite := rebuildExternalNpmRewrite(ctx, lease, digest, census, classifications)
					got, err = rewrite(caller, "member")
					require.NoError(t, err)
					require.Equal(t, want, got)
					got, err = rewrite(caller, "member/keep")
					require.NoError(t, err)
					require.Equal(t, "../packages/lib/keep", got)
					return nil
				})
			require.NoError(t, err)
		})
	}
}

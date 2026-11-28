package indexer

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/modules"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

func TestRebuildFrozenWorkspaceMembership(t *testing.T) {
	const (
		caller      = "packages/z/sub/main.ts"
		first       = "packages/a/shared-local/index.ts"
		winner      = "packages/z/sub/shared-local/index.ts"
		pending     = "unresolved::import::shared-local"
		packageRoot = `{"workspaces":{"packages":[" packages/z ","packages/z/sub","spare/*"]}}`
		pnpmRoot    = "packages:\n  - 'packages/z/sub/'\n  - 'spare/*'\n"
		cargoRoot   = "[workspace]\nmembers = [\"packages/*\", \"packages/z/sub\"]\nexclude = [\"packages/a\"]\n"
	)
	for _, tc := range []struct {
		name, npm, pnpm, cargo string
		want                   string
		rows                   []string
		denied                 bool
	}{
		{name: "npm_before_pnpm", npm: packageRoot, pnpm: "packages: ['packages/a']", cargo: cargoRoot, want: winner},
		{name: "pnpm_before_cargo", npm: `{"workspaces":[" "]}`, pnpm: pnpmRoot,
			cargo: "[workspace]\nmembers = [\"packages/a\"]\n", want: winner, rows: []string{rebuildPnpmWorkspaceFile}},
		{name: "cargo_after_empty_pnpm", npm: `{}`, pnpm: "packages: []", cargo: cargoRoot, want: winner,
			rows: []string{rebuildPnpmWorkspaceFile, rebuildCargoWorkspaceFile}},
		{name: "proved_no_workspace", want: first},
		{name: "opaque_pnpm_stops_before_cargo", pnpm: pnpmRoot, cargo: cargoRoot, want: pending, denied: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			root, src := rebuildInputFixture(t)
			files := map[string]string{
				caller: `import 'shared-local'`, first: "export {}", winner: "export {}",
				"packages/a/package.json": `{}`, "packages/z/package.json": `{}`, "packages/z/sub/package.json": `{}`,
				"packages/a/Cargo.toml": "", "packages/z/Cargo.toml": "", "packages/z/sub/Cargo.toml": "",
				"spare/docs/readme.txt": "not a member",
			}
			for name, content := range map[string]string{"package.json": tc.npm, rebuildPnpmWorkspaceFile: tc.pnpm, rebuildCargoWorkspaceFile: tc.cargo} {
				if content != "" {
					files[name] = content
				}
			}
			for file, body := range files {
				writeRebuildTSAliasFixtureFile(t, root, file, body)
			}
			var flat *excludes.Matcher
			if tc.denied {
				flat = excludes.New([]string{rebuildPnpmWorkspaceFile})
			}
			native := newWorkspaceMembershipIndex(map[string]string{"": root})
			if tc.want == winner {
				require.Equal(t, "packages/z/sub", native.Lookup(caller))
				require.Equal(t, native.Lookup(caller), native.Lookup(winner))
				require.Empty(t, native.Lookup(first))
				require.Empty(t, native.Lookup("spare/docs/readme.txt"))
			} else if !tc.denied {
				require.Empty(t, native.Lookup(caller))
			}

			reg := parser.NewRegistry()
			languages.RegisterAll(reg)
			idx := New(graph.New(), reg, config.Default().Index, zap.NewNop())
			t.Cleanup(idx.Close)
			t.Setenv("GORTEX_TEST_REBUILD_BUILD_CHILD", "1")
			req := rebuildBuildRequest{Root: root, Witness: store_pebble.Witness{Recipe: store_pebble.HeadDigest{1}}}
			link := rebuildTestLinkOptions(t)
			err := withRebuildCheckoutBuildInputs(ctx, t.TempDir(), src, req, flat, false,
				func(key string, mode fs.FileMode, member rebuildGitMember, raw *int64, emit func(rebuildSidecarRow) error) (rebuildCheckoutLeaf, error) {
					return idx.freezeRebuildCheckoutLeaf(ctx, src.FrozenRoot(), key, mode, member, nil, 1024, raw, emit)
				}, func(epoch rebuildCheckoutInputs, raw *int64, emit func(rebuildSidecarRow) error) (string, error) {
					return captureRebuildCheckoutResolverInputs(ctx, src, epoch, true, raw, emit)
				}, func(lease string, bound rebuildBuildRequest, inputs []rebuildInput) error {
					digest := rebuildDigest(bound.Witness.FrozenInputs)
					var declarationRows []string
					require.NoError(t, visitRebuildInputSidecar(ctx, lease, digest, func(row rebuildSidecarRow) error {
						if row.Kind == rebuildWorkspaceConfigRow {
							declarationRows = append(declarationRows, string(row.Path))
						}
						return nil
					}))
					require.Equal(t, tc.rows, declarationRows)
					// Replay can no longer see the workspace at its original path.
					require.NoError(t, os.Rename(root, filepath.Join(t.TempDir(), "relocated[ab]")))
					require.Empty(t, newWorkspaceMembershipIndex(map[string]string{"": root}).Lookup(caller))
					if !tc.denied {
						ns, npm, followed, err := openRebuildNpmManifestFacts(ctx, lease, digest)
						require.NoError(t, err)
						manifest, err := openRebuildWorkspaceManifest(ctx, lease, digest, ns, npm["package.json"], followed)
						require.NoError(t, err)
						glob, err := newRebuildWorkspaceGlob(ctx, ns)
						require.NoError(t, err)
						members, err := newRebuildWorkspaceMembers(ctx, glob, manifest)
						require.NoError(t, err)
						for _, file := range []string{caller, first, winner, "packages/z/top.ts", "spare/docs/readme.txt"} {
							require.Equal(t, native.Lookup(file), rebuildWorkspaceMember(members, file), file)
						}
					}
					build, err := store_pebble.OpenBuild(filepath.Join(lease, "working"), store_pebble.FullProfile)
					require.NoError(t, err)
					defer func() { require.NoError(t, build.Close()) }()
					census := &rebuildExtractorCensus{admitted: uint64(len(inputs)), byPath: map[string]string{}, originByGraphPath: map[string]string{}}
					for _, in := range inputs {
						census.byPath[in.Path] = in.Language
						census.originByGraphPath[in.Path] = in.Path
					}
					for _, file := range []string{caller, first, winner} {
						require.Equal(t, "typescript", census.byPath[file])
						build.AddNode(&graph.Node{ID: file, Kind: graph.KindFile, FilePath: file, Language: "typescript"})
					}
					edge := graph.Edge{From: caller, To: pending, Kind: graph.EdgeImports, FilePath: caller, Line: 3}
					build.AddEdge(&edge)
					err = runRebuildFrozenMain(ctx, lease, digest, build, census, nil, link, bound.SkipPlaceholderSources)
					if tc.denied {
						require.ErrorIs(t, err, errRebuildResolverInputDenied)
						require.ErrorContains(t, err, rebuildPnpmWorkspaceFile)
						require.Equal(t, buildRefusal(errRebuildResolverInputDenied).Refusal, buildRefusal(err).Refusal)
					} else {
						require.NoError(t, err)
					}
					var rows []graph.Edge
					require.NoError(t, build.RebuildView().VisitEdges(ctx, func(e *graph.Edge) error {
						rows = append(rows, *e)
						return nil
					}))
					edge.To = tc.want
					require.Equal(t, []graph.Edge{edge}, rows)
					return nil
				})
			require.NoError(t, err)
		})
	}
}

func TestRebuildWorkspaceMemberRefusals(t *testing.T) {
	g := recordTestRebuildWorkspaceGlob(t, []rebuildNamespaceRow{
		{"members", rebuildNamespaceDirectory},
		{"members/a", rebuildNamespaceDirectory},
		{"members/a/package.json", rebuildNamespaceDenied},
		{"members/a", rebuildNamespaceCloseExact},
		{"members/opaque", rebuildNamespaceDenied},
		{"members", rebuildNamespaceCloseExact},
		{".", rebuildNamespaceCloseExact},
	})()
	for _, p := range []string{"members/a", "members/o*"} {
		got, err := newRebuildWorkspaceMembers(t.Context(), g, &modules.WorkspaceManifest{Patterns: []string{p}})
		require.Nil(t, got)
		require.ErrorIs(t, err, errRebuildResolverInputDenied)
	}
	m := &modules.WorkspaceManifest{Patterns: []string{"../outside/*"}}
	got, err := newRebuildWorkspaceMembers(t.Context(), g, m)
	require.Nil(t, got)
	require.ErrorIs(t, err, errRebuildResolverNamespace)

	// An exact miss uses only the closed listing, but many repeats are
	// still charged to one work budget, even with no retained results.
	missing := strings.Repeat("x", 1023)
	m.Patterns = slices.Repeat([]string{missing}, rebuildNpmFactBytes/(len(missing)+1)+1)
	got, err = newRebuildWorkspaceMembers(t.Context(), g, m)
	require.Nil(t, got)
	require.ErrorIs(t, err, errRebuildCppCaptureLimit)
}

func TestRebuildWorkspaceConfigRow(t *testing.T) {
	row := rebuildSidecarRow{Kind: rebuildWorkspaceConfigRow, Path: []byte(rebuildCargoWorkspaceFile),
		Present: true, Digest: rebuildDigest{1}, Workspace: &rebuildWorkspacePayload{
			Patterns: [][]byte{[]byte("packages/*")}, Exclude: [][]byte{[]byte("packages/x")},
		}}
	require.NoError(t, row.check())
	invalid := row
	invalid.Path = []byte(rebuildPnpmWorkspaceFile)
	require.ErrorIs(t, invalid.check(), errRebuildInputSidecar)
	invalid = row
	invalid.Kind = rebuildNpmManifestRow
	require.ErrorIs(t, invalid.check(), errRebuildInputSidecar)
	invalid = row
	invalid.Workspace = &rebuildWorkspacePayload{Patterns: [][]byte{[]byte(strings.Repeat("x", rebuildCppEntryLimit))}}
	require.ErrorIs(t, invalid.check(), errRebuildCppCaptureLimit)
}

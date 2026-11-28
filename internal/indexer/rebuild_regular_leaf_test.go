package indexer

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer/merkle"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

func TestFreezeRebuildRegularLeaf(t *testing.T) {
	const shebang = "#!/bin/sh\n"
	const parserLimit = 128
	for _, tc := range []struct {
		name, body, language string
		member               rebuildGitMember
		listed               fs.FileMode
		cap, charged         int64
		rows                 int
		wantParse, sizeSkip  bool
		wantErr              error
	}{
		{name: "main.go", body: "package demo\n", language: "go", member: rebuildGitTracked, wantParse: true},
		{name: "script", body: shebang + "echo hello\n", language: "bash", member: rebuildGitUntracked, cap: 512, charged: 21, rows: 1, wantParse: true},
		{name: "oversized", body: shebang + strings.Repeat("#", 600), language: "bash", member: rebuildGitTracked, cap: 512, charged: 512, rows: 1, sizeSkip: true},
		{name: "oversized.go", body: strings.Repeat(" ", 600), language: "go", member: rebuildGitTracked, rows: 1, sizeSkip: true},
		{name: "unknown", body: "no language", member: rebuildGitTracked, cap: 11, charged: 11, rows: 1},
		{name: "ignored", body: shebang, member: 0},
		{name: "link", body: shebang, member: rebuildGitTracked, listed: fs.ModeSymlink},
		{name: "too-little-prefix-budget", body: shebang, member: rebuildGitUntracked, cap: 1, wantErr: errRebuildCppCaptureLimit},
		{name: "replaced-by-link", body: shebang, member: rebuildGitTracked, cap: 512, wantErr: ErrBuildInputChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			root, src := rebuildInputFixture(t)
			path := filepath.Join(root, tc.name)
			if tc.name == "link" || tc.name == "replaced-by-link" {
				require.NoError(t, os.WriteFile(filepath.Join(root, "target"), []byte(tc.body), 0o600))
				require.NoError(t, os.Symlink("target", path))
			} else {
				require.NoError(t, os.WriteFile(path, []byte(tc.body), 0o600))
			}
			cfg := config.Default().Index
			cfg.MaxFileSize = parserLimit
			reg := parser.NewRegistry()
			languages.RegisterAll(reg)
			idx := New(graph.New(), reg, cfg, zap.NewNop())
			t.Cleanup(idx.Close)
			remaining := tc.cap
			var rows []rebuildSidecarRow
			input, _, err := idx.freezeRebuildRegularLeaf(ctx, src.FrozenRoot(), tc.name, tc.listed,
				tc.member, idx.newContentAdmissionGate(), parserLimit, &remaining, func(row rebuildSidecarRow) error {
					require.NoError(t, row.check())
					rows = append(rows, row)
					return nil
				})
			require.ErrorIs(t, err, tc.wantErr)
			require.Equal(t, tc.charged, tc.cap-remaining)
			require.Len(t, rows, tc.rows)
			if len(rows) > 0 {
				fact := rows[0].Classification
				require.Equal(t, tc.sizeSkip, fact.sizeSkipped())
				require.Equal(t, tc.language, fact.Language)
				if fact.PrefixBytes > 0 {
					require.Equal(t, rebuildDigest(blake3.Sum256([]byte(tc.body[:fact.PrefixBytes]))), fact.PrefixDigest)
				}
			}
			if tc.wantParse {
				require.Equal(t, &rebuildInput{Path: tc.name, Kind: merkle.RegularFile, Size: int64(len(tc.body)),
					Language: tc.language, Content: blake3.Sum256([]byte(tc.body))}, input)
			} else {
				require.Nil(t, input)
			}
		})
	}
}

func TestFreezeRebuildRegularLeafPreservesContentSkips(t *testing.T) {
	ctx := t.Context()
	root, src := rebuildInputFixture(t)
	cfg := config.Default().Index
	cfg.MaxFileSize = 1024
	cfg.Content.MaxImageBytes = 256
	cfg.SkipUntrackedAssets = true
	idx := New(graph.New(), newAssetTestRegistry(), cfg, zap.NewNop())
	t.Cleanup(idx.Close)
	const key = "large.gif"
	require.NoError(t, os.WriteFile(filepath.Join(root, key), make([]byte, 600), 0o600))
	for _, tc := range []struct {
		member rebuildGitMember
		reason string
	}{{rebuildGitTracked, skipReasonLargeImage}, {rebuildGitUntracked, skipReasonUntrackedAsset}} {
		var raw int64
		var rows []rebuildSidecarRow
		// A known-name asset skip is decided before the parse byte limit
		// and produces no input, even with no room for a content read.
		input, _, err := idx.freezeRebuildRegularLeaf(ctx, src.FrozenRoot(), key, 0, tc.member, idx.newContentAdmissionGate(), 1, &raw,
			func(row rebuildSidecarRow) error { rows = append(rows, row); return row.check() })
		require.NoError(t, err)
		require.Nil(t, input)
		require.Len(t, rows, 1)
		require.Equal(t, tc.reason, rows[0].Classification.ContentSkipReason)
		require.Zero(t, rows[0].Classification.PrefixBytes)
		require.Zero(t, raw)
	}
}

func TestReadRebuildClassifiedBodyRefusesDrift(t *testing.T) {
	ctx := t.Context()
	root, src := rebuildInputFixture(t)
	name := filepath.Join(root, "script")
	require.NoError(t, os.WriteFile(name, []byte("#!/bin/sh\n"+strings.Repeat("a", 600)), 0o600))
	obs, err := source.ObserveFrozenLiteralRootPath(ctx, src.FrozenRoot(), "script")
	require.NoError(t, err)
	prefix, err := obs.ReadPrefix(ctx, sniffPrefixBytes)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(name, []byte("#!/bin/sh\n"+strings.Repeat("b", 600)), 0o600))
	fresh, err := source.ObserveFrozenLiteralRootPath(ctx, src.FrozenRoot(), "script")
	require.NoError(t, err)
	// Even if existing metadata checks alone could accept the fresh
	// observation, no body may be bound to a different prefix.
	data, err := readRebuildClassifiedBody(ctx, fresh, 1024, prefix)
	require.ErrorIs(t, err, ErrBuildInputChanged)
	require.Nil(t, data)
}

func TestFreezeRebuildCheckoutRootManifestLinks(t *testing.T) {
	ctx := t.Context()
	dir, src := rebuildInputFixture(t)
	const target = "fixture.go"
	const body = "module example.org/fixture\n"
	const outside = "../outside.json"
	require.NoError(t, os.WriteFile(filepath.Join(dir, target), []byte(body), 0o600))
	require.NoError(t, os.Symlink(target, filepath.Join(dir, "go.mod")))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "package.json")))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "unrelated.go")))

	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	idx := New(graph.New(), reg, config.Default().Index, zap.NewNop())
	t.Cleanup(idx.Close)
	gate := idx.newContentAdmissionGate()
	raw := int64(rebuildCppRawLimit)
	epoch, err := freezeRebuildCheckoutInputs(ctx, src.FrozenRoot(), nil, nil, &raw,
		func(key string, mode fs.FileMode, member rebuildGitMember, raw *int64, emit func(rebuildSidecarRow) error) (rebuildCheckoutLeaf, error) {
			return idx.freezeRebuildCheckoutLeaf(ctx, src.FrozenRoot(), key, mode, member, gate, 128, raw, emit)
		}, func(row rebuildSidecarRow) error { return row.check() })
	require.NoError(t, err)
	require.Len(t, epoch.files, 1)
	require.Equal(t, target, epoch.files[0].Path)
	require.Equal(t, &rebuildInput{Path: "go.mod", Language: "manifest", Kind: merkle.Symlink,
		Size: int64(len(target)), Content: blake3.Sum256([]byte(target))}, epoch.admitted["go.mod"])
	require.Equal(t, &rebuildInput{Path: "package.json", Language: "manifest", Kind: merkle.Symlink,
		Size: int64(len(outside)), Content: blake3.Sum256([]byte(outside))}, epoch.admitted["package.json"])
	_, err = epoch.namespace.lookup("unrelated.go")
	require.ErrorIs(t, err, errRebuildResolverInputDenied)
	require.NotContains(t, epoch.admitted, "unrelated.go")

	m, err := readCheckedRebuildRootManifest(ctx, src, epoch, "go.mod", 128)
	require.NoError(t, err)
	require.NotNil(t, m)
	require.Equal(t, body, string(m.data))
	require.Equal(t, *epoch.admitted[target], m.target)
	require.Equal(t, m.target.Content, m.row.Digest)

	m, err = readCheckedRebuildRootManifest(ctx, src, epoch, "package.json", 128)
	require.ErrorIs(t, err, source.ErrOutsideRoot)
	require.Nil(t, m)

	small := int64(len(target) - 1)
	leaf, err := freezeRebuildRootManifestLink(ctx, src.FrozenRoot(), "go.mod", &small)
	require.ErrorIs(t, err, errRebuildCppCaptureLimit)
	require.Empty(t, leaf)
	require.EqualValues(t, len(target)-1, small)
	before := int64(len(target))
	leaf, err = freezeRebuildRootManifestLink(ctx, src.FrozenRoot(), "go.mod", &before)
	require.NoError(t, err)
	require.Zero(t, before)
	require.Equal(t, epoch.admitted["go.mod"], leaf.input)

	require.NoError(t, os.Remove(filepath.Join(dir, "go.mod")))
	require.NoError(t, os.Symlink("unrelated.go", filepath.Join(dir, "go.mod")))
	m, err = readCheckedRebuildRootManifest(ctx, src, epoch, "go.mod", 128)
	require.ErrorIs(t, err, ErrBuildInputChanged)
	require.Nil(t, m)
}

func TestRebuildCheckoutGoConfigObservations(t *testing.T) {
	ctx := t.Context()
	dir, src := rebuildInputFixture(t)
	const mod = "module example.org/rootmod\n"
	const childMod = "module example.org/sub\n"
	for key, body := range map[string]string{
		"go.mod": mod, "sub/go.mod": childMod, "sub/lib.go": "package sub\n",
		"spare/go.mod": "module example.org/unused\n",
	} {
		file := filepath.Join(dir, filepath.FromSlash(key))
		require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o700))
		require.NoError(t, os.WriteFile(file, []byte(body), 0o600))
	}
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	idx := New(graph.New(), reg, config.Default().Index, zap.NewNop())
	t.Cleanup(idx.Close)
	raw := int64(rebuildCppRawLimit)
	epoch, err := freezeRebuildCheckoutInputs(ctx, src.FrozenRoot(), nil, nil, &raw,
		func(key string, mode fs.FileMode, member rebuildGitMember, raw *int64, emit func(rebuildSidecarRow) error) (rebuildCheckoutLeaf, error) {
			return idx.freezeRebuildCheckoutLeaf(ctx, src.FrozenRoot(), key, mode, member, nil, 1024, raw, emit)
		}, func(row rebuildSidecarRow) error { return row.check() })
	require.NoError(t, err)
	require.Len(t, epoch.files, 1)
	require.Len(t, epoch.admitted, 1)
	require.Len(t, epoch.configObservations, 3)
	// Removing an unconsulted manifest does not force a read or refusal.
	require.NoError(t, os.Remove(filepath.Join(dir, "spare", "go.mod")))
	left := int64(len(mod) + len(childMod))
	goRows := make(map[string]rebuildSidecarRow)
	name, err := captureRebuildCheckoutResolverInputs(ctx, src, epoch, true, &left, func(row rebuildSidecarRow) error {
		if row.Kind == rebuildGoManifestFact {
			goRows[string(row.Path)] = row
		}
		return row.check()
	})
	require.NoError(t, err)
	require.Equal(t, "rootmod", name)
	require.Zero(t, left, "one body allowance per consulted config")
	require.NotContains(t, goRows, "spare/go.mod")
	for key, body := range map[string]string{"go.mod": mod, "sub/go.mod": childMod} {
		row := goRows[key]
		require.Equal(t, rebuildDigest(blake3.Sum256([]byte(body))), row.Digest)
		require.NotNil(t, row.Config)
		require.Equal(t, strings.TrimSpace(strings.TrimPrefix(body, "module ")), row.Config.OwnerModule)
	}
	left = int64(len(mod) + len(childMod))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "go.mod"), []byte(childMod+"\n"), 0o600))
	name, err = captureRebuildCheckoutResolverInputs(ctx, src, epoch, true, &left, func(rebuildSidecarRow) error {
		t.Fatal("emitted a manifest after selected config drift")
		return nil
	})
	require.ErrorIs(t, err, ErrBuildInputChanged)
	require.Empty(t, name)
}

func TestRebuildCheckoutJSConfigObservations(t *testing.T) {
	ctx := t.Context()
	root, src := rebuildInputFixture(t)
	const rootNpm = `{"name":"fixture-npm","workspaces":["pkg/*"]}`
	const innerNpm = `{"name":"inner-npm","dependencies":{"dep":"1.2.3"}}`
	const tsConfig = `{"compilerOptions":{"baseUrl":".","paths":{"@a/*":["lib/*"]}}}`
	for key, body := range map[string]string{
		"package.json": rootNpm, "pkg/inner/package.json": innerNpm,
		"tsconfig.json": tsConfig, "unused/jsconfig.json": `{"compilerOptions":{}}`,
		"main.ts": "export const A = 1", "lib/x.ts": "export const X = 2",
	} {
		file := filepath.Join(root, filepath.FromSlash(key))
		require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o700))
		require.NoError(t, os.WriteFile(file, []byte(body), 0o600))
	}
	reg := parser.NewRegistry()
	reg.Register(languages.NewTypeScriptExtractor())
	idx := New(graph.New(), reg, config.Default().Index, zap.NewNop())
	t.Cleanup(idx.Close)
	var epoch rebuildCheckoutInputs
	lease := t.TempDir()
	digest, err := writeRebuildInputSidecar(ctx, lease, func(emit func(rebuildSidecarRow) error) error {
		raw := int64(rebuildCppRawLimit)
		var err error
		epoch, err = freezeRebuildCheckoutInputs(ctx, src.FrozenRoot(), nil, nil, &raw,
			func(key string, mode fs.FileMode, member rebuildGitMember, raw *int64, emit func(rebuildSidecarRow) error) (rebuildCheckoutLeaf, error) {
				return idx.freezeRebuildCheckoutLeaf(ctx, src.FrozenRoot(), key, mode, member, nil, 1024, raw, emit)
			}, emit)
		if err != nil {
			return err
		}
		require.Len(t, epoch.files, 2)
		require.Len(t, epoch.admitted, 2)
		require.Len(t, epoch.configObservations, 4)
		require.NoError(t, os.Remove(filepath.Join(root, "unused", "jsconfig.json")))
		before := raw
		name, err := captureRebuildCheckoutResolverInputs(ctx, src, epoch, true, &raw, emit)
		if err == nil {
			require.Equal(t, "fixture-npm", name)
			require.EqualValues(t, len(rootNpm)+len(innerNpm)+len(tsConfig), before-raw)
		}
		return err
	})
	require.NoError(t, err)
	_, aliases, err := openRebuildResolverConfigFacts(ctx, lease, digest)
	require.NoError(t, err)
	alias, err := aliases.resolve(epoch.namespace.probeFile, "main.ts", "@a/x")
	require.NoError(t, err)
	require.EqualValues(t, "lib/x", alias)
	_, npm, _, err := openRebuildNpmManifestFacts(ctx, lease, digest)
	require.NoError(t, err)
	require.Len(t, npm, 2)
	require.Equal(t, "fixture-npm", npm["package.json"].Manifest.name)
	require.Equal(t, "inner-npm", npm["pkg/inner/package.json"].Manifest.name)
	require.Equal(t, rebuildDigest(blake3.Sum256([]byte(innerNpm))), npm["pkg/inner/package.json"].Digest)

	require.NoError(t, os.WriteFile(filepath.Join(root, "pkg", "inner", "package.json"), []byte(innerNpm+"\n"), 0o600))
	raw := int64(rebuildCppRawLimit)
	name, err := captureRebuildCheckoutResolverInputs(ctx, src, epoch, true, &raw, func(rebuildSidecarRow) error { return nil })
	require.ErrorIs(t, err, ErrBuildInputChanged)
	require.Empty(t, name)
}

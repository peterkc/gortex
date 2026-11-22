//go:build unix

package indexer

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/parser/tsalias"
)

func TestRebuildTSAliasConfigsNativeReplay(t *testing.T) {
	for _, basename := range []string{"repo", "repo[1]", "build"} {
		t.Run(basename, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), basename)
			const rootConfig = `{"compilerOptions":{"paths":{"@lib/*":["shared/*"]}}}`
			const badChoice = `{"compilerOptions":{"paths":{"@lib/*":["wrong/*"]}}}`
			files := map[string]string{
				"jsconfig.json":                rootConfig,
				"tsconfig.json":                badChoice,
				"app/jsconfig.json":            `{"extends":"../base.json",`,
				"app/tsconfig.json":            `{"extends":"../base.json"}`,
				"app/deep/TSConfig.json":       badChoice,
				"app/shadow/tsconfig.json":     `{"compilerOptions":{"paths":{"@lib/*":[]}}}`,
				"build/deep/jsconfig.json":     badChoice,
				"linked-target/jsconfig.json":  badChoice,
				"base.json":                    badChoice,
				"shared/x.ts":                  "",
				"app/deep/main.ts":             "",
				"app/deep/also.js":             "",
				"app/shadow/main.js":           "",
				"build/deep/ignored-config.ts": "",
				"linked-target/main.ts":        "",
			}
			src, frozen := newRebuildTSAliasFixture(t, root, files)
			// Native reports this name as a directory, not a config file.
			require.NoError(t, os.Mkdir(filepath.Join(root, "app/deep/jsconfig.json"), 0o700))
			require.NoError(t, os.Symlink("linked-target", filepath.Join(root, "link")))
			ctx := t.Context()
			var ns rebuildResolverNamespace
			namespaceRaw := int64(rebuildCppRawLimit)
			for _, dir := range []string{".", "app", "app/deep", "app/shadow"} {
				require.NoError(t, captureRebuildResolverDirectory(ctx, src.FrozenRoot(), dir, false,
					func(key string, _ fs.FileMode) (bool, error) { return key != "build", nil },
					&ns, &namespaceRaw, func(rebuildSidecarRow) error { return nil }))
			}
			require.NoError(t, ns.freeze())
			points := []struct{ caller, want string }{
				{"app/deep/main.ts", "shared/x"},
				{"app/deep/also.js", "shared/x"},
				{"app/shadow/main.js", ""},
				{"build/deep/ignored-config.ts", "shared/x"},
				{"link/main.ts", "shared/x"},
			}
			var callers []rebuildInput
			for _, p := range points {
				language := "typescript"
				if strings.HasSuffix(p.caller, ".js") {
					language = "javascript"
				}
				callers = append(callers, rebuildInput{Path: p.caller, Language: language})
			}
			// Pass identities only for eligible names. A read of the case
			// variant, symlink subtree, pruned config or extends would fail.
			identities := make(map[string]*rebuildInput)
			var wantRaw int64
			for _, key := range []string{"jsconfig.json", "tsconfig.json", "app/jsconfig.json", "app/tsconfig.json", "app/shadow/tsconfig.json"} {
				in := frozen[key]
				identities[key] = &in
				wantRaw += in.Size
			}
			raw := namespaceRaw
			rows, err := captureRebuildTSAliasConfigs(ctx, src.FrozenRoot(), true, &ns, callers, identities, &raw)
			require.NoError(t, err)
			require.Equal(t, namespaceRaw-wantRaw, raw)
			require.Len(t, rows, 14) // seven directories, two exact candidate names each
			for key, in := range identities {
				row, err := rows.lookup(key)
				require.NoError(t, err)
				require.True(t, row.Present)
				require.Equal(t, in.Content, row.Digest)
			}
			require.Nil(t, rows["app/jsconfig.json"].Config)
			require.Nil(t, rows["app/tsconfig.json"].Config)
			require.NotNil(t, rows["app/shadow/tsconfig.json"].Config)
			require.Empty(t, rows["app/shadow/tsconfig.json"].Config.Entries)
			for _, key := range []string{"app/deep/jsconfig.json", "app/deep/tsconfig.json", "link/jsconfig.json"} {
				row, err := rows.lookup(key)
				require.NoError(t, err)
				require.False(t, row.Present)
			}
			_, err = rows.lookup("linked-target/jsconfig.json")
			require.ErrorIs(t, err, tsalias.ErrFrozenAliasUnproved)

			native := tsalias.Load(root)
			for _, p := range points {
				want := p.want
				if basename == "build" {
					// Native WalkDir also tests the checkout basename against
					// SkipDirName. AC-54 keeps this exclusion inside the root.
					want = ""
				}
				require.Equal(t, want, tsalias.Resolve(native.FindForFile(p.caller), "@lib/x"), p.caller)
			}
			writeRebuildTSAliasFixtureFile(t, root, "jsconfig.json", badChoice)
			require.NoError(t, os.Remove(filepath.Join(root, "app/shadow/tsconfig.json")))
			for _, p := range points {
				stem, err := tsalias.ResolveFrozenAlias(p.caller, "@lib/x", rows.config, func(key string) (bool, error) {
					// The frozen target receipts are a separate fixture input.
					require.Contains(t, []string{"shared/x", "shared/x.ts"}, key)
					return key == "shared/x.ts", nil
				})
				require.NoError(t, err)
				require.Equal(t, tsalias.FrozenAliasStem(p.want), stem, p.caller)
			}
			var unbound rebuildTSAliasConfigs
			_, err = tsalias.ResolveFrozenAlias("app/main.ts", "@lib/x", unbound.config, nil)
			require.ErrorIs(t, err, tsalias.ErrFrozenAliasUnproved)
		})
	}
}

func TestRebuildTSAliasConfigsPostParseProbeReplay(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	src, ids := newRebuildTSAliasFixture(t, root, map[string]string{
		"tsconfig.json": `{"compilerOptions":{"paths":{"@lib/*":["missing/*","shared/*"]}}}`,
		"app/main.ts":   `import "@lib/x"`,
		"shared/x.ts":   "export {}",
	})
	const caller, spec = "app/main.ts", "@lib/x"
	native := tsalias.Load(root)
	require.Equal(t, "shared/x", tsalias.Resolve(native.FindForFile(caller), spec))
	cfg, input := ids["tsconfig.json"], ids[caller]
	input.Language = "typescript"
	lease := t.TempDir()
	digest, err := writeRebuildInputSidecar(ctx, lease, func(emit func(rebuildSidecarRow) error) error {
		raw := int64(rebuildCppRawLimit)
		ns, err := captureRebuildCheckoutNamespace(ctx, src.FrozenRoot(), nil, &raw, func(key string, _ fs.FileMode) (bool, error) {
			_, ok := ids[key]
			return ok, nil
		}, emit)
		if err != nil {
			return err
		}
		rows, err := captureRebuildTSAliasConfigs(ctx, src.FrozenRoot(), true, ns, []rebuildInput{input},
			map[string]*rebuildInput{"tsconfig.json": &cfg}, &raw)
		if err != nil {
			return err
		}
		for _, key := range []string{"app/jsconfig.json", "app/tsconfig.json", "jsconfig.json", "tsconfig.json"} {
			fact, err := rows.lookup(key)
			if err != nil {
				return err
			}
			row, err := rebuildTSAliasConfigSidecarRow(fact)
			if err != nil {
				return err
			}
			if err := emit(row); err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)

	// Only after sealing the input does the parser's import become a
	// resolver query. A new live lookup chooses a different expansion.
	require.NoError(t, os.Remove(filepath.Join(root, "shared/x.ts")))
	writeRebuildTSAliasFixtureFile(t, root, "missing/x.ts", "export {}")
	native = tsalias.Load(root)
	require.Equal(t, "missing/x", tsalias.Resolve(native.FindForFile(caller), spec))
	_, configs, err := openRebuildResolverConfigFacts(ctx, lease, digest)
	require.NoError(t, err)
	ns, err := openRebuildResolverNamespace(ctx, lease, digest)
	require.NoError(t, err)
	stem, err := configs.resolve(ns, caller, spec)
	require.NoError(t, err)
	require.Equal(t, tsalias.FrozenAliasStem("shared/x"), stem)
}

func TestRebuildTSAliasConfigsRefusals(t *testing.T) {
	const key = "jsconfig.json"
	const initial = `{"compilerOptions":{"paths":{"x":["a"]}}}`
	for _, tc := range []struct {
		name            string
		kind            rebuildNamespaceKind
		close, identity bool
		remaining       int64
		mutate          string
		want            error
		text            string
	}{
		{"unclosed", rebuildNamespaceRegular, false, true, 64, "", tsalias.ErrFrozenAliasUnproved, "not closed"},
		{"denied", rebuildNamespaceDenied, true, true, 64, "", tsalias.ErrFrozenAliasUnproved, "denied"},
		{"unsupported leaf", rebuildNamespaceUnsupported, true, true, 64, "", tsalias.ErrFrozenAliasUnproved, "unsupported"},
		{"no identity", rebuildNamespaceRegular, true, false, 64, "", tsalias.ErrFrozenAliasUnproved, "no admitted regular identity"},
		{"raw cap", rebuildNamespaceRegular, true, true, 1, "", errRebuildTSAliasCaptureLimit, "raw config"},
		{"changed bytes", rebuildNamespaceRegular, true, true, 64, strings.Replace(initial, `"a"`, `"b"`, 1), ErrBuildInputChanged, "config bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			src, frozen := newRebuildTSAliasFixture(t, root, map[string]string{key: initial})
			var ns rebuildResolverNamespace
			require.NoError(t, ns.add(rebuildNamespaceRow{key: key, kind: tc.kind}))
			if tc.close {
				require.NoError(t, ns.add(rebuildNamespaceRow{key: ".", kind: rebuildNamespaceClose}))
			}
			require.NoError(t, ns.freeze())
			if tc.mutate != "" {
				writeRebuildTSAliasFixtureFile(t, root, key, tc.mutate)
			}
			ids := make(map[string]*rebuildInput)
			if tc.identity {
				in := frozen[key]
				ids[key] = &in
			}
			raw := tc.remaining
			rows, err := captureRebuildTSAliasConfigs(t.Context(), src.FrozenRoot(), true, &ns,
				[]rebuildInput{{Path: "main.ts", Language: "typescript"}}, ids, &raw)
			require.Nil(t, rows)
			require.ErrorIs(t, err, tc.want)
			require.ErrorContains(t, err, tc.text)
			if tc.mutate == "" {
				require.Equal(t, tc.remaining, raw) // rejected before reading config bodies
			}
		})
	}
}

func TestRebuildTSAliasConfigsNormalizedCap(t *testing.T) {
	// JSON targets are much smaller than the retained target slice. Each
	// record fits alone; both records must not survive one aggregate capture.
	body := `{"compilerOptions":{"paths":{"x":[` + strings.Repeat(`"a",`, 124000) + `"a"]}}}`
	files := map[string]string{"jsconfig.json": body, "tsconfig.json": body}
	src, frozen := newRebuildTSAliasFixture(t, t.TempDir(), files)
	var ns rebuildResolverNamespace
	ids := make(map[string]*rebuildInput)
	for key, in := range frozen {
		require.NoError(t, ns.add(rebuildNamespaceRow{key: key, kind: rebuildNamespaceRegular}))
		ids[key] = &in
	}
	require.NoError(t, ns.add(rebuildNamespaceRow{key: ".", kind: rebuildNamespaceClose}))
	require.NoError(t, ns.freeze())
	raw := int64(2 * len(body))
	rows, err := captureRebuildTSAliasConfigs(t.Context(), src.FrozenRoot(), true, &ns,
		[]rebuildInput{{Path: "main.ts", Language: "typescript"}}, ids, &raw)
	require.Nil(t, rows)
	require.ErrorIs(t, err, errRebuildTSAliasCaptureLimit)
	require.ErrorContains(t, err, `retained normalized config "tsconfig.json"`)
	require.Zero(t, raw) // each checked body fit the separate raw allowance
}

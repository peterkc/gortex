package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRebuildNpmInputsNative(t *testing.T) {
	// A deterministic in-memory transcript of the fixture, separate from the
	// working-copy tree used as the native oracle.
	files := map[string]string{
		"package.json":              `{"workspaces":["packages/*"],"dependencies":{"foo":"npm:real-foo@1","pkg":"^1.0.0"}}`,
		"packages/app/package.json": `{"dependencies":{"pkg":"workspace:*"}}`,
		"packages/lib/package.json": `{"name":"@demo/lib","exports":{"./feature":{"import":"./src/feature.js","default":"./other.js"}}}`,
		"packages/lib/index.ts":     "export {}",
	}
	facts := rebuildNpmInputs{
		readFile: func(_ context.Context, path string, limit int) ([]byte, bool, error) {
			value, present := files[path]
			require.LessOrEqual(t, len(value), limit)
			return []byte(value), present, nil
		},
		isFile: func(_ context.Context, path string) (bool, error) {
			_, present := files[path]
			return present, nil
		},
		matchDirs: func(_ context.Context, glob string, _ int) ([]string, error) {
			require.Equal(t, "packages/*", glob)
			return []string{"packages/app", "packages/lib"}, nil
		},
	}
	cases := []struct {
		caller, spec, rewrite string
		external              bool
	}{
		{"packages/app/src/use.ts", "@demo/lib", "../../lib", false},
		{"packages/app/src/use.js", "foo/sub", "real-foo/sub", true},
		{"packages/lib/src/use.ts", "@demo/lib/feature", "@demo/lib/src/feature.js", false},
		{"packages/app/src/use.ts", "pkg", "", false},
		{"src/use.ts", "pkg", "", true},
		{"packages/app/src/use.ts", "unknown", "", false},
		{"packages/app/src/use.go", "foo", "", false},
	}
	for range 2 {
		root := t.TempDir()
		for path, content := range files {
			name := filepath.Join(root, filepath.FromSlash(path))
			require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o700))
			require.NoError(t, os.WriteFile(name, []byte(content), 0o600))
		}
		native := newNpmAliasIndex(map[string]string{"": root})
		for _, tc := range cases {
			require.Equal(t, tc.rewrite, native.Resolve(tc.caller, tc.spec), "%#v", tc)
			require.Equal(t, tc.external, native.DeclaresExternalDependency(tc.caller, tc.spec), "%#v", tc)
		}
		require.NoError(t, os.Remove(filepath.Join(root, "package.json")))
		fresh := newNpmAliasIndex(map[string]string{"": root})
		require.Empty(t, fresh.Resolve(cases[1].caller, cases[1].spec), "live oracle changed")
		frozen := newRebuildNpmLookup(t.Context(), facts)
		for _, tc := range cases {
			got, err := frozen.rewrite(tc.caller, tc.spec)
			require.NoError(t, err)
			require.Equal(t, tc.rewrite, got, "%#v", tc)
			external, err := frozen.declaresExternal(tc.caller, tc.spec)
			require.NoError(t, err)
			require.Equal(t, tc.external, external, "%#v", tc)
		}
	}
}

func TestRebuildNpmInputsRefusesUnknownAndOverLimit(t *testing.T) {
	unproved := errors.New("path not admitted")
	for _, oversized := range []bool{false, true} {
		calls := 0
		lookup := newRebuildNpmLookup(t.Context(), rebuildNpmInputs{
			readFile: func(_ context.Context, path string, limit int) ([]byte, bool, error) {
				calls++
				require.Equal(t, "package.json", path, "do not consult a later manifest after an unknown")
				if oversized {
					return make([]byte, limit+1), true, nil
				}
				return nil, false, unproved
			},
			isFile: func(context.Context, string) (bool, error) {
				t.Fatal("did not prove any workspace")
				return false, nil
			},
			matchDirs: func(context.Context, string, int) ([]string, error) {
				t.Fatal("did not prove any workspace")
				return nil, nil
			},
		})
		got, err := lookup.rewrite("src/use.ts", "foo")
		require.Empty(t, got)
		require.ErrorIs(t, err, errRebuildNpmInput)
		if !oversized {
			require.ErrorIs(t, err, unproved)
		}
		_, err = lookup.declaresExternal("src/use.ts", "foo")
		require.ErrorIs(t, err, errRebuildNpmInput)
		require.Equal(t, 1, calls)
	}
}

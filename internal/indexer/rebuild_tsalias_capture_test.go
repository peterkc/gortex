package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser/tsalias"
)

const rebuildTSAliasFixtureConfig = `{
	// An extends file must never be consulted.
	"extends": "./parent.json",
	"compilerOptions": { "paths": {
		"@lib/*": ["miss/*", "miss/*", "portal/*"],
		"@*": ["wrong/*"],
		"$absent": ["gone"]
	}, },
}`

func TestRebuildTSAliasCaptureNative(t *testing.T) {
	const caller = "app/deep/use.ts"
	var first []rebuildTSAliasFact
	for _, leaf := range []string{"plain", "repo[1]"} {
		t.Run(leaf, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), leaf)
			files := map[string]string{
				"jsconfig.json":            rebuildTSAliasFixtureConfig,
				"tsconfig.json":            `{"compilerOptions":{"paths":{"@lib/*":["wrong/*"]}}}`,
				"app/jsconfig.json":        `{"malformed":`,
				"app/tsconfig.json":        `{}`,
				"app/shadow/jsconfig.json": `{"compilerOptions":{"paths":{"@lib/*":[]}}}`,
				"app/shadow/use.js":        "",
				caller:                     "",
				"src/x/index.ts":           "export {}",
			}
			src, frozen := newRebuildTSAliasFixture(t, root, files)
			require.NoError(t, os.Symlink("src", filepath.Join(root, "portal")))
			read := rebuildTSAliasFixtureRead(src, frozen)
			probe := func(ctx context.Context, rel string) (bool, error) {
				p, err := source.ResolveFrozenRootPath(ctx, src.FrozenRoot(), rel)
				if errors.Is(err, source.ErrNotInSource) {
					// This controlled fixture owns the whole tree. A production
					// callback needs frozen negative evidence as well as a miss.
					return false, nil
				}
				if err != nil {
					return false, err
				}
				if p.Info().IsDir() {
					return false, nil
				}
				if _, admitted := frozen[p.Path()]; !admitted || !p.Info().Mode().IsRegular() {
					return false, tsalias.ErrFrozenAliasUnproved
				}
				return true, nil
			}
			const budget = int64(64 << 20)
			left := budget
			capture := func(file, spec string) (tsalias.FrozenAliasStem, []rebuildTSAliasFact) {
				var facts []rebuildTSAliasFact
				stem, err := captureRebuildTSAlias(t.Context(), file, spec, &left, read, probe, func(fact rebuildTSAliasFact) error {
					facts = append(facts, fact)
					return nil
				})
				require.NoError(t, err)
				return stem, facts
			}
			native := tsalias.Load(root)
			var nativeProbes []rebuildTSAliasFact
			nativeStem := resolveTSPathAliasObserved(native, "repo", "repo/"+caller, "@lib/x", func(rel string, present bool) {
				nativeProbes = append(nativeProbes, rebuildTSAliasFact{Kind: rebuildTSAliasProbe, Path: rel, Present: present})
			})
			stem, facts := capture(caller, "@lib/x")
			require.Equal(t, "portal/x", string(stem))
			require.Equal(t, nativeStem, prefixTSAliasTarget("repo", string(stem)))
			require.Len(t, facts, 50) // five config reads/checks, then 21 + 21 + 3 probes
			require.Equal(t, []rebuildTSAliasFact{
				{Kind: rebuildTSAliasConfig, Path: "app/deep/jsconfig.json"},
				{Kind: rebuildTSAliasConfig, Path: "app/deep/tsconfig.json"},
				{Kind: rebuildTSAliasConfig, Path: "app/jsconfig.json", Present: true, Digest: frozen["app/jsconfig.json"].Content},
				{Kind: rebuildTSAliasConfig, Path: "app/tsconfig.json", Present: true, Digest: frozen["app/tsconfig.json"].Content},
			}, facts[:4])
			require.Equal(t, "jsconfig.json", facts[4].Path)
			require.Equal(t, frozen["jsconfig.json"].Content, facts[4].Digest)
			require.NotNil(t, facts[4].Config)
			require.Equal(t, nativeProbes, facts[5:])
			require.Equal(t, facts[5:26], facts[26:47]) // the repeated negative is not deduplicated
			require.Equal(t, rebuildTSAliasFact{Kind: rebuildTSAliasProbe, Path: "portal/x/index.ts", Present: true}, facts[49])
			require.Equal(t, budget-frozen["jsconfig.json"].Size-frozen["app/jsconfig.json"].Size-frozen["app/tsconfig.json"].Size, left)
			if first == nil {
				first = facts
			} else {
				require.Equal(t, first, facts)
			}

			shadow, shadowFacts := capture("app/shadow/use.js", "@lib/x")
			require.Empty(t, shadow)
			require.Empty(t, resolveTSPathAlias(native, "", "app/shadow/use.js", "@lib/x"))
			require.Len(t, shadowFacts, 1)
			require.True(t, shadowFacts[0].Present)
			require.NotNil(t, shadowFacts[0].Config)
			require.Empty(t, shadowFacts[0].Config.Entries)

			unmatched, unmatchedFacts := capture(caller, "third-party")
			require.Empty(t, unmatched)
			require.Equal(t, facts[:5], unmatchedFacts)
			relative, relativeFacts := capture(caller, "../route/../target.js")
			require.Empty(t, relative)
			require.Empty(t, relativeFacts) // relative probing belongs to the graph

			negative, negativeFacts := capture(caller, "$absent")
			require.Equal(t, "gone", string(negative)) // native first target on all misses
			require.Equal(t, string(negative), resolveTSPathAlias(native, "", caller, "$absent"))
			require.Len(t, negativeFacts, 26)
			for _, fact := range negativeFacts[5:] {
				require.Equal(t, rebuildTSAliasProbe, fact.Kind)
				require.False(t, fact.Present)
			}
			// Replay either winning or all-negative evidence after both an
			// earlier miss turns into a hit and a winning input disappears.
			writeRebuildTSAliasFixtureFile(t, root, "miss/x.ts", "")
			writeRebuildTSAliasFixtureFile(t, root, "gone.ts", "")
			require.NoError(t, os.Remove(filepath.Join(root, "src/x/index.ts")))
			require.NoError(t, os.Remove(filepath.Join(root, "jsconfig.json")))
			assertRebuildTSAliasFixtureReplay(t, caller, "@lib/x", stem, facts)
			assertRebuildTSAliasFixtureReplay(t, caller, "$absent", negative, negativeFacts)
		})
	}
}

func TestRebuildTSAliasCaptureRefusals(t *testing.T) {
	const good = `{"compilerOptions":{"paths":{"@name":["first","later"]}}}`
	const limit = int64(tsalias.MaxFrozenAliasConfigBytes)
	root := t.TempDir()
	src, frozen := newRebuildTSAliasFixture(t, root, map[string]string{"jsconfig.json": good, "later.ts": ""})
	outside := t.TempDir()
	writeRebuildTSAliasFixtureFile(t, outside, "target.ts", "")
	require.NoError(t, os.Symlink(filepath.Join(outside, "target.ts"), filepath.Join(root, "first")))
	read := rebuildTSAliasFixtureRead(src, frozen)
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"outside", source.ErrOutsideRoot},
		{"non-admitted", tsalias.ErrFrozenAliasUnproved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var facts []rebuildTSAliasFact
			var probed []string
			left := int64(len(good))
			stem, err := captureRebuildTSAlias(t.Context(), "main.ts", "@name", &left, read,
				func(ctx context.Context, rel string) (bool, error) {
					probed = append(probed, rel)
					if rel != "first" {
						return rel == "later.ts", nil
					}
					if tc.name == "outside" {
						_, err := source.ResolveFrozenRootPath(ctx, src.FrozenRoot(), rel)
						return false, err
					}
					return false, tc.err
				}, func(fact rebuildTSAliasFact) error {
					facts = append(facts, fact)
					return nil
				})
			require.ErrorIs(t, err, tc.err)
			require.Empty(t, stem)
			require.Equal(t, []string{"first"}, probed)
			require.Len(t, facts, 1)
			require.Zero(t, left) // exactly enough for the proved config
		})
	}
	for _, tc := range []struct {
		name   string
		left   int64
		config string
		want   error
	}{
		{"aggregate limit", int64(len(good) - 1), good, errRebuildTSAliasCaptureLimit},
		{"entry limit", 64 << 20, strings.Repeat(" ", int(limit)+1), errRebuildTSAliasCaptureLimit},
		{"drift", 64 << 20, strings.Replace(good, "first", "other", 1), ErrBuildInputChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			initial := tc.config
			if tc.name == "drift" {
				initial = good
			}
			src, frozen := newRebuildTSAliasFixture(t, root, map[string]string{
				"jsconfig.json": initial, "tsconfig.json": good,
			})
			if tc.name == "drift" {
				writeRebuildTSAliasFixtureFile(t, root, "jsconfig.json", tc.config)
			}
			var consulted []string
			read := rebuildTSAliasFixtureRead(src, frozen)
			left := tc.left
			stem, err := captureRebuildTSAlias(t.Context(), "main.ts", "@name", &left,
				func(ctx context.Context, rel string, maxBytes int64) ([]byte, bool, error) {
					consulted = append(consulted, rel)
					require.Equal(t, min(tc.left, limit), maxBytes)
					return read(ctx, rel, maxBytes)
				}, nil, func(fact rebuildTSAliasFact) error {
					t.Fatalf("unproved config emitted or continued: %+v", fact)
					return nil
				})
			require.ErrorIs(t, err, tc.want)
			require.Empty(t, stem)
			require.Equal(t, []string{"jsconfig.json"}, consulted)
			require.Equal(t, tc.left, left)
		})
	}
}

func assertRebuildTSAliasFixtureReplay(t *testing.T, caller, spec string, want tsalias.FrozenAliasStem, facts []rebuildTSAliasFact) {
	t.Helper()
	i := 0
	take := func(kind rebuildTSAliasFactKind, rel string) rebuildTSAliasFact {
		t.Helper()
		require.Less(t, i, len(facts))
		fact := facts[i]
		i++
		require.Equal(t, kind, fact.Kind)
		require.Equal(t, rel, fact.Path)
		return fact
	}
	got, err := tsalias.ResolveFrozenAlias(caller, spec, func(rel string) (*tsalias.FrozenAliasConfig, error) {
		return take(rebuildTSAliasConfig, rel).Config, nil
	}, func(rel string) (bool, error) {
		return take(rebuildTSAliasProbe, rel).Present, nil
	})
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, len(facts), i)
}

func newRebuildTSAliasFixture(t *testing.T, root string, files map[string]string) (*source.FilesystemSource, map[string]rebuildInput) {
	t.Helper()
	for rel, content := range files {
		writeRebuildTSAliasFixtureFile(t, root, rel, content)
	}
	src, err := source.NewFilesystemSource(root)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, src.Close()) })
	frozen := make(map[string]rebuildInput, len(files))
	for rel, content := range files {
		input, _, err := readRebuildInput(t.Context(), src, rel, "fixture", int64(len(content)))
		require.NoError(t, err)
		frozen[rel] = input
	}
	return src, frozen
}

func writeRebuildTSAliasFixtureFile(t *testing.T, root, rel, data string) {
	t.Helper()
	name := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o755))
	require.NoError(t, os.WriteFile(name, []byte(data), 0o600))
}

func rebuildTSAliasFixtureRead(src *source.FilesystemSource, frozen map[string]rebuildInput) func(context.Context, string, int64) ([]byte, bool, error) {
	return func(ctx context.Context, rel string, maxBytes int64) ([]byte, bool, error) {
		input, has := frozen[rel]
		if !has {
			// The fixture's complete inventory has no symlinked config dirs.
			if _, err := src.FrozenRoot().Lstat(filepath.FromSlash(rel)); !errors.Is(err, os.ErrNotExist) {
				return nil, false, fmt.Errorf("%q: %w", rel, tsalias.ErrFrozenAliasUnproved)
			}
			return nil, false, nil
		}
		if input.Size > maxBytes {
			return nil, false, source.ErrRegularFileTooLarge
		}
		data, err := readFrozenRebuildInput(ctx, src, input)
		return data, true, err
	}
}

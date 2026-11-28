package indexer

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/indexer/merkle"
	"github.com/zzet/gortex/internal/parser/tsalias"
)

func TestRebuildResolverConfigFactsRoundTrip(t *testing.T) {
	const raw = `{"extends":"never-read.json","compilerOptions":{"baseUrl":"./src/../src","paths":{"@lib/*":["./missing/*.js","second/*"],"@lib/a":["never"],"@lib/*tail":["tail/*"]}}}`
	alias, err := tsalias.ParseFrozenAliasConfig([]byte(raw))
	require.NoError(t, err)
	for _, c := range []struct {
		contents string
		shadow   bool
	}{
		{`{ "extends": "never-read.json" }`, false},
		{`{"compilerOptions":{"baseUrl":"unused"}}`, true},
	} {
		name := "no-scope"
		if c.shadow {
			name = "empty-shadow"
		}
		t.Run(name, func(t *testing.T) {
			const dir = "pkg-\xff"
			rootGo := rebuildGoManifest{
				Path: "go.mod", Present: true, Digest: blake3.Sum256([]byte("root go.mod")), RootClaim: "example.com/root",
				DirectDeps: []string{"example.com/dep", "example.com/dep/v2"},
				Replace:    []rebuildGoReplace{{Module: "example.com/old"}, {Module: "example.com/old", Local: true}},
			}
			nestedGo := rebuildGoManifest{
				Path: dir + "/go.mod", Present: true, Digest: blake3.Sum256([]byte("nested go.mod")), OwnerModule: "example.com/nested",
			}
			goRows := []rebuildGoManifest{rootGo, nestedGo, {Path: "go.work"}, {Path: dir + "/go.work"}}
			scope, err := tsalias.ParseFrozenAliasConfig([]byte(c.contents))
			require.NoError(t, err)
			if c.shadow {
				require.NotNil(t, scope)
			} else {
				require.Nil(t, scope)
			}
			tsRows := []rebuildTSAliasFact{
				{Kind: rebuildTSAliasConfig, Path: "jsconfig.json"},
				{Kind: rebuildTSAliasConfig, Path: "tsconfig.json", Present: true, Digest: blake3.Sum256([]byte(raw)), Config: alias},
				{Kind: rebuildTSAliasConfig, Path: dir + "/jsconfig.json", Present: true, Digest: blake3.Sum256([]byte(c.contents)), Config: scope},
				{Kind: rebuildTSAliasConfig, Path: dir + "/tsconfig.json"},
			}
			wire := []rebuildSidecarRow{rebuildSidecarCppRow(rebuildCppFact{Kind: rebuildCppHeuristicDir, Path: "include"})}
			for _, f := range goRows {
				wire = append(wire, rebuildGoManifestSidecarRow(f))
			}
			for _, f := range tsRows {
				r, err := rebuildTSAliasConfigSidecarRow(f)
				require.NoError(t, err)
				wire = append(wire, r)
			}
			lease, digest := writeRebuildConfigFixture(t, wire...)
			_, relocated := writeRebuildConfigFixture(t, wire...)
			require.Equal(t, digest, relocated, "the lease path is not resolver input")
			data, err := os.ReadFile(filepath.Join(lease, rebuildInputSidecarName))
			require.NoError(t, err)
			require.True(t, bytes.HasPrefix(data, []byte(rebuildInputSidecarMagic+"{\"kind\":3,\"path\":\"aW5jbHVkZQ==\"}\n")), "legacy row bytes changed")
			require.False(t, bytes.Contains(data, []byte("never-read.json")), "raw config text or extends leaked into the sidecar")

			goFacts, tsFacts, err := openRebuildResolverConfigFacts(t.Context(), lease, digest)
			require.NoError(t, err)
			require.Len(t, goFacts, len(goRows))
			require.Len(t, tsFacts, len(tsRows))
			for _, want := range goRows {
				got, err := goFacts.lookup(want.Path)
				require.NoError(t, err)
				require.Equal(t, want, got)
			}
			for _, want := range tsRows {
				got, err := tsFacts.lookup(want.Path)
				require.NoError(t, err)
				require.Equal(t, want, got)
			}
			owner, err := rebuildGoNearest(dir, goFacts.lookup)
			require.NoError(t, err)
			require.Equal(t, nestedGo, owner)
			_, err = goFacts.lookup("pkg-\ufffd/go.mod")
			require.ErrorIs(t, err, errRebuildGoEvidence)
			_, err = tsFacts.lookup("pkg-\ufffd/jsconfig.json")
			require.ErrorIs(t, err, tsalias.ErrFrozenAliasUnproved)

			var probes []string
			stem, err := tsalias.ResolveFrozenAlias(dir+"/main.ts", "@lib/a.txt", tsFacts.config, func(key string) (bool, error) {
				probes = append(probes, key)
				return key == "src/second/a.txt", nil
			})
			require.NoError(t, err)
			if c.shadow {
				require.Empty(t, stem)
				require.Empty(t, probes)
			} else {
				require.Equal(t, tsalias.FrozenAliasStem("src/second/a.txt"), stem)
				require.Len(t, probes, 22)
				require.Equal(t, "src/missing/a.txt.js", probes[0])
				require.Equal(t, "src/second/a.txt", probes[21])
			}
		})
	}
}

func TestRebuildResolverConfigFactsRefusesIncompleteReplay(t *testing.T) {
	goRow := rebuildGoManifestSidecarRow(rebuildGoManifest{Path: "go.mod"})
	tsRow, err := rebuildTSAliasConfigSidecarRow(rebuildTSAliasFact{Kind: rebuildTSAliasConfig, Path: "tsconfig.json"})
	require.NoError(t, err)
	for _, tc := range []struct {
		name   string
		extra  []rebuildSidecarRow
		mutate func([]byte) []byte
		resign bool
		why    string
	}{
		{name: "identical Go duplicate", extra: []rebuildSidecarRow{goRow}, why: "duplicate Go manifest"},
		{name: "contradictory TS duplicate", extra: []rebuildSidecarRow{{Kind: tsRow.Kind, Path: tsRow.Path, Present: true, Digest: rebuildDigest{1}}}, why: "duplicate TS/JS config"},
		{name: "conflicting root digest", extra: []rebuildSidecarRow{{Kind: rebuildFollowedManifest, Path: goRow.Path, Present: true, Digest: rebuildDigest{1}}}, why: "conflicting followed go.mod"},
		{name: "valid changed tail", mutate: func(data []byte) []byte {
			return append(data, []byte("{\"kind\":10,\"path\":\"Lg==\"}\n")...)
		}, why: "byte digest"},
		{name: "truncated tail", mutate: func(data []byte) []byte { return data[:len(data)-1] }, why: "truncated"},
		{name: "duplicate JSON field", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"path":"Z28ubW9k"`), []byte(`"path":"Z28ubW9k","path":"Z28ubW9k"`), 1)
		}, resign: true, why: "noncanonical"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lease, digest := writeRebuildConfigFixture(t, append([]rebuildSidecarRow{goRow, tsRow}, tc.extra...)...)
			if tc.mutate != nil {
				name := filepath.Join(lease, rebuildInputSidecarName)
				data, err := os.ReadFile(name)
				require.NoError(t, err)
				data = tc.mutate(data)
				require.NotEqual(t, digest, rebuildDigest(blake3.Sum256(data)))
				require.NoError(t, os.WriteFile(name, data, 0o600))
				if tc.resign {
					digest = blake3.Sum256(data)
				}
			}
			goFacts, tsFacts, err := openRebuildResolverConfigFacts(t.Context(), lease, digest)
			require.ErrorIs(t, err, errRebuildInputSidecar)
			require.ErrorContains(t, err, tc.why)
			require.Nil(t, goFacts)
			require.Nil(t, tsFacts)
			_, err = goFacts.lookup("go.mod")
			require.ErrorIs(t, err, errRebuildGoEvidence)
			_, err = tsalias.ResolveFrozenAlias("main.ts", "@x", tsFacts.config, nil)
			require.ErrorIs(t, err, tsalias.ErrFrozenAliasUnproved)
		})
	}
}

func TestRebuildResolverConfigFactsRefusesInvalidRows(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  rebuildSidecarRow
	}{
		{"wrong digest state", rebuildSidecarRow{Kind: rebuildGoManifestFact, Path: []byte("go.mod"), Present: true}},
		{"nonportable key", rebuildGoManifestSidecarRow(rebuildGoManifest{Path: "../go.mod"})},
		{"workspace with payload", rebuildGoManifestSidecarRow(rebuildGoManifest{Path: "go.work", Present: true, Digest: rebuildDigest{1}, OwnerModule: "example.com/m"})},
		{"bad replacement order", rebuildGoManifestSidecarRow(rebuildGoManifest{Path: "go.mod", Present: true, Digest: rebuildDigest{1}, Replace: []rebuildGoReplace{{"example.com/m", true}, {"example.com/m", false}}})},
		{"negative TS scope", rebuildSidecarRow{Kind: rebuildTSAliasConfigFact, Path: []byte("tsconfig.json"), Config: &rebuildResolverConfigPayload{Alias: &tsalias.FrozenAliasConfig{}}}},
		{"legacy payload", rebuildSidecarRow{Kind: rebuildNamespaceCloseExactFact, Path: []byte("."), Config: &rebuildResolverConfigPayload{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := writeRebuildInputSidecar(t.Context(), t.TempDir(), func(emit func(rebuildSidecarRow) error) error { return emit(tc.row) })
			require.ErrorIs(t, err, errRebuildInputSidecar)
			require.Zero(t, d)
		})
	}
	_, err := rebuildTSAliasConfigSidecarRow(rebuildTSAliasFact{Kind: rebuildTSAliasProbe, Path: "src/main.ts", Present: true})
	require.ErrorIs(t, err, errRebuildInputSidecar)
	cfg, err := tsalias.ParseFrozenAliasConfig([]byte(`{"compilerOptions":{"paths":{"@x":["one","two"]}}}`))
	require.NoError(t, err)
	require.NotNil(t, cfg)
	cfg.Entries[0].TargetPrefix = "wrong-first-target"
	r, err := rebuildTSAliasConfigSidecarRow(rebuildTSAliasFact{Kind: rebuildTSAliasConfig, Path: "tsconfig.json", Present: true, Digest: rebuildDigest{1}, Config: cfg})
	require.NoError(t, err)
	_, err = writeRebuildInputSidecar(t.Context(), t.TempDir(), func(emit func(rebuildSidecarRow) error) error { return emit(r) })
	require.ErrorIs(t, err, errRebuildInputSidecar)
}

func TestRebuildResolverConfigFactsRefusesCaps(t *testing.T) {
	cfg, err := tsalias.ParseFrozenAliasConfig([]byte(`{"compilerOptions":{"paths":{"@x":["one"]}}}`))
	require.NoError(t, err)
	require.NotNil(t, cfg)
	wire, err := rebuildTSAliasConfigSidecarRow(rebuildTSAliasFact{Kind: rebuildTSAliasConfig, Path: "tsconfig.json", Present: true, Digest: rebuildDigest{1}, Config: cfg})
	require.NoError(t, err)
	cfg.BaseURL = strings.Repeat("x", rebuildCppEntryLimit)
	d, err := writeRebuildInputSidecar(t.Context(), t.TempDir(), func(emit func(rebuildSidecarRow) error) error { return emit(wire) })
	require.ErrorIs(t, err, errRebuildCppCaptureLimit)
	require.Zero(t, d)

	cfg.BaseURL = strings.Repeat("x", rebuildCppEntryLimit/2)
	tests := []struct {
		name string
		n    int
		row  func(int) rebuildSidecarRow
		want error
	}{
		{"Go row count", rebuildGoKeyLimit + 1, func(i int) rebuildSidecarRow {
			return rebuildGoManifestSidecarRow(rebuildGoManifest{Path: fmt.Sprintf("dir%d/go.work", i)})
		}, errRebuildGoCaptureLimit},
		{"TS retained charge", rebuildTSAliasConfigRetainedLimit/len(cfg.BaseURL) + 1, func(i int) rebuildSidecarRow {
			r := wire
			r.Path = []byte(fmt.Sprintf("dir%d/tsconfig.json", i))
			return r
		}, errRebuildTSAliasCaptureLimit},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lease := t.TempDir()
			digest, err := writeRebuildInputSidecar(t.Context(), lease, func(emit func(rebuildSidecarRow) error) error {
				for i := range tc.n {
					if err := emit(tc.row(i)); err != nil {
						return err
					}
				}
				return nil
			})
			require.NoError(t, err, "rows must fit encoded bounds but not the replay map bound")
			g, ts, err := openRebuildResolverConfigFacts(t.Context(), lease, digest)
			require.ErrorIs(t, err, tc.want)
			require.Nil(t, g)
			require.Nil(t, ts)
		})
	}
}

func writeRebuildConfigFixture(t *testing.T, rows ...rebuildSidecarRow) (string, rebuildDigest) {
	t.Helper()
	lease := t.TempDir()
	digest, err := writeRebuildInputSidecar(t.Context(), lease, func(emit func(rebuildSidecarRow) error) error {
		for _, row := range rows {
			if err := emit(row); err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)
	require.NotZero(t, digest)
	return lease, digest
}

func TestRebuildGoContractDependenciesCheckedReplay(t *testing.T) {
	const name = "go.mod"
	ctx := t.Context()
	data := []byte(`module example.com/root

require (
    example.com/z v1.2.3
    example.com/indirect v1.1.0 // indirect
    example.com/a v0.9.8
)
`)
	digest := rebuildDigest(blake3.Sum256(data))
	left := int64(len(data))
	row, err := captureRebuildGoManifest(ctx, name, &left,
		func(_ context.Context, path string) (*rebuildInput, error) {
			require.Equal(t, name, path)
			return &rebuildInput{Path: name, Size: int64(len(data)), Kind: merkle.RegularFile, Content: digest}, nil
		},
		func(_ context.Context, input rebuildInput) ([]byte, error) {
			require.Equal(t, digest, input.Content)
			return data, nil
		})
	require.NoError(t, err)
	require.Zero(t, left)
	require.Equal(t, []string{"example.com/a", "example.com/z"}, row.DirectDeps)
	lease, proof := writeRebuildConfigFixture(t, rebuildGoManifestSidecarRow(row))
	rows, _, err := openRebuildResolverConfigFacts(ctx, lease, proof)
	require.NoError(t, err)
	replayed, err := rows.lookup(name)
	require.NoError(t, err)
	want := (&contracts.GoModExtractor{}).Extract(name, data, nil, nil)
	var got []contracts.Contract
	require.NoError(t, replayed.visitContractDeps(func(c contracts.Contract) error {
		got = append(got, c)
		return nil
	}))
	require.Equal(t, want, got, "versions and exact source order come from the checked row")

	legacy := row
	legacy.ContractDeps = nil
	require.NoError(t, rebuildGoManifestSidecarRow(legacy).check())
	unexpected := func(contracts.Contract) error {
		t.Fatal("unproved or checked-empty input yielded a record")
		return nil
	}
	require.ErrorIs(t, legacy.visitContractDeps(unexpected), errRebuildGoEvidence)
	require.NoError(t, (rebuildGoManifest{Path: name}).visitContractDeps(unexpected))

	for _, invalid := range []rebuildGoContractDep{
		{Module: "example.com/foreign", Version: "v1.2.3", Line: 4},
		{Module: "example.com/z", Version: "", Line: 4},
		{Module: "example.com/z", Version: "v1.2.3", Line: 0},
	} {
		bad := row
		bad.ContractDeps = []rebuildGoContractDep{invalid, row.ContractDeps[1]}
		require.ErrorIs(t, rebuildGoManifestSidecarRow(bad).check(), errRebuildInputSidecar)
	}
}

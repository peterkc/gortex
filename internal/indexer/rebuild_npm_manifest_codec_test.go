package indexer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
)

func TestRebuildNpmManifestCodecNative(t *testing.T) {
	const root = "package.json"
	const nested = "raw\xff/package.json"
	const manifest = `{
		"name":"self","dependencies":{"alias":"npm:@demo/real@1","exact":" ^3.1 ","dep":"workspace:*","":"npm:empty-key"},
		"optionalDependencies":{"alias":" ~3 ","dev":""},
		"exports":{"./*":{"default":"./other/*.js","import":" ./chosen/*.js "},"./flat":"./one.js","./ignored":null},
		"workspaces":{"packages":["z","a","z"]},"ignored":"not-a-resolver-fact"
	}`
	boundary := []rebuildNamespaceRow{
		{root, rebuildNamespaceRegular},
		{"raw\xff", rebuildNamespaceDirectory},
		{nested, rebuildNamespaceRegular},
		{"raw\xff", rebuildNamespaceCloseExact},
		{".", rebuildNamespaceCloseExact},
	}
	left := int64(rebuildCppRawLimit)
	rootRow := rebuildNpmCodecTestRow(t, root, manifest, &left)
	nestedRow := rebuildNpmCodecTestRow(t, nested, `{"name":" spaced ","workspaces":["not-root"]}`, &left)
	// Reinsert map entries in a different order without changing the facts.
	other := *rootRow.NPM
	other.Exports = map[string]string{"./flat": "./one.js", "./*": "./chosen/*.js"}
	otherRootRow := rootRow
	otherRootRow.NPM = &other
	lease, digest := writeRebuildNpmCodecTestSidecar(t, boundary, rootRow, nestedRow)
	otherLease, otherDigest := writeRebuildNpmCodecTestSidecar(t, boundary, otherRootRow, nestedRow)
	name := filepath.Join(lease, rebuildInputSidecarName)
	wire, err := os.ReadFile(name)
	require.NoError(t, err)
	otherWire, err := os.ReadFile(filepath.Join(otherLease, rebuildInputSidecarName))
	require.NoError(t, err)
	require.Equal(t, wire, otherWire)
	require.Equal(t, digest, otherDigest)
	require.NotContains(t, string(wire), "not-a-resolver-fact")

	checkout := t.TempDir()
	live := filepath.Join(checkout, root)
	require.NoError(t, os.WriteFile(live, []byte(manifest), 0o600))
	native := newNpmAliasIndex(map[string]string{"": checkout})
	require.Equal(t, "@demo/real/f", native.Resolve("use.ts", "alias/f"))
	require.Equal(t, "self/chosen/f.js", native.Resolve("use.ts", "self/f"))
	require.True(t, native.DeclaresExternalDependency("use.ts", "alias"))
	require.NoError(t, os.Remove(live))
	require.Empty(t, newNpmAliasIndex(map[string]string{"": checkout}).Resolve("use.ts", "alias/f"))

	ns, got, followed, err := openRebuildNpmManifestFacts(t.Context(), lease, digest)
	require.NoError(t, err)
	require.Zero(t, followed)
	require.Len(t, got, 2)
	require.Equal(t, rebuildNpmManifestFact{Path: root, Digest: blake3.Sum256([]byte(manifest)), Manifest: npmResolverManifest{
		name: "self", aliases: map[string]string{"alias": "@demo/real", "": "empty-key"},
		dependencies: map[string]string{"alias": " ~3 ", "exact": " ^3.1 ", "dep": "workspace:*", "dev": ""},
		exports:      &packageExports{name: "self", subpaths: map[string]string{"./*": "./chosen/*.js", "./flat": "./one.js"}},
		workspaces:   []string{"z", "a", "z"},
	}}, got[root])
	require.Equal(t, rebuildNpmManifestFact{Path: nested, Digest: nestedRow.Digest, Manifest: npmResolverManifest{name: " spaced "}}, got[nested])
	lookup := rebuildNpmCodecTestLookup(t, ns, got, followed)
	for spec, want := range map[string]string{"alias/f": "@demo/real/f", "self/f": "self/chosen/f.js"} {
		got, err := lookup.rewrite("use.ts", spec)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	external, err := lookup.declaresExternal("use.ts", "alias")
	require.NoError(t, err)
	require.True(t, external)

	// Change canonical bytes without breaking row framing or the namespace.
	changed := bytes.Replace(wire, []byte(`"name":"self"`), []byte(`"name":"edit"`), 1)
	require.NotEqual(t, wire, changed)
	require.NoError(t, os.WriteFile(name, changed, 0o600))
	ns, got, followed, err = openRebuildNpmManifestFacts(t.Context(), lease, digest)
	require.ErrorIs(t, err, errRebuildInputSidecar)
	require.ErrorContains(t, err, "sidecar byte digest")
	require.Nil(t, ns)
	require.Nil(t, got)
	require.Zero(t, followed)
}

func TestRebuildExternalNpmRewriteUsesCapturedEdgePath(t *testing.T) {
	// Only the frozen namespace and manifest rows exist. There is no parse
	// receipt or live checkout source to establish the importer path.
	boundary := []rebuildNamespaceRow{
		{"package.json", rebuildNamespaceRegular},
		{"app", rebuildNamespaceDirectory},
		{"app/package.json", rebuildNamespaceRegular},
		{"app/caller.ts", rebuildNamespaceRegular},
		{"app", rebuildNamespaceCloseExact},
		{"denied", rebuildNamespaceDenied},
		{".", rebuildNamespaceCloseExact},
	}
	left := int64(rebuildCppRawLimit)
	root := rebuildNpmCodecTestRow(t, "package.json", `{"dependencies":{"alias":"npm:wrong@1"}}`, &left)
	app := rebuildNpmCodecTestRow(t, "app/package.json", `{"dependencies":{"alias":"npm:@demo/real@1"}}`, &left)
	lease, digest := writeRebuildNpmCodecTestSidecar(t, boundary, root, app)
	ctx := t.Context()
	rewrite := rebuildExternalNpmRewrite(ctx, lease, digest, &rebuildExtractorCensus{}, nil)
	got, err := rewrite("app/caller.ts", "alias/sub")
	require.NoError(t, err)
	require.Equal(t, "@demo/real/sub", got)

	got, err = rewrite("app/missing.ts", "alias/sub")
	require.NoError(t, err)
	require.Equal(t, "@demo/real/sub", got)
	got, err = rewrite("denied/caller.ts", "alias/sub")
	require.ErrorIs(t, err, errRebuildResolverInputDenied)
	require.Empty(t, got)

	badDigest := digest
	badDigest[0] ^= 1
	got, err = rebuildExternalNpmRewrite(ctx, lease, badDigest, &rebuildExtractorCensus{}, nil)("app/caller.ts", "alias/sub")
	require.ErrorIs(t, err, errRebuildInputSidecar)
	require.Empty(t, got)
}

func TestRebuildNpmManifestCodecAuthority(t *testing.T) {
	const key = "package.json"
	left := int64(rebuildCppRawLimit)
	empty := rebuildNpmCodecTestRow(t, key, `{}`, &left)
	require.Nil(t, empty.NPM)
	follow := rebuildSidecarRow{Kind: rebuildFollowedManifest, Path: []byte(key), Present: true, Digest: empty.Digest}
	wrong := follow
	wrong.Digest[0] ^= 1
	for _, tc := range []struct {
		name        string
		kind, close rebuildNamespaceKind
		facts       []rebuildSidecarRow
		openErr     error
		lookupErr   error
		count       int
		followed    rebuildDigest
	}{
		{name: "proved absence", close: rebuildNamespaceCloseExact},
		{name: "valid empty", kind: rebuildNamespaceRegular, close: rebuildNamespaceCloseExact, facts: []rebuildSidecarRow{empty}, count: 1},
		{name: "regular without fact", kind: rebuildNamespaceRegular, close: rebuildNamespaceCloseExact, lookupErr: errRebuildResolverNamespace},
		{name: "unsupported", kind: rebuildNamespaceUnsupported, close: rebuildNamespaceCloseExact, lookupErr: errRebuildResolverNamespace},
		{name: "unknown", close: rebuildNamespaceClose, lookupErr: errRebuildResolverNamespace},
		{name: "denied", kind: rebuildNamespaceDenied, close: rebuildNamespaceCloseExact, lookupErr: errRebuildResolverInputDenied},
		{name: "payload without admitted name", close: rebuildNamespaceCloseExact, facts: []rebuildSidecarRow{empty}, openErr: errRebuildResolverNamespace},
		{name: "denied payload", kind: rebuildNamespaceDenied, close: rebuildNamespaceCloseExact, facts: []rebuildSidecarRow{empty}, openErr: errRebuildResolverInputDenied},
		{name: "duplicate", kind: rebuildNamespaceRegular, close: rebuildNamespaceCloseExact, facts: []rebuildSidecarRow{empty, empty}, openErr: errRebuildInputSidecar},
		{name: "verified root link", kind: rebuildNamespaceUnsupported, close: rebuildNamespaceCloseExact, facts: []rebuildSidecarRow{follow, empty}, count: 1, followed: empty.Digest},
		{name: "changed root target", kind: rebuildNamespaceUnsupported, close: rebuildNamespaceCloseExact, facts: []rebuildSidecarRow{wrong, empty}, openErr: errRebuildResolverNamespace},
		{name: "missing root target fact", kind: rebuildNamespaceUnsupported, close: rebuildNamespaceCloseExact, facts: []rebuildSidecarRow{follow}, openErr: errRebuildResolverNamespace},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var boundary []rebuildNamespaceRow
			if tc.kind != rebuildNamespaceUnknown {
				boundary = append(boundary, rebuildNamespaceRow{key, tc.kind})
			}
			lease, digest := writeRebuildNpmCodecTestSidecar(t, append(boundary, rebuildNamespaceRow{".", tc.close}), tc.facts...)
			ns, got, followed, err := openRebuildNpmManifestFacts(t.Context(), lease, digest)
			if tc.openErr != nil {
				require.ErrorIs(t, err, errRebuildInputSidecar)
				require.ErrorIs(t, err, tc.openErr)
				require.Nil(t, ns)
				require.Nil(t, got)
				require.Zero(t, followed)
				return
			}
			require.NoError(t, err)
			require.Len(t, got, tc.count)
			require.Equal(t, tc.followed, followed)
			if tc.count != 0 {
				require.Equal(t, empty.Digest, got[key].Digest)
			}
			lookup := rebuildNpmCodecTestLookup(t, ns, got, followed)
			out, err := lookup.rewrite("use.ts", "anything")
			require.Empty(t, out)
			if tc.lookupErr != nil {
				require.ErrorIs(t, err, errRebuildNpmInput)
				require.ErrorIs(t, err, tc.lookupErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestRebuildNpmManifestCodecInvalidRows(t *testing.T) {
	left := int64(rebuildCppRawLimit)
	row := rebuildNpmCodecTestRow(t, "package.json", `{"name":"x"}`, &left)
	encoded, err := json.Marshal(row)
	require.NoError(t, err)
	for _, tc := range []struct{ name, from, to, suffix, why string }{
		{"bad field", `"name":"x"`, `"aliases":{"alias":""}`, "\n", "invalid normalized npm manifest"},
		{"unknown field", `"name":"x"`, `"name":"x","other":"x"`, "\n", "noncanonical"},
		{"duplicate field", `"name":"x"`, `"name":"y","name":"x"`, "\n", "noncanonical"},
		{"truncated frame", `"name":"x"`, `"name":"x"`, "", "truncated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Contains(t, string(encoded), tc.from)
			bad := []byte(rebuildInputSidecarMagic + strings.Replace(string(encoded), tc.from, tc.to, 1) + tc.suffix)
			lease := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(lease, rebuildInputSidecarName), bad, 0o600))
			ns, got, followed, err := openRebuildNpmManifestFacts(t.Context(), lease, blake3.Sum256(bad))
			require.ErrorIs(t, err, errRebuildInputSidecar)
			require.ErrorContains(t, err, tc.why)
			require.Nil(t, ns)
			require.Nil(t, got)
			require.Zero(t, followed)
		})
	}
}

func TestRebuildNpmManifestCodecCaps(t *testing.T) {
	const key = "package.json"
	const small = `{"name":"kept"}`
	raw := int64(len(small))
	row := rebuildNpmCodecTestRow(t, key, small, &raw)
	require.Zero(t, raw)
	_, err := normalizeRebuildNpmManifest(key, rebuildNpmTestIdentity(key, []byte(small)), []byte(small), &raw)
	require.ErrorIs(t, err, errRebuildCppCaptureLimit)
	encoded, err := json.Marshal(row)
	require.NoError(t, err)
	var b bytes.Buffer
	w := bufio.NewWriter(&b)
	left := len(encoded) + 1
	require.NoError(t, writeRebuildSidecarRow(t.Context(), w, &left, row))
	require.Zero(t, left)
	require.ErrorIs(t, writeRebuildSidecarRow(t.Context(), w, &left, row), errRebuildCppCaptureLimit)
	require.NoError(t, w.Flush())
	require.Equal(t, string(encoded)+"\n", b.String())

	// Small enough raw; the canonical JSON escapes expand it past 1 MiB.
	wide := `{"name":"` + strings.Repeat("<", rebuildCppEntryLimit/2) + `"}`
	raw = rebuildCppRawLimit
	row = rebuildNpmCodecTestRow(t, key, wide, &raw)
	left = rebuildInputEncodedLimit
	require.ErrorIs(t, writeRebuildSidecarRow(t.Context(), bufio.NewWriter(io.Discard), &left, row), errRebuildCppCaptureLimit)
	require.Equal(t, rebuildInputEncodedLimit, left)

	lease := t.TempDir()
	name := filepath.Join(lease, rebuildInputSidecarName)
	require.NoError(t, os.WriteFile(name, []byte(rebuildInputSidecarMagic), 0o600))
	require.NoError(t, os.Truncate(name, rebuildInputEncodedLimit+1))
	ns, got, followed, err := openRebuildNpmManifestFacts(t.Context(), lease, rebuildDigest{1})
	require.ErrorIs(t, err, errRebuildInputSidecar)
	require.ErrorIs(t, err, errRebuildCppCaptureLimit)
	require.Nil(t, ns)
	require.Nil(t, got)
	require.Zero(t, followed)
}

func rebuildNpmCodecTestRow(t *testing.T, key, text string, left *int64) rebuildSidecarRow {
	t.Helper()
	data := []byte(text)
	f, err := normalizeRebuildNpmManifest(key, rebuildNpmTestIdentity(key, data), data, left)
	require.NoError(t, err)
	row, err := rebuildNpmManifestSidecarRow(f)
	require.NoError(t, err)
	return row
}

func writeRebuildNpmCodecTestSidecar(t *testing.T, boundary []rebuildNamespaceRow, rows ...rebuildSidecarRow) (string, rebuildDigest) {
	t.Helper()
	lease := t.TempDir()
	digest, err := writeRebuildInputSidecar(t.Context(), lease, func(emit func(rebuildSidecarRow) error) error {
		for _, row := range boundary {
			if err := writeRebuildNamespaceFact(row, emit); err != nil {
				return err
			}
		}
		for _, row := range rows {
			if err := emit(row); err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)
	return lease, digest
}

func rebuildNpmCodecTestLookup(t *testing.T, ns *rebuildResolverNamespace, facts map[string]rebuildNpmManifestFact, followed rebuildDigest) *rebuildNpmLookup {
	t.Helper()
	globs, err := newRebuildWorkspaceGlob(t.Context(), ns)
	require.NoError(t, err)
	return newRebuildNpmLookup(t.Context(), rebuildNpmInputs{
		namespace: ns, manifests: facts, followedRoot: followed, matchDirs: globs.matchDirs,
		isFile: func(_ context.Context, key string) (bool, error) { return ns.probeFile(key) },
	})
}

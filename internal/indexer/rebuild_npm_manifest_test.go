package indexer

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/indexer/merkle"
)

func rebuildNpmTestIdentity(rel string, src []byte) rebuildInput {
	return rebuildInput{Path: rel, Language: "json", Kind: merkle.RegularFile, Size: int64(len(src)), Content: blake3.Sum256(src)}
}

func TestRebuildNpmManifestFieldErrors(t *testing.T) {
	const root = "package.json"
	left := int64(rebuildCppRawLimit)
	capture := func(rel, text string) rebuildNpmManifestFact {
		src := []byte(text)
		before := left
		fact, err := normalizeRebuildNpmManifest(rel, rebuildNpmTestIdentity(rel, src), src, &left)
		require.NoError(t, err)
		require.Equal(t, rel, fact.Path)
		require.Equal(t, rebuildDigest(blake3.Sum256(src)), fact.Digest)
		require.Equal(t, int64(len(src)), before-left)
		return fact
	}
	p := capture(root, `{"name":42,"dependencies":{"x":"npm:y@1"},"exports":"./a.js","workspaces":[" z ","a"," z "]}`).Manifest
	require.Empty(t, p.name)
	require.Nil(t, p.exports)
	require.Equal(t, map[string]string{"x": "y"}, p.aliases)
	require.Equal(t, map[string]string{"x": "npm:y@1"}, p.dependencies)
	require.Equal(t, []string{" z ", "a", " z "}, p.workspaces)
	p = capture(root, `{"name":"p","dependencies":{"x":42},"exports":{"import":"./a.js"},"workspaces":7}`).Manifest
	require.Equal(t, "p", p.name)
	require.Equal(t, &packageExports{name: "p", subpaths: map[string]string{".": "./a.js"}}, p.exports)
	require.Nil(t, p.aliases)
	require.Nil(t, p.dependencies)
	require.Nil(t, p.workspaces)
	// The identity, not a useful payload, marks a positive fact.
	p = capture("raw\xff/package.json", `{malformed`).Manifest
	require.Equal(t, npmResolverManifest{}, p)
}

func TestRebuildNpmManifestRefusesIdentityAndLimits(t *testing.T) {
	const rel = "packages/app/package.json"
	src := []byte(`{"dependencies":{"x":"npm:y@1"}}`)
	in := rebuildNpmTestIdentity(rel, src)
	badPath, badKind := in, in
	badPath.Path = "package.json"
	badKind.Kind = merkle.Symlink
	atCap := bytes.Repeat([]byte{' '}, rebuildNpmFactBytes-len(rel))
	overCap := append(bytes.Clone(atCap), ' ')
	for _, tc := range []struct {
		label string
		in    rebuildInput
		src   []byte
		left  int64
		err   error
	}{
		{"wrong path", badPath, src, 100, ErrBuildInputChanged},
		{"wrong kind", badKind, src, 100, ErrBuildInputChanged},
		{"different bytes of same size", in, bytes.Repeat([]byte{'x'}, len(src)), 100, ErrBuildInputChanged},
		{"raw budget", in, src, int64(len(src) - 1), errRebuildCppCaptureLimit},
		{"invalid raw budget", in, src, rebuildCppRawLimit + 1, errRebuildCppCaptureLimit},
		{"per entry cap", rebuildNpmTestIdentity(rel, overCap), overCap, rebuildCppRawLimit, errRebuildCppCaptureLimit},
	} {
		t.Run(tc.label, func(t *testing.T) {
			left := tc.left
			fact, err := normalizeRebuildNpmManifest(rel, tc.in, tc.src, &left)
			require.ErrorIs(t, err, tc.err)
			require.Zero(t, fact)
			require.Equal(t, tc.left, left)
		})
	}
	left := int64(len(atCap))
	fact, err := normalizeRebuildNpmManifest(rel, rebuildNpmTestIdentity(rel, atCap), atCap, &left)
	require.NoError(t, err)
	require.NotZero(t, fact.Digest)
	require.Zero(t, left)
}

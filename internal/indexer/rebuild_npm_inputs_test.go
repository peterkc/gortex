package indexer

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRebuildNpmInputsNative(t *testing.T) {
	layouts := []struct {
		label, workspaces, winner string
	}{
		{"array order", `["packages/*","[","z","a"]`, "a"},
		{"object keeps repeats", `{"packages":["packages/*","[","z","a","z"]}`, "z"},
	}
	for _, layout := range layouts {
		t.Run(layout.label, func(t *testing.T) {
			rootManifest := `{"name":"self","workspaces":` + layout.workspaces + `,
					"dependencies":{"foo":"npm:one@1","bar":"npm:one@1","pkg":"^1.0.0","self":"npm:self@1","plain":"workspace:*"},
					"devDependencies":{"foo":"npm:two@1","bar":"npm:two@1"},"peerDependencies":{"foo":"^3","bar":"workspace:*"},
					"optionalDependencies":{"foo":"workspace:*","bar":"^3"},"exports":{"./f":"./dist/f.js"}}`
			const libManifest = `{"name":"@demo/lib","exports":{
					"./feature":{"default":"./other.js","import":{"import":null,"default":" ./src/feature.js "}},
					"./blocked":null,"./junk":["./decoy.js"],"./*":"./wild/*.js","./internal/*":"./private/*.js"}}`
			files := map[string]string{
				"package.json":                  rootManifest,
				"packages/app/package.json":     `{"name":42,"dependencies":{"pkg":"workspace:*","plain":null}}`,
				"packages/app/src/package.json": `{malformed`,
				"packages/lib/package.json":     libManifest,
				"packages/lib/index.ts":         "export {}",
				"packages/lib/feature/.keep":    "",
				"packages/lib/blocked/.keep":    "",
				"packages/lib/junk/.keep":       "",
				"packages/lib/internal/f/.keep": "",
				"packages/bad/package.json":     `{"name":"bad","dependencies":{"invalid":1},"exports":["./decoy.js"]}`,
				"a/package.json":                `{"name":"dupe"}`,
				"a/index.ts":                    "export {}",
				"z/package.json":                `{"name":"dupe"}`,
				"z/index.ts":                    "export {}",
				"src/use.ts":                    "export {}",
			}
			facts := rebuildNpmTestFacts(t, files)
			cases := []struct {
				caller, spec, rewrite string
				external              bool
			}{
				{"packages/app/src/use.ts", "@demo/lib", "../../lib", false},
				{"packages/app/src/use.js", "foo/sub", "two/sub", false},
				{"packages/app/src/use.js", "bar/sub", "two/sub", true},
				{"packages/lib/use.ts", "@demo/lib/feature", "@demo/lib/src/feature.js", false},
				{"packages/lib/use.ts", "@demo/lib/blocked", "@demo/lib/wild/blocked.js", false},
				{"packages/lib/use.ts", "@demo/lib/junk", "@demo/lib/wild/junk.js", false},
				{"packages/lib/use.ts", "@demo/lib/internal/f", "@demo/lib/private/f.js", false},
				{"packages/bad/use.ts", "bad", "", false},
				{"packages/app/src/use.ts", "pkg", "", false},
				{"src/use.ts", "pkg", "", true},
				{"packages/app/src/use.ts", "plain", "", true},
				{"src/use.ts", "plain", "", false},
				{"src/use.ts", "self/f", "self/f", true},
				{"src/use.ts", "dupe", "../" + layout.winner, false},
				{"packages/app/src/use.ts", "unknown", "", false},
				{"packages/app/src/use.go", "foo", "", false},
			}
			for range 2 {
				root := t.TempDir()
				for rel, content := range files {
					name := filepath.Join(root, filepath.FromSlash(rel))
					require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o700))
					require.NoError(t, os.WriteFile(name, []byte(content), 0o600))
				}
				native := newNpmAliasIndex(map[string]string{"": root})
				for _, tc := range cases {
					require.Equal(t, tc.rewrite, native.Resolve(tc.caller, tc.spec), "%#v", tc)
					require.Equal(t, tc.external, native.DeclaresExternalDependency(tc.caller, tc.spec), "%#v", tc)
				}
				require.NoError(t, os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"dependencies":{"foo":"npm:changed@1"}}`), 0o600))
				require.NoError(t, os.Remove(filepath.Join(root, "packages", "lib", "index.ts")))
				fresh := newNpmAliasIndex(map[string]string{"": root})
				require.Equal(t, "changed/sub", fresh.Resolve("packages/app/src/use.ts", "foo/sub"))
				require.Empty(t, fresh.Resolve("packages/app/src/use.ts", "@demo/lib"))
				frozen := newRebuildNpmLookup(t.Context(), facts)
				for _, tc := range cases {
					got, err := frozen.rewrite(tc.caller, tc.spec)
					require.NoError(t, err, "%#v", tc)
					require.Equal(t, tc.rewrite, got, "%#v", tc)
					external, err := frozen.declaresExternal(tc.caller, tc.spec)
					require.NoError(t, err, "%#v", tc)
					require.Equal(t, tc.external, external, "%#v", tc)
				}
			}
		})
	}
}

func TestRebuildNpmInputsWorkspaceAggregateCap(t *testing.T) {
	facts := rebuildNpmTestFacts(t, map[string]string{"package.json": `{"workspaces":["work/*","work/*"]}`})
	const pattern = "work/*"
	var many []string
	for i := range 160 {
		many = append(many, fmt.Sprintf("work/%03d/%s", i, strings.Repeat("x", rebuildNamespaceKeyLimit-9)))
	}
	var limits []int
	facts.matchDirs = func(_ context.Context, key string, limit int) ([]string, error) {
		require.Equal(t, pattern, key)
		limits = append(limits, limit)
		return many, nil
	}
	lookup := newRebuildNpmLookup(t.Context(), facts)
	out, err := lookup.rewrite("src/use.ts", "workspace-member")
	require.Empty(t, out)
	require.ErrorIs(t, err, errRebuildNpmInput)
	require.ErrorIs(t, err, errRebuildCppCaptureLimit)
	require.Equal(t, []int{rebuildNpmFactBytes - len(pattern), rebuildNpmFactBytes - 2*len(pattern) - len(many)*(len(many[0])+1)}, limits)
	out, err = lookup.rewrite("src/use.ts", "workspace-member")
	require.Empty(t, out)
	require.ErrorIs(t, err, errRebuildCppCaptureLimit)
	require.Len(t, limits, 2, "a failed workspace expansion remains refused")
}

// Convert the fixture to checked listings and normalized manifests without
// handing the replay callbacks any manifest bytes or a checkout path.
func rebuildNpmTestFacts(t *testing.T, files map[string]string) rebuildNpmInputs {
	t.Helper()
	dirs := map[string]bool{".": true}
	keys := make([]string, 0, len(files))
	manifests := make(map[string]rebuildNpmManifestFact)
	left := int64(rebuildCppRawLimit)
	for rel, content := range files {
		keys = append(keys, rel)
		for p := path.Dir(rel); !dirs[p]; p = path.Dir(p) {
			dirs[p] = true
			keys = append(keys, p)
		}
		if path.Base(rel) == "package.json" {
			src := []byte(content)
			fact, err := normalizeRebuildNpmManifest(rel, rebuildNpmTestIdentity(rel, src), src, &left)
			require.NoError(t, err)
			manifests[rel] = fact
		}
	}
	slices.Sort(keys) // parent before each child
	var ns rebuildResolverNamespace
	for _, rel := range keys {
		kind := rebuildNamespaceRegular
		if dirs[rel] {
			kind = rebuildNamespaceDirectory
		}
		require.NoError(t, ns.add(rebuildNamespaceRow{key: rel, kind: kind}))
	}
	for _, rel := range slices.Backward(keys) {
		if dirs[rel] {
			require.NoError(t, ns.add(rebuildNamespaceRow{key: rel, kind: rebuildNamespaceCloseExact}))
		}
	}
	require.NoError(t, ns.add(rebuildNamespaceRow{key: ".", kind: rebuildNamespaceCloseExact}))
	require.NoError(t, ns.freeze())
	globs, err := newRebuildWorkspaceGlob(t.Context(), &ns)
	require.NoError(t, err)
	return rebuildNpmInputs{
		namespace: &ns,
		manifests: manifests,
		isFile: func(_ context.Context, rel string) (bool, error) {
			return ns.probeFile(rel)
		},
		matchDirs: globs.matchDirs,
	}
}

func TestRebuildNpmInputsManifestAuthority(t *testing.T) {
	src := []byte(`{"dependencies":{"foo":"npm:kept@1"}}`)
	left := int64(rebuildCppRawLimit)
	fact, err := normalizeRebuildNpmManifest("package.json", rebuildNpmTestIdentity("package.json", src), src, &left)
	require.NoError(t, err)
	wrongPath := fact
	wrongPath.Path = "other/package.json"
	wrongDigest := fact.Digest
	wrongDigest[0] ^= 1
	for _, tc := range []struct {
		label    string
		kind     rebuildNamespaceKind
		close    rebuildNamespaceKind
		fact     *rebuildNpmManifestFact
		followed rebuildDigest
		want     string
		err      error
	}{
		{label: "closed absence", close: rebuildNamespaceCloseExact},
		{label: "unproved absence", close: rebuildNamespaceClose, err: errRebuildResolverNamespace},
		{label: "denied", kind: rebuildNamespaceDenied, close: rebuildNamespaceCloseExact, fact: &fact, err: errRebuildResolverInputDenied},
		{label: "no positive fact", kind: rebuildNamespaceRegular, close: rebuildNamespaceCloseExact, err: errRebuildResolverNamespace},
		{label: "wrong path", kind: rebuildNamespaceRegular, close: rebuildNamespaceCloseExact, fact: &wrongPath, err: errRebuildResolverNamespace},
		{label: "positive contradicts absence", close: rebuildNamespaceCloseExact, fact: &fact, err: errRebuildResolverNamespace},
		{label: "unproved link", kind: rebuildNamespaceUnsupported, close: rebuildNamespaceCloseExact, fact: &fact, err: errRebuildResolverNamespace},
		{label: "verified root link", kind: rebuildNamespaceUnsupported, close: rebuildNamespaceCloseExact, fact: &fact, followed: fact.Digest, want: "kept"},
		{label: "wrong target", kind: rebuildNamespaceUnsupported, close: rebuildNamespaceCloseExact, fact: &fact, followed: wrongDigest, err: errRebuildResolverNamespace},
	} {
		t.Run(tc.label, func(t *testing.T) {
			var ns rebuildResolverNamespace
			if tc.kind != rebuildNamespaceUnknown {
				require.NoError(t, ns.add(rebuildNamespaceRow{key: "package.json", kind: tc.kind}))
			}
			require.NoError(t, ns.add(rebuildNamespaceRow{key: ".", kind: tc.close}))
			require.NoError(t, ns.freeze())
			manifests := make(map[string]rebuildNpmManifestFact)
			if tc.fact != nil {
				manifests["package.json"] = *tc.fact
			}
			lookup := newRebuildNpmLookup(t.Context(), rebuildNpmInputs{
				namespace: &ns, manifests: manifests, followedRoot: tc.followed,
				isFile: func(context.Context, string) (bool, error) {
					t.Fatal("no workspace file should be probed")
					return false, nil
				},
				matchDirs: func(context.Context, string, int) ([]string, error) {
					t.Fatal("no workspace pattern was proved")
					return nil, nil
				},
			})
			got, err := lookup.rewrite("use.ts", "foo")
			require.Equal(t, tc.want, got)
			if tc.err == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, errRebuildNpmInput)
			require.ErrorIs(t, err, tc.err)
			external, err := lookup.declaresExternal("use.ts", "foo")
			require.False(t, external)
			require.ErrorIs(t, err, tc.err)
		})
	}
}

package excludes

import (
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
)

func TestMatcherPrefilterPreservesAuthoritativeMatch(t *testing.T) {
	patternSets := [][]string{
		Builtin,
		{"node_modules/", "!node_modules/keep/"},
		{"a/b/*", "**/foo", "foo/**", "/foo/", "*.tmp"},
		{"foo", "*", "!keep"},
		{"foo", "obj/[Dd]ebug/", "[ab]", "?x"},
		{"!foo", "!bar/"},
		{"foo", `\!literal`, `\#literal`, `space\ name`},
		{"café/", "*.réserve", "!café/keep"},
		{"*$", "foo(bar)", "foo+bar", "foo|bar", "foo^bar"},
	}
	paths := []string{
		"", ".", "./", "a/b", "a/b/", "a/b/file", "foo", "foo/", "foo/bar", "a/foo", "a/foo/",
		"assets/logo.sketch", "internal/app/main.go", "node_modules/pkg/file", "node_modules/keep/file",
		"scratch.tmp", "scratch.tmp/child", "keep", "obj/Debug", "obj/Debug/file", "obj/debug/file",
		"a", "b", "ax", "!literal", "#literal", "space name", "café/file", "cafe\u0301/file", "café/keep",
		"doc.re\u0301serve", "foo$", "foo(bar)", "foo+bar", "foo|bar", "foo^bar", "./assets/logo.sketch",
	}
	root := t.TempDir()
	for _, patterns := range patternSets {
		m := New(patterns)
		for _, path := range paths {
			want, _ := m.Explain(path)
			if got := m.MatchRel(path); got != want {
				t.Errorf("patterns=%q path=%q: MatchRel=%v, Explain=%v", patterns, path, got, want)
			}
			abs := filepath.Join(root, filepath.FromSlash(path))
			for _, isDir := range []bool{false, true} {
				want, _ := m.ExplainAbsDir(abs, root, isDir)
				if got := m.MatchAbsDir(abs, root, isDir); got != want {
					t.Errorf("patterns=%q path=%q dir=%v: MatchAbsDir=%v, ExplainAbsDir=%v", patterns, path, isDir, got, want)
				}
			}
		}
	}
}

func FuzzMatcherPrefilterParity(f *testing.F) {
	for _, seed := range []struct{ patterns, path string }{
		{"a/b/*", "a/b"},
		{"foo/**", "foo"},
		{"**/foo", "foo"},
		{"node_modules/\n!node_modules/keep/", "node_modules/keep/file"},
		{"foo\nobj/[Dd]ebug/", "obj/Debug/file"},
		{"café/", "cafe\u0301/file"},
		{"*$", "ordinary"},
	} {
		f.Add(seed.patterns, seed.path)
	}
	f.Fuzz(func(t *testing.T, patterns, path string) {
		if len(patterns) > 1024 || len(path) > 1024 {
			t.Skip()
		}
		m := New(strings.Split(patterns, "\n"))
		want, _ := m.Explain(path)
		if got := m.MatchRel(path); got != want {
			t.Fatalf("patterns=%q path=%q: MatchRel=%v, Explain=%v", patterns, path, got, want)
		}
	})
}

// A no-negation list cannot enter the parent loop. Keep its cheap rejection
// ahead of the full compiled match, even for uncertain compiler forms.
func TestMatcherNoNegationUsesPrefilter(t *testing.T) {
	for _, pattern := range []string{"/**", "foo/**/", `\#*\#`} {
		m := New(append(append([]string{}, Builtin...), pattern))
		const rel = "src/pkg/server/main.go"
		if m.hasNegation || !m.checkUnmatchedParents || m.couldMatch(rel) {
			t.Fatalf("invalid prefilter fixture for %q", pattern)
		}
		// A sentinel full matcher makes an unnecessary evaluation observable
		// without a timing assertion on a loaded test host.
		full := New([]string{"*"})
		m.ign, m.dirs = full.ign, full.dirs
		if m.MatchRel(rel) {
			t.Errorf("%q: file miss reached the full matcher", pattern)
		}
		if m.MatchRelEither(rel) {
			t.Errorf("%q: either miss reached the full matcher", pattern)
		}
	}
}

func TestMatcherNoNegationPrefilterDifferential(t *testing.T) {
	rng := rand.New(rand.NewSource(951))
	pool := []string{"/**", "foo/**/", `\#*\#`, `.\#*`, "gen/", "*.tmp", "src/*", "a/**/b", "/root.go", "[ab]", "?x"}
	paths := []string{"main.go", "src/pkg/server/main.go", "foo/bar.go", "gen/keep.go", "root.go", "#lock#", ".#lock", "a/b", "a/deep/b", "cache.tmp", "ax", "b"}
	for range 400 {
		patterns := append(append([]string{}, Builtin...), `\#*\#`)
		for range 1 + rng.Intn(len(pool)) {
			patterns = append(patterns, pool[rng.Intn(len(pool))])
		}
		m := New(patterns)
		prefiltered := New(patterns)
		prefiltered.checkUnmatchedParents = false // 08391bef's no-negation prefilter path.
		for _, path := range paths {
			for _, rel := range []string{path, "sub/" + path, "deep/sub/" + path} {
				for _, candidate := range []string{rel, rel + "/"} {
					if got, want := m.MatchRel(candidate), prefiltered.MatchRel(candidate); got != want {
						t.Fatalf("patterns=%q path=%q: got %v, prefiltered %v", patterns, candidate, got, want)
					}
				}
				if got, want := m.MatchRelEither(rel), prefiltered.MatchRel(rel) && prefiltered.MatchRel(rel+"/"); got != want {
					t.Fatalf("patterns=%q path=%q: either %v, prefiltered pair %v", patterns, rel, got, want)
				}
			}
		}
	}
}

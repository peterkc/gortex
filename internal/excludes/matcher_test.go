package excludes

import (
	"path"
	"testing"
)

func TestMatcher_Nil(t *testing.T) {
	var m *Matcher
	if m.MatchRel("anything") {
		t.Fatal("nil matcher should never match")
	}
}

func TestMatcher_Builtin(t *testing.T) {
	m := New(Builtin)
	cases := []struct {
		path string
		want bool
	}{
		{".git/HEAD", true},
		{"pkg/.git/HEAD", true},
		{"src/node_modules/foo/index.js", true},
		{"vendor/lib/x.go", true},
		{"pkg/foo.go", false},
		{"README.md", false},
		{"tmp.tmp", true},
		{"deep/nested/file.swp", true},
		{".Makefile.swpx", true},
		{"src/.foo.swo", true},
		{"src/.foo.swn", true},
		{"backup~", true},
	}
	for _, tc := range cases {
		if got := m.MatchRel(tc.path); got != tc.want {
			t.Errorf("MatchRel(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestMatcher_Negation(t *testing.T) {
	// Exclude all of dist, except dist/keep
	m := New([]string{"dist/", "!dist/keep/**"})
	if !m.MatchRel("dist/junk.txt") {
		t.Error("dist/junk.txt should be excluded")
	}
	if m.MatchRel("dist/keep/foo.txt") {
		t.Error("dist/keep/foo.txt should be re-included")
	}
}

func TestMatcher_HasNegatedDescendant(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		relDir   string
		want     bool
	}{
		{
			// Issue #113: a blanket "dir/*" makes go-gitignore report the
			// parent directory itself as excluded; the re-include lives
			// beneath it, so the parent must keep being descended.
			name:     "blanket plus reinclude under parent",
			patterns: []string{"wp-content/plugins/*", "!wp-content/plugins/foo/"},
			relDir:   "wp-content/plugins",
			want:     true,
		},
		{
			// literalAnchor decodes the escaped plus, so this exact folder
			// stays reachable while unrelated folders remain prunable.
			name:     "escaped literal character keeps its folder",
			patterns: []string{"*", `!foo\+/keep.go`},
			relDir:   "foo+",
			want:     true,
		},
		{
			// A backslash before the slash is not a plain literal segment,
			// so the walk stays conservative.
			name:     "escaped slash stays conservative",
			patterns: []string{"*", `!foo\/keep.go`},
			relDir:   "foo",
			want:     true,
		},
		{
			name:     "escaped negation leaves unrelated folders prunable",
			patterns: []string{"*", `!foo\+/keep.go`},
			relDir:   "other",
			want:     false,
		},
		{
			name:     "sibling without reinclude is prunable",
			patterns: []string{"wp-content/plugins/*", "!wp-content/plugins/foo/"},
			relDir:   "wp-content/plugins/other",
			want:     false,
		},
		{
			name:     "unrelated dir is prunable",
			patterns: []string{"wp-content/plugins/*", "!wp-content/plugins/foo/"},
			relDir:   "node_modules",
			want:     false,
		},
		{
			name:     "reinclude two levels down keeps grandparent",
			patterns: []string{"a/*", "!a/b/c/"},
			relDir:   "a",
			want:     true,
		},
		{
			name:     "reinclude two levels down keeps parent",
			patterns: []string{"a/*", "!a/b/c/"},
			relDir:   "a/b",
			want:     true,
		},
		{
			name:     "reinclude two levels down prunes uninvolved sibling",
			patterns: []string{"a/*", "!a/b/c/", "!a/x"},
			relDir:   "a/y",
			want:     false,
		},
		{
			name:     "unanchored basename negation keeps every dir",
			patterns: []string{"build/", "!keep.txt"},
			relDir:   "build",
			want:     true,
		},
		{
			name:     "wildcard-leading negation keeps every dir",
			patterns: []string{"build/", "!**/keep/"},
			relDir:   "build",
			want:     true,
		},
		{
			name:     "no negations never keeps",
			patterns: []string{"build/", "dist/"},
			relDir:   "build",
			want:     false,
		},
		{
			name:     "root-anchored file negation prunes other dirs",
			patterns: []string{"/*", "!/AGENTS.md", ".worktrees/"},
			relDir:   ".worktrees",
			want:     false,
		},
		{
			name:     "root-anchored file negation keeps the root",
			patterns: []string{"/*", "!/AGENTS.md"},
			relDir:   "",
			want:     true,
		},
		{
			name:     "root-anchored dir negation keeps its subtree",
			patterns: []string{"/*", "!/agent/"},
			relDir:   "agent/skills",
			want:     true,
		},
		{
			name:     "root-anchored wildcard negation stays conservative",
			patterns: []string{"/*", "!/*.md"},
			relDir:   ".worktrees",
			want:     true,
		},
		{
			name:     "trailing-slash relDir is tolerated",
			patterns: []string{"a/*", "!a/b/c/"},
			relDir:   "a/b/",
			want:     true,
		},
		{
			name:     "leading-dot-slash relDir is tolerated",
			patterns: []string{"a/*", "!a/b/c/"},
			relDir:   "./a",
			want:     true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New(tc.patterns)
			if got := m.HasNegatedDescendant(tc.relDir); got != tc.want {
				t.Errorf("HasNegatedDescendant(%q) = %v, want %v", tc.relDir, got, tc.want)
			}
		})
	}
}

func TestMatcher_HasNegatedDescendant_Nil(t *testing.T) {
	var m *Matcher
	if m.HasNegatedDescendant("anything") {
		t.Fatal("nil matcher should report no negated descendants")
	}
}

func TestMatcher_CommentsAndEmpty(t *testing.T) {
	m := New([]string{"", "# comment", "foo/"})
	pats := m.Patterns()
	if len(pats) != 1 || pats[0] != "foo/" {
		t.Errorf("expected [foo/], got %v", pats)
	}
}

// TestMatcher_EscapedNegationsAgreeWithMatchRel checks both descendant
// checks against the compiled matcher: when a negation with an escape
// re-includes a file, neither check may prune the file's folder.
func TestMatcher_EscapedNegationsAgreeWithMatchRel(t *testing.T) {
	negations := []string{
		`!foo\*/keep.go`, `!foo\?/keep.go`, `!foo\[/keep.go`, `!foo\+/keep.go`,
		`!foo\./keep.go`, `!foo\\/keep.go`, `!foo\/keep.go`, `!foo\a/keep.go`,
		`!foo\ /keep.go`, `!foo\$/keep.go`, `!foo\(/keep.go`, `!foo\]/keep.go`,
		`!/foo\?`, `!/foo\*`, `!/foo\.`,
		`![abc/keep.go`, `!/[abc`,
	}
	files := []string{
		"foo/keep.go", "foo*/keep.go", "foo?/keep.go", "foo[/keep.go",
		"foo+/keep.go", "foo./keep.go", `foo\/keep.go`, `foo\x/keep.go`,
		"fooa/keep.go", "foo\a/keep.go", "foo /keep.go", "foo$/keep.go",
		"foo(/keep.go", "foo]/keep.go", "foox/keep.go",
		"[abc/keep.go", "[abc",
	}
	checked := 0
	for _, neg := range negations {
		m := New([]string{"*", neg})
		for _, f := range files {
			if m.MatchRel(f) {
				continue
			}
			checked++
			dir := path.Dir(f)
			if !m.HasNegatedDescendant(dir) {
				t.Errorf("%s re-includes %q, but HasNegatedDescendant(%q) = false", neg, f, dir)
			}
			if !m.HasAnchoredNegatedDescendant(dir) {
				t.Errorf("%s re-includes %q, but HasAnchoredNegatedDescendant(%q) = false", neg, f, dir)
			}
		}
	}
	if checked < len(negations)-2 {
		t.Fatalf("only %d re-included files checked; the fixture no longer exercises the matcher", checked)
	}
}

func TestIsBracketExpr(t *testing.T) {
	for s, want := range map[string]bool{"[ab]": true, "[ab]c": true, "[abc": false, "[z-a]": false} {
		if got := isBracketExpr(s); got != want {
			t.Fatalf("isBracketExpr(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestMatcher_HasAnchoredNegatedDescendant(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pattern string
		dir     string
		want    bool
	}{
		{"unanchored_gitkeep", "!.gitkeep", "node_modules/pkg", false},
		{"unanchored_env", "!.env.example", "node_modules/pkg", false},
		{"wildcard_led", "!**/.gitkeep", "node_modules/pkg", false},
		{"root_wildcard_led", "!/**/.gitkeep", "node_modules/pkg", false},
		{"root_anchored_unrelated", "!/AGENTS.md", "node_modules/pkg", false},
		{"root_anchored_at_root", "!/AGENTS.md", ".", true},
		{"anchored_child", "!gen/keep.go", "gen", true},
		{"root_anchored_child", "!/gen/keep.go", "gen", true},
		{"root_anchored_folder", "!/gen/", "gen/sub", true},
		{"anchored_parent", "!gen/sub/keep.go", "./gen/", true},
		{"anchored_unrelated", "!gen/keep.go", "other", false},
		// An escaped wildcard names a literal folder; such patterns stay conservative.
		{"escaped_first_segment", `!foo\*/keep.go`, "foo*", true},
		{"root_escaped_segment", `!/foo\*`, "foo*", true},
		{"unanchored_escaped", `!foo\*`, "node_modules/pkg", false},
		{"escaped_literal_char", `!foo\+/keep.go`, "foo+", true},
		{"escaped_unrelated", `!foo\+/keep.go`, "other", false},
		// An unclosed "[" is literal in the compiled matcher; a closed one
		// is a wildcard-led negation, which anchored mode leaves out.
		{"unclosed_bracket", "![abc/keep.go", "[abc", true},
		{"unclosed_bracket_unrelated", "![abc/keep.go", "other", false},
		{"bracket_class_first", "![ab]c/keep.go", "ac", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := New([]string{tc.pattern})
			if got := m.HasAnchoredNegatedDescendant(tc.dir); got != tc.want {
				t.Fatalf("HasAnchoredNegatedDescendant(%q) = %v, want %v", tc.dir, got, tc.want)
			}
		})
	}
	var m *Matcher
	if m.HasAnchoredNegatedDescendant("gen") || New(nil).HasAnchoredNegatedDescendant("gen") {
		t.Fatal("nil and empty matchers have no negated descendants")
	}
}

// Expected classifications were captured from isAnchoredNegation before
// negation parsing moved into New.
func TestMatcher_ParsedNegationClassification(t *testing.T) {
	for _, tc := range []struct {
		pattern  string
		anchored bool
		atRoot   bool
	}{
		{"!", false, false},
		{"!/", false, false},
		{"!//", false, false},
		{"!.gitkeep", false, false},
		{"!keep/", false, false},
		{"!/AGENTS.md", true, true},
		{"!/gen/", true, true},
		{"!gen/keep.go", true, true},
		{"!/gen/keep.go", true, true},
		{"!**/keep", false, false},
		{"!/**/keep", false, false},
		{"!*/keep", false, false},
		{"!/*.md", false, false},
		{"!?foo/keep", false, false},
		{"![ab]c/keep", false, false},
		{"![abc/keep", true, true},
		{"!/[abc", true, true},
		{"![z-a]/keep", true, true},
		{"!foo\\*/keep", true, true},
		{"!foo\\+/keep", true, true},
		{"!foo\\?/keep", true, true},
		{"!foo\\/keep", true, true},
		{"!foo\\a/keep", true, true},
		{"!/foo\\?", true, true},
		{"!foo\\*", false, false},
		{"!foo\\\\/keep", true, true},
		// Preserve the old empty-first-segment classification and match result.
		{"!//*/keep", true, false},
		{"!//foo/keep", true, true},
	} {
		t.Run(tc.pattern, func(t *testing.T) {
			m := New([]string{tc.pattern})
			if m.hasAnchoredNegation != tc.anchored {
				t.Errorf("anchored classification = %v, want %v", m.hasAnchoredNegation, tc.anchored)
			}
			if got := m.HasAnchoredNegatedDescendant("."); got != tc.atRoot {
				t.Errorf("anchored descendant at root = %v, want %v", got, tc.atRoot)
			}
			for _, dir := range []string{"", ".", "./gen/", "gen", "gen/sub", "other", "foo*", "foo+", "[abc", "node_modules/pkg"} {
				if m.HasAnchoredNegatedDescendant(dir) && !m.HasNegatedDescendant(dir) {
					t.Errorf("anchored descendant under %q was not counted by the walk", dir)
				}
			}
		})
	}
}

func TestMatcher_NegatedDescendantAllocs(t *testing.T) {
	for _, pattern := range []string{"!gen/keep.go", `!foo\*/keep.go`, "!gen/[ab]/keep.go", "!**/keep.go", "!long-directory-name-that-exceeds-the-stack-buffer/keep.go"} {
		t.Run(pattern, func(t *testing.T) {
			m := New([]string{"gen/", pattern})
			for _, dir := range []string{"gen", "other", "long-directory-name-that-exceeds-the-stack-buffer"} {
				if allocs := testing.AllocsPerRun(100, func() { m.HasNegatedDescendant(dir) }); allocs != 0 {
					t.Errorf("HasNegatedDescendant(%q) allocates %v times, want 0", dir, allocs)
				}
				if allocs := testing.AllocsPerRun(100, func() { m.HasAnchoredNegatedDescendant(dir) }); allocs != 0 {
					t.Errorf("HasAnchoredNegatedDescendant(%q) allocates %v times, want 0", dir, allocs)
				}
			}
		})
	}
}

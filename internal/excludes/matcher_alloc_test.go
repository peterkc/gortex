//go:build !race

package excludes

import (
	"path/filepath"
	"testing"
)

// Regexp uses sync.Pool. Race builds deliberately discard pooled values,
// so their allocation count measures detector-induced misses too.
func TestMatcherNegationMatchRelAllocs(t *testing.T) {
	patterns := append(append([]string{}, Builtin...), "*.log", "!keep.log", ".env", "!.env.example")
	m := New(patterns)
	if m.checkUnmatchedParents {
		t.Fatal("ordinary negation list must retain the unmatched-path fast path")
	}
	if _, ip := m.ign.MatchesPathHow("internal/indexer/builder/sub/deep/file_build.go"); ip != nil {
		t.Fatal("unmatched-path allocation fixture must have no positive match")
	}
	for _, tc := range []struct {
		path     string
		excluded bool
	}{
		{"internal/indexer/builder/sub/deep/file_build.go", false},
		{"node_modules/pkg/keep.log", true},
		{"logs/keep.log", false},
	} {
		if got := m.MatchRel(tc.path); got != tc.excluded {
			t.Fatalf("MatchRel(%q)=%v, want %v", tc.path, got, tc.excluded)
		}
		if allocs := testing.AllocsPerRun(100, func() { m.MatchRel(tc.path) }); allocs != 0 {
			t.Errorf("MatchRel(%q) allocates %v times, want 0", tc.path, allocs)
		}
	}
}

func TestMatcherUnrelatedIncludeAllocs(t *testing.T) {
	root := t.TempDir()
	const rel = "src/a/b/c/d/e/f/g.go"
	abs := filepath.Join(root, filepath.FromSlash(rel))
	for _, include := range []string{"*.proto", "Pods/"} {
		t.Run(include, func(t *testing.T) {
			m := NewWithIncludes(Builtin, []string{include})
			for _, tc := range []struct {
				name  string
				match func() bool
			}{
				{"walk", func() bool { matched, _ := m.ExplainAbsDirInOpenDir(abs, root, false); return matched }},
				{"watcher", func() bool { return m.MatchRelEither(rel) }},
				{"include", func() bool { return m.IncludesRel(rel) }},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if tc.match() {
						t.Fatal("unrelated path must remain admitted")
					}
					if allocs := testing.AllocsPerRun(100, func() { tc.match() }); allocs != 0 {
						t.Errorf("unrelated include allocates %v times, want 0", allocs)
					}
				})
			}
		})
	}
}

//go:build !race

package excludes

import "testing"

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

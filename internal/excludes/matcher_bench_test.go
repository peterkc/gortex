package excludes

import "testing"

var negatedDescendantResult bool

func BenchmarkMatcherNegatedDescendant(b *testing.B) {
	for _, tc := range []struct {
		name     string
		patterns []string
		dir      string
	}{
		{"literal", []string{"gen/", "!gen/keep.go"}, "gen"},
		{"escaped", []string{"gen/", `!foo\*/keep.go`}, "foo*"},
		{"bracket", []string{"gen/", "!gen/[ab]/keep.go"}, "gen"},
		{"wildcard_leading", []string{"gen/", "!**/keep.go"}, "gen"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			m := New(tc.patterns)
			b.Run("all", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					negatedDescendantResult = m.HasNegatedDescendant(tc.dir)
				}
			})
			b.Run("anchored", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					negatedDescendantResult = m.HasAnchoredNegatedDescendant(tc.dir)
				}
			})
		})
	}
}

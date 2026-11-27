package contracts

import (
	"testing"
)

// TestExtractGoShape_NestedAnonymousStruct guards the regression
// where the line-based extractor hoisted fields of an inline
// anonymous struct into the outer struct's field list — producing
// duplicate names when the parent and substruct shared field names
// (e.g. dashboardSnapshot's outer `Repos` / `Caveats` clashing with
// the embedded Stats.Repos / Stats.Caveats fields). React then
// rendered two children with the same key and warned in the
// dashboard.
//
// Fix: track brace depth and only emit fields at depth 0 (direct
// members of the outer struct). Nested-struct fields are dropped
// rather than mis-attributed.
func TestExtractGoShape_NestedAnonymousStruct(t *testing.T) {
	src := []byte(`type dashboardSnapshot struct {
	Stats struct {
		TotalNodes int    ` + "`json:\"total_nodes\"`" + `
		TotalEdges int    ` + "`json:\"total_edges\"`" + `
		Repos      int    ` + "`json:\"repos\"`" + `
		Caveats    int    ` + "`json:\"caveats\"`" + `
		Version    string ` + "`json:\"version\"`" + `
	} ` + "`json:\"stats\"`" + `
	Kinds     []kvEntry      ` + "`json:\"kinds\"`" + `
	Languages []kvEntry      ` + "`json:\"languages\"`" + `
	Repos     []repoEntry    ` + "`json:\"repos\"`" + `
	Activity  []changeEvent  ` + "`json:\"activity\"`" + `
	Caveats   []caveatEntry  ` + "`json:\"caveats\"`" + `
	Processes []processEntry ` + "`json:\"processes\"`" + `
}
`)
	shape := ExtractShape("dashboard.go", src, 1, 16)
	if shape == nil {
		t.Fatal("expected non-nil shape")
	}
	seen := map[string]int{}
	for _, f := range shape.Fields {
		seen[f.Name]++
	}
	for name, count := range seen {
		if count > 1 {
			t.Errorf("field %q appears %d times — duplicate names hoist nested struct into parent", name, count)
		}
	}
	// Each top-level field must show up exactly once. We don't assert
	// the parent `stats` is present (the regex doesn't match the
	// `Stats struct {` line — that's an acceptable under-report; the
	// alternative — over-reporting with duplicates — broke the React
	// tree).
	wantTopLevel := []string{"kinds", "languages", "repos", "activity", "caveats", "processes"}
	for _, name := range wantTopLevel {
		if seen[name] != 1 {
			t.Errorf("top-level field %q expected once, got %d", name, seen[name])
		}
	}
	// Inner Stats fields MUST NOT be hoisted (they collide with the
	// outer ones). Specifically `total_nodes` is unique to the inner
	// struct, so its presence proves the bug recurred.
	if seen["total_nodes"] > 0 {
		t.Errorf("inner Stats.TotalNodes leaked into outer shape (hoisting regression)")
	}
}

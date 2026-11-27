package contracts

import "testing"

// TestExtractTSShape_NestedAnonymousObject is the regression for the
// React duplicate-key warning that the dashboard fired on
// /v1/dashboard. The web `DashboardSnapshot` type literal contains
// an inline anonymous `stats: { ... }` whose fields collide with
// the outer ones (`repos`, `caveats`). The pre-fix extractor walked
// every line in the body, so the inner Stats fields were emitted
// alongside the outer arrays — producing duplicate `repos` /
// `caveats` rows in the shape and two React children with the same
// key in the rendered table.
//
// Fix: track brace depth and only emit fields at depth 0 (direct
// members of the outer `{`). Inline-object fields drop their nested
// members; the parent field itself still appears with type `{...}`
// so the structure isn't lost.
func TestExtractTSShape_NestedAnonymousObject(t *testing.T) {
	src := []byte(`export type DashboardSnapshot = {
  stats: {
    total_nodes: number
    total_edges: number
    repos: number
    caveats: number
    version: string
  }
  kinds: KindCount[]
  languages: LanguageCount[]
  repos: Repo[]
  activity: Activity[]
  caveats: Caveat[]
  processes: Process[]
}
`)
	shape := ExtractShape("dashboard.ts", src, 1, 15)
	if shape == nil {
		t.Fatal("expected non-nil shape")
	}
	seen := map[string]int{}
	for _, f := range shape.Fields {
		seen[f.Name]++
	}
	for name, count := range seen {
		if count > 1 {
			t.Errorf("field %q appears %d times — duplicate names hoist nested object into parent", name, count)
		}
	}
	wantTopLevel := []string{"stats", "kinds", "languages", "repos", "activity", "caveats", "processes"}
	for _, name := range wantTopLevel {
		if seen[name] != 1 {
			t.Errorf("top-level field %q expected once, got %d", name, seen[name])
		}
	}
	// total_nodes is unique to the inner Stats object — its presence
	// proves the bug recurred.
	if seen["total_nodes"] > 0 {
		t.Errorf("inner Stats.total_nodes leaked into outer shape (hoisting regression)")
	}
	// The parent `stats` field MUST be recorded with the normalised
	// inline-object label rather than a stray opening brace, so the
	// dashboard renders something readable.
	for _, f := range shape.Fields {
		if f.Name == "stats" {
			if f.Type != "{...}" {
				t.Errorf("stats type: want %q, got %q", "{...}", f.Type)
			}
		}
	}
}

// TestExtractTSShape_FlatInterfaceUnchanged guards against the
// brace-depth tracking accidentally suppressing fields in a normal
// flat interface — the hot path that has to keep working.
func TestExtractTSShape_FlatInterfaceUnchanged(t *testing.T) {
	src := []byte(`export interface User {
  id: string
  name: string
  email?: string
}
`)
	shape := ExtractShape("dashboard.ts", src, 1, 5)
	if shape == nil {
		t.Fatal("expected non-nil shape")
	}
	if len(shape.Fields) != 3 {
		t.Fatalf("want 3 fields, got %d", len(shape.Fields))
	}
	want := []string{"id", "name", "email"}
	for i, w := range want {
		if shape.Fields[i].Name != w {
			t.Errorf("field %d: want %q, got %q", i, w, shape.Fields[i].Name)
		}
	}
	if shape.Fields[2].Required {
		t.Errorf("email should be optional (?:)")
	}
}

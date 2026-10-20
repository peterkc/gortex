package excludes

import (
	"os"
	"path/filepath"
	"testing"
)

func mkIgnore(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHierarchical_RootLevel(t *testing.T) {
	root := t.TempDir()
	mkIgnore(t, filepath.Join(root, ".gortexignore"), "*.gen.go\nbuild/\n")
	h := NewHierarchical(root, ".gortexignore")

	if !h.Match(filepath.Join(root, "foo.gen.go"), false) {
		t.Error("root .gortexignore should exclude foo.gen.go")
	}
	if h.Match(filepath.Join(root, "foo.go"), false) {
		t.Error("foo.go should not be excluded")
	}
	if !h.Match(filepath.Join(root, "build"), true) {
		t.Error("build/ directory should be excluded so the subtree is pruned")
	}
	if !h.Match(filepath.Join(root, "pkg", "sub", "foo.gen.go"), false) {
		t.Error("root pattern should reach a deeply nested foo.gen.go")
	}
}

func TestHierarchical_PerDirectory(t *testing.T) {
	root := t.TempDir()
	// An ignore file deep in the tree scopes only to its own subtree.
	mkIgnore(t, filepath.Join(root, "internal", ".gortexignore"), "secret.go\n")
	h := NewHierarchical(root, ".gortexignore")

	if !h.Match(filepath.Join(root, "internal", "secret.go"), false) {
		t.Error("internal/.gortexignore should exclude internal/secret.go")
	}
	if !h.Match(filepath.Join(root, "internal", "sub", "secret.go"), false) {
		t.Error("internal/.gortexignore should reach internal/sub/secret.go")
	}
	if h.Match(filepath.Join(root, "secret.go"), false) {
		t.Error("internal/.gortexignore must not affect a root-level secret.go")
	}
	if h.Match(filepath.Join(root, "cmd", "secret.go"), false) {
		t.Error("internal/.gortexignore must not affect a sibling directory")
	}
}

func TestHierarchical_Nested(t *testing.T) {
	root := t.TempDir()
	mkIgnore(t, filepath.Join(root, ".gortexignore"), "*.tmp\n")
	mkIgnore(t, filepath.Join(root, "pkg", ".gortexignore"), "local.go\nvendored/\n")
	h := NewHierarchical(root, ".gortexignore")

	// Both the root rule and the pkg rule apply under pkg/.
	if !h.Match(filepath.Join(root, "pkg", "x.tmp"), false) {
		t.Error("root rule should still apply inside pkg/")
	}
	if !h.Match(filepath.Join(root, "pkg", "local.go"), false) {
		t.Error("pkg/.gortexignore should exclude pkg/local.go")
	}
	if !h.Match(filepath.Join(root, "pkg", "vendored"), true) {
		t.Error("pkg/.gortexignore should prune the pkg/vendored/ subtree")
	}
	if h.Match(filepath.Join(root, "local.go"), false) {
		t.Error("pkg/.gortexignore must not reach the root")
	}
}

func TestHierarchical_Negation(t *testing.T) {
	root := t.TempDir()
	mkIgnore(t, filepath.Join(root, ".gortexignore"), "*.log\n!keep.log\n")
	h := NewHierarchical(root, ".gortexignore")

	if !h.Match(filepath.Join(root, "debug.log"), false) {
		t.Error("*.log should exclude debug.log")
	}
	if h.Match(filepath.Join(root, "keep.log"), false) {
		t.Error("!keep.log should re-include keep.log")
	}
}

func TestHierarchical_HasNegatedDescendant(t *testing.T) {
	root := t.TempDir()
	// A blanket "dir/*" plus a re-include of one child, expressed in a
	// per-directory ignore file. The walk must keep descending pkg/ so
	// the re-included pkg/keep/ subtree is reached.
	mkIgnore(t, filepath.Join(root, ".gortexignore"), "pkg/*\n!pkg/keep/\n")
	h := NewHierarchical(root, ".gortexignore")

	if !h.HasNegatedDescendant(filepath.Join(root, "pkg")) {
		t.Error("pkg/ has a re-included child and must not be pruned")
	}
	if h.HasNegatedDescendant(filepath.Join(root, "pkg", "other")) {
		t.Error("pkg/other has no re-included descendant and is prunable")
	}
	if h.HasNegatedDescendant(filepath.Join(root, "unrelated")) {
		t.Error("an unrelated directory has no re-included descendant")
	}
}

func TestHierarchical_HasNegatedDescendant_Anchored(t *testing.T) {
	root := t.TempDir()
	// The negation lives in a nested ignore file, anchored at internal/.
	mkIgnore(t, filepath.Join(root, "internal", ".gortexignore"), "build/*\n!build/keep/\n")
	h := NewHierarchical(root, ".gortexignore")

	if !h.HasNegatedDescendant(filepath.Join(root, "internal", "build")) {
		t.Error("internal/build has a re-included child via the nested ignore file")
	}
	if h.HasNegatedDescendant(filepath.Join(root, "build")) {
		t.Error("a root-level build/ is unaffected by internal/.gortexignore")
	}
}

func TestHierarchical_HasNegatedDescendant_NilAndEmpty(t *testing.T) {
	var h *Hierarchical
	if h.HasNegatedDescendant("/anything") {
		t.Error("nil Hierarchical should report no negated descendants")
	}
	root := t.TempDir()
	mkIgnore(t, filepath.Join(root, ".gortexignore"), "pkg/*\n!pkg/keep/\n")
	empty := NewHierarchical(root) // no filenames configured
	if empty.HasNegatedDescendant(filepath.Join(root, "pkg")) {
		t.Error("an empty filename list should report no negated descendants")
	}
}

func TestHierarchical_NoFiles(t *testing.T) {
	root := t.TempDir()
	mkIgnore(t, filepath.Join(root, "foo.go"), "package x\n")
	h := NewHierarchical(root, ".gortexignore")
	if h.Match(filepath.Join(root, "foo.go"), false) {
		t.Error("with no ignore files, nothing should be excluded")
	}
}

func TestHierarchical_OutsideRoot(t *testing.T) {
	root := t.TempDir()
	mkIgnore(t, filepath.Join(root, ".gortexignore"), "*\n")
	h := NewHierarchical(root, ".gortexignore")
	if h.Match(filepath.Join(filepath.Dir(root), "elsewhere.go"), false) {
		t.Error("a path outside the root must never be excluded")
	}
}

func TestHierarchical_NilAndEmpty(t *testing.T) {
	var h *Hierarchical
	if h.Match("/anything", false) {
		t.Error("nil Hierarchical should match nothing")
	}
	root := t.TempDir()
	mkIgnore(t, filepath.Join(root, ".gortexignore"), "*\n")
	empty := NewHierarchical(root) // no filenames configured
	if empty.Match(filepath.Join(root, "foo.go"), false) {
		t.Error("an empty filename list should match nothing")
	}
}

func TestHierarchical_MultipleFilenames(t *testing.T) {
	root := t.TempDir()
	mkIgnore(t, filepath.Join(root, ".gortexignore"), "a.go\n")
	mkIgnore(t, filepath.Join(root, ".ignore"), "b.go\n")
	h := NewHierarchical(root, ".gortexignore", ".ignore")

	if !h.Match(filepath.Join(root, "a.go"), false) {
		t.Error(".gortexignore pattern should apply")
	}
	if !h.Match(filepath.Join(root, "b.go"), false) {
		t.Error(".ignore pattern should apply")
	}
	if h.Match(filepath.Join(root, "c.go"), false) {
		t.Error("c.go should not be excluded")
	}
}

func TestHierarchical_HasAnchoredNegatedDescendant(t *testing.T) {
	for _, tc := range []struct {
		name  string
		file  string
		rules string
		dir   string
		want  bool
	}{
		{"unanchored_gitkeep", ".gortexignore", "vendor2/\n!.gitkeep\n", "vendor2/pkg", false},
		{"unanchored_env", ".gortexignore", "!.env.example\n", "vendor2/pkg", false},
		{"wildcard_led", ".gortexignore", "!**/.gitkeep\n", "vendor2/pkg", false},
		{"root_unrelated", ".gortexignore", "!/AGENTS.md\n", "vendor2/pkg", false},
		{"root_at_root", ".gortexignore", "!/AGENTS.md\n", ".", true},
		{"anchored", ".gortexignore", "gen/\n!gen/keep.go\n", "gen", true},
		{"root_anchored", ".gortexignore", "gen/\n!/gen/keep.go\n", "gen", true},
		{"nested", "sub/.gortexignore", "gen/\n!gen/keep.go\nother/\n", "sub/gen", true},
		{"nested_unrelated", "sub/.gortexignore", "gen/\n!gen/keep.go\nother/\n", "sub/other", false},
		{"nested_at_folder", "sub/.gortexignore", "!/AGENTS.md\n", "sub", true},
		{"nested_scoping", "sub/.gortexignore", "gen/\n!gen/keep.go\n", "gen", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			mkIgnore(t, filepath.Join(root, tc.file), tc.rules)
			h := NewHierarchical(root, ".gortexignore")
			if got := h.HasAnchoredNegatedDescendant(filepath.Join(root, tc.dir)); got != tc.want {
				t.Fatalf("HasAnchoredNegatedDescendant(%q) = %v, want %v", tc.dir, got, tc.want)
			}
			if h.HasAnchoredNegatedDescendant(filepath.Join(root, "..", "outside")) {
				t.Fatal("outside path has no negated descendant")
			}
		})
	}
	var h *Hierarchical
	if h.HasAnchoredNegatedDescendant("/anything") || NewHierarchical(t.TempDir()).HasAnchoredNegatedDescendant("/anything") {
		t.Fatal("nil and empty matchers have no negated descendants")
	}
}

func TestHierarchicalForgetRereadsIgnoreFile(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	ignore := filepath.Join(sub, ".gortexignore")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	h := NewHierarchical(root, ".gortexignore")
	target := filepath.Join(sub, "gen", "x.go")
	if h.Match(target, false) {
		t.Fatal("no ignore file yet")
	}
	if err := os.WriteFile(ignore, []byte("gen/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if h.Match(target, false) {
		t.Fatal("the cached empty rules stay until Forget")
	}
	h.Forget(sub)
	if !h.Match(target, false) {
		t.Fatal("Forget must reread the folder's ignore file")
	}
}

// A folder moved away and replaced by another tree must not keep the old
// tree's rules at any depth.
func TestHierarchicalForgetDropsDescendants(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	mkIgnore(t, filepath.Join(sub, "nested", ".gortexignore"), "gen/\n")
	h := NewHierarchical(root, ".gortexignore")
	target := filepath.Join(sub, "nested", "gen", "x.go")
	if !h.Match(target, false) {
		t.Fatal("nested rule applies")
	}
	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sub, "nested", "gen"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.Forget(sub)
	if h.Match(target, false) {
		t.Fatal("Forget(sub) must drop the cached rules of sub/nested")
	}
	// Forget on a path that is not cached, such as a removed file, leaves
	// the other entries alone.
	h.Forget(filepath.Join(root, "other.go"))
	h.mu.RLock()
	n := len(h.cache)
	h.mu.RUnlock()
	if n != 4 {
		t.Fatalf("cache entries = %d, want root, sub, sub/nested and sub/nested/gen", n)
	}
}

// A folder is cached only after its parent, which lets Forget skip the
// scan for a folder that is not cached.
func TestHierarchicalCachesFolderOnlyAfterParent(t *testing.T) {
	root := t.TempDir()
	h := NewHierarchical(root, ".gortexignore")
	sub := filepath.Join(root, "sub")
	h.dirMatcher(sub)
	h.mu.RLock()
	_, ok := h.cache[sub]
	h.mu.RUnlock()
	if ok {
		t.Fatal("sub cached before root")
	}
	h.dirMatcher(root)
	h.dirMatcher(sub)
	h.mu.RLock()
	_, ok = h.cache[sub]
	h.mu.RUnlock()
	if !ok {
		t.Fatal("sub not cached after root")
	}
}

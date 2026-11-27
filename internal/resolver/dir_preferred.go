package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
)

// ResolveByConvention resolves a symbol name to its definition node by the
// directory-convention heuristic shared across the per-framework
// middleware/controller/service/helper resolvers. It:
//
//	(a) finds candidate symbols by name — and, when suffix is set and name
//	    ends with it, also by the suffix-stripped name (so `authMiddleware`
//	    matches a definition named `auth`);
//	(b) prefers a candidate under one of preferDirs (substring match on the
//	    candidate's file path, e.g. "/middleware/");
//	(c) falls back to a candidate in fromFile's own directory;
//	(d) returns the unambiguous top match with a confidence tier —
//	    exact-dir 0.9, same-dir 0.85, sole-candidate 0.7 — or ("", 0) when
//	    the choice is ambiguous or nothing matched.
//
// This is the one tested primitive the express/laravel/rails/spring/etc.
// `*Service`/`*Controller` heuristics build on.
func ResolveByConvention(g graph.Store, name, suffix string, preferDirs []string, fromFile string) (string, float64) {
	if g == nil || name == "" {
		return "", 0
	}
	cands := dirConventionCandidates(g, name, suffix)
	if len(cands) == 0 {
		return "", 0
	}

	// Tier 1 — a candidate under a preferred directory.
	var preferred []*graph.Node
	for _, c := range cands {
		if dirMatchesAny(c.FilePath, preferDirs) {
			preferred = append(preferred, c)
		}
	}
	switch len(preferred) {
	case 1:
		return preferred[0].ID, 0.9
	case 0:
		// fall through to same-dir / sole-candidate tiers
	default:
		// Several candidates in preferred dirs — break the tie by the
		// caller's own directory, else ambiguous.
		if id := uniqueInDir(preferred, dirOf(fromFile)); id != "" {
			return id, 0.9
		}
		return "", 0
	}

	// Tier 2 — a candidate in the caller's own directory.
	if id := uniqueInDir(cands, dirOf(fromFile)); id != "" {
		return id, 0.85
	}

	// Tier 3 — a sole candidate anywhere.
	if len(cands) == 1 {
		return cands[0].ID, 0.7
	}

	// Ambiguous.
	return "", 0
}

// dirConventionCandidates returns the resolvable symbol nodes matching name
// (and the suffix-stripped name when applicable).
func dirConventionCandidates(g graph.Store, name, suffix string) []*graph.Node {
	seen := map[string]bool{}
	var out []*graph.Node
	for _, nm := range dirConventionNames(name, suffix) {
		for _, n := range g.FindNodesByName(nm) {
			if n == nil || seen[n.ID] || !isConventionResolvable(n) {
				continue
			}
			seen[n.ID] = true
			out = append(out, n)
		}
	}
	return out
}

func dirConventionNames(name, suffix string) []string {
	names := []string{name}
	if suffix != "" && len(name) > len(suffix) && strings.HasSuffix(name, suffix) {
		names = append(names, strings.TrimSuffix(name, suffix))
	}
	return names
}

// isConventionResolvable reports whether a node is a real definition this
// heuristic may bind to (a function/method/type, not a stub or unresolved
// placeholder).
func isConventionResolvable(n *graph.Node) bool {
	switch n.Kind {
	case graph.KindFunction, graph.KindMethod, graph.KindType, graph.KindInterface, graph.KindVariable, graph.KindPackage:
	default:
		return false
	}
	return !graph.IsStub(n.ID) && !graph.IsUnresolvedTarget(n.ID)
}

// dirMatchesAny reports whether filePath contains any of the preferred
// directory segments. A preferDir is matched both as written and with
// surrounding slashes trimmed, so "/middleware/" and "middleware" both work.
func dirMatchesAny(filePath string, dirs []string) bool {
	filePath = graphpath.Norm(filePath)
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if strings.Contains(filePath, d) || strings.Contains(filePath, "/"+strings.Trim(d, "/")+"/") {
			return true
		}
	}
	return false
}

// uniqueInDir returns the sole candidate whose directory equals dir, or ""
// when there are zero or more than one (and dir is non-empty).
func uniqueInDir(cands []*graph.Node, dir string) string {
	if dir == "" {
		return ""
	}
	id := ""
	for _, c := range cands {
		if dirOf(c.FilePath) == dir {
			if id != "" {
				return "" // two in the same dir: ambiguous
			}
			id = c.ID
		}
	}
	return id
}

// dirOf returns the '/'-joined directory portion of a store path.
func dirOf(path string) string {
	return graphpath.Dir(path)
}

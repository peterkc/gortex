package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// GDScript preload-alias resolution. A script with no `class_name` is
// still reachable by name — every file that wants it declares its own
// alias:
//
//	const Persist = preload("res://src/save/Persist.gd")
//	...
//	Persist.snapshot(self)
//
// This is the standard way to reach a static-utility script, and it is
// the shape that survives when a project deliberately keeps a script out
// of the global class table. The extractor stamps the alias as the
// call's receiver_type and records the loaded path on the const itself;
// nothing else in the graph connects the two, because the alias is
// file-local — the same name means a different script one directory over.
//
// This pass supplies that link, per file: it reads the alias consts each
// script declares, maps each to the file it preloads, and binds residual
// `unresolved::*.method` calls in THAT file whose receiver_type names one
// of its own aliases. A name the loaded script does not declare is left
// alone rather than bound to the file.
func ResolveGDScriptPreloadAliases(g graph.Store) int {
	if g == nil {
		return 0
	}
	// aliases[fileID][aliasName] = target script file ID.
	aliases := gdPreloadAliases(g)
	if len(aliases) == 0 {
		return 0
	}
	targets := make(map[string]bool)
	for _, byName := range aliases {
		for _, fileID := range byName {
			targets[fileID] = true
		}
	}
	members := gdMembersByFileSet(g, targets)

	resolved := 0
	var reindex []graph.EdgeReindex
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		recv, member := gdScriptCallKey(e)
		if recv == "" {
			continue
		}
		byName := aliases[gdEdgeSourceFile(e)]
		if byName == nil {
			continue
		}
		target := members[byName[recv]][member]
		if target == "" {
			continue
		}
		reindex = append(reindex, gdPreloadAliasRebind(e, target))
		resolved++
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	return resolved
}

func gdPreloadAliasRebind(e *graph.Edge, target string) graph.EdgeReindex {
	oldTo := e.To
	e.To = target
	// The alias states its own path in the source line that declares
	// it, and the loaded script declares the member. Same grade of
	// evidence as the autoload binding this mirrors.
	e.Origin = graph.OriginASTResolved
	e.Confidence = 0.9
	e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, 0.9)
	StampSynthesized(e, SynthGodotPreloadAlias)
	return graph.EdgeReindex{Edge: e, OldTo: oldTo}
}

// gdEdgeSourceFile returns the graph file ID an edge is sourced from.
// A call attributed to the file node IS the file ID; one attributed to a
// symbol carries it ahead of the `::` separator.
func gdEdgeSourceFile(e *graph.Edge) string {
	if i := strings.Index(e.From, "::"); i >= 0 {
		return e.From[:i]
	}
	return e.From
}

// gdPreloadAliases maps, per GDScript file, each preload-alias name to
// the graph file ID of the script it loads. An alias whose path is not
// indexed, or is ambiguous across two indexed scripts, is dropped.
func gdPreloadAliases(g graph.Store) map[string]map[string]string {
	// decls[fileID][alias] = declared res:// path (project-relative).
	var decls map[string]map[string]string
	for n := range g.NodesByKind(graph.KindVariable) {
		fileID, path := gdPreloadAliasDecl(n)
		if path == "" {
			continue
		}
		if decls == nil {
			decls = map[string]map[string]string{}
		}
		if decls[fileID] == nil {
			decls[fileID] = map[string]string{}
		}
		decls[fileID][n.Name] = path
	}
	if len(decls) == 0 {
		return nil
	}

	// One pass over the script file nodes serves every alias — a scan per
	// alias would multiply a full-store walk by the project's alias count.
	byPath := make(map[string]string)
	ambiguous := make(map[string]bool)
	wanted := make(map[string]bool)
	for _, byName := range decls {
		for _, path := range byName {
			wanted[path] = true
		}
	}
	for f := range g.NodesByKind(graph.KindFile) {
		if !isGDScriptFile(f) {
			continue
		}
		for path := range wanted {
			if ambiguous[path] || !gdPathMatches(f.FilePath, path) {
				continue
			}
			if prev, seen := byPath[path]; seen && prev != f.ID {
				delete(byPath, path)
				ambiguous[path] = true
				continue
			}
			byPath[path] = f.ID
		}
	}

	out := make(map[string]map[string]string, len(decls))
	for fileID, byName := range decls {
		for alias, path := range byName {
			target := byPath[path]
			if target == "" || target == fileID {
				continue
			}
			if out[fileID] == nil {
				out[fileID] = map[string]string{}
			}
			out[fileID][alias] = target
		}
	}
	return out
}

func gdPreloadAliasDeclCandidate(n *graph.Node) bool {
	return n != nil && n.Kind == graph.KindVariable && n.Language == "gdscript"
}

func gdPreloadAliasDecl(n *graph.Node) (fileID, path string) {
	if !gdPreloadAliasDeclCandidate(n) {
		return "", ""
	}
	path, _ = n.Meta["gd_preload"].(string)
	path = gdProjectRelPath(path)
	if path == "" || !strings.HasSuffix(path, ".gd") {
		return "", ""
	}
	fileID, _, ok := strings.Cut(n.ID, "::")
	if !ok {
		return "", ""
	}
	return fileID, path
}

// gdProjectRelPath strips the `res://` scheme and any leading separator,
// leaving the project-relative path the file index is matched against.
func gdProjectRelPath(p string) string {
	if !strings.HasPrefix(p, godotResScheme) {
		return ""
	}
	return strings.Trim(strings.TrimPrefix(p, godotResScheme), "/")
}

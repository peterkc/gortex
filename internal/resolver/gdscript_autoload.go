package resolver

import (
	"iter"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Godot autoload resolution. An `[autoload]` entry in `project.godot`
// binds a project-global identifier to a script:
//
//	Game="*res://src/core/Game.gd"
//
// From then on `Game.set_speed()` is legal in every script in the
// project — with no import, no preload, and no `class_name` on the
// target. The GDScript extractor stamps `Game` as the call's
// receiver_type, but the generic receiver passes can only match that
// against a method whose own `receiver` says `Game`, and an autoload
// script typically declares no `class_name` at all. So the name and the
// definition never meet, and every cross-script autoload call stays
// unresolved.
//
// This pass supplies the missing link: it reads the autoload nodes the
// project.godot extractor emitted, maps each singleton name to its
// script, and binds residual `unresolved::*.method` calls whose
// receiver_type names an autoload to that script's member of the same
// name. Already-resolved edges are never touched, and a name the script
// does not define is left alone rather than bound to the file.
func ResolveGDScriptAutoloads(g graph.Store) int {
	if g == nil {
		return 0
	}
	scripts := gdAutoloadScripts(g)
	if len(scripts) == 0 {
		return 0
	}
	want := make(map[string]bool, len(scripts))
	for _, fileID := range scripts {
		want[fileID] = true
	}
	members := gdMembersByFileSet(g, want)
	resolved := 0
	var reindex []graph.EdgeReindex
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		recv, method := gdScriptCallKey(e)
		if recv == "" {
			continue
		}
		fileID, ok := scripts[recv]
		if !ok {
			continue
		}
		target := members[fileID][method]
		if target == "" {
			continue
		}
		reindex = append(reindex, gdAutoloadRebind(e, target))
		resolved++
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	return resolved
}

func gdScriptCallKey(e *graph.Edge) (receiver, member string) {
	if e == nil || !graph.IsUnresolvedTarget(e.To) {
		return "", ""
	}
	receiver, _ = e.Meta["receiver_type"].(string)
	if receiver == "" {
		return "", ""
	}
	return receiver, strings.TrimPrefix(graph.UnresolvedName(e.To), "*.")
}

func gdAutoloadRebind(e *graph.Edge, target string) graph.EdgeReindex {
	oldTo := e.To
	e.To = target
	// The manifest names the script and the script declares the member.
	// That is the same evidence as the extractor's receiver_type match.
	e.Origin = graph.OriginASTResolved
	e.Confidence = 0.9
	e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, 0.9)
	StampSynthesized(e, SynthGodotAutoload)
	return graph.EdgeReindex{Edge: e, OldTo: oldTo}
}

func gdAutoloadDeclCandidate(n *graph.Node) bool {
	return n != nil && n.Kind == graph.KindType && n.Language == "godot_project"
}

func gdAutoloadDecl(n *graph.Node) (name, script string) {
	if !gdAutoloadDeclCandidate(n) {
		return "", ""
	}
	if kind, _ := n.Meta["gd_kind"].(string); kind != "autoload" {
		return "", ""
	}
	script, _ = n.Meta["gd_script"].(string)
	return n.Name, script
}

// gdAutoloadScripts maps each autoload singleton name to the graph file
// ID of the script it names. An entry whose script is not indexed, and
// a name declared by two different manifests with conflicting targets,
// are both dropped — a wrong binding here is worse than none.
func gdAutoloadScripts(g gdKindNodes) map[string]string {
	var decls map[string]string
	for n := range g.NodesByKind(graph.KindType) {
		name, script := gdAutoloadDecl(n)
		if script == "" {
			continue
		}
		if decls == nil {
			decls = map[string]string{}
		}
		gdRecordAutoload(decls, name, script)
	}
	if len(decls) == 0 {
		return nil
	}
	return gdResolveAutoloadScripts(decls, g.NodesByKind(graph.KindFile))
}

func gdRecordAutoload(decls map[string]string, name, script string) {
	if prev, dup := decls[name]; dup && prev != script {
		decls[name] = ""
		return
	}
	decls[name] = script
}

func gdResolveAutoloadScripts(decls map[string]string, files iter.Seq[*graph.Node]) map[string]string {
	// `res://` paths are project-root relative; graph file IDs carry the
	// repo prefix and the repo-relative path. Match by path suffix on a
	// separator boundary, and drop any singleton whose path is ambiguous
	// across two indexed scripts. One pass over the file nodes serves
	// every autoload — NodesByKind is a full scan on a disk backend, and
	// a scan per singleton would multiply that by the manifest's length.
	out := make(map[string]string, len(decls))
	ambiguous := make(map[string]bool, len(decls))
	for f := range files {
		if !isGDScriptFile(f) {
			continue
		}
		for name, script := range decls {
			if script == "" || ambiguous[name] || !gdPathMatches(f.FilePath, script) {
				continue
			}
			if prev, seen := out[name]; seen && prev != f.ID {
				delete(out, name)
				ambiguous[name] = true
				continue
			}
			out[name] = f.ID
		}
	}
	return out
}

// gdPathMatches reports whether an indexed file path ends with the
// project-relative script path on a path-separator boundary, so
// `src/core/Game.gd` matches `<repo>/src/core/Game.gd` but never
// `src/core/NotGame.gd`.
func gdPathMatches(filePath, script string) bool {
	p := strings.ReplaceAll(filePath, "\\", "/")
	if p == script {
		return true
	}
	return strings.HasSuffix(p, "/"+script)
}

func isGDScriptFile(n *graph.Node) bool {
	return n != nil && n.Kind == graph.KindFile && n.Language == "gdscript"
}

var gdScriptMemberKinds = [...]graph.NodeKind{graph.KindMethod, graph.KindFunction}

// gdMembersByFileSet indexes the callable members declared by each of the
// named script files, by name. Signals are included: `Game.started.emit()`
// and a direct connect both reference the signal as a member.
func gdMembersByFileSet(g graph.Store, want map[string]bool) map[string]map[string]string {
	if len(want) == 0 {
		return nil
	}
	out := make(map[string]map[string]string, len(want))
	for _, kind := range gdScriptMemberKinds {
		for n := range g.NodesByKind(kind) {
			fileID, ok := gdScriptMemberFile(n)
			if !ok || !want[fileID] {
				continue
			}
			if out[fileID] == nil {
				out[fileID] = map[string]string{}
			}
			if _, exists := out[fileID][n.Name]; !exists {
				out[fileID][n.Name] = n.ID
			}
		}
	}
	return out
}

func gdScriptMemberFile(n *graph.Node) (string, bool) {
	if n == nil || n.Language != "gdscript" {
		return "", false
	}
	fileID, _, ok := strings.Cut(n.ID, "::")
	return fileID, ok
}

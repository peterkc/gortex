package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Godot scene-connection resolution. A `.tscn` wires a signal to a
// handler in a block that names the method as a bare string:
//
//	[connection signal="pressed" from="Button" to="." method="_on_random"]
//
// The method belongs to the script attached to the `to` node — a
// relationship spread across three places (the connection block, the
// node's `script = ExtResource(…)` assignment, and the `[ext_resource]`
// header that gives the path) and recorded in none of the scripts. Left
// unresolved, a rename rewrites the method and leaves the scene calling a
// name that no longer exists; the project still loads, nothing errors,
// and the button silently stops working.
//
// This pass joins the three: for each connection edge, it follows the
// owning scene node's script reference to the `.gd` file and binds the
// named member. A node with no script, a script that is not indexed, and
// a method the script does not declare are each left alone rather than
// guessed at.
//
// It is authoritative, not a fallback. The generic reference resolver
// reaches these placeholders first and binds them by name at a weak
// tier — which happens to be right when the handler name is unique in
// the project and silently wrong when it is not, and `_on_pressed` is
// never unique. The scene states which script owns the node, so this
// pass overrides that guess wherever it can compute the answer.
func ResolveGodotSceneConnections(g graph.Store) int {
	if g == nil {
		return 0
	}
	pending := gdConnectionEdges(g)
	if len(pending) == 0 {
		return 0
	}

	// The scene node that owns the connection carries the script binding as
	// an outgoing reference; a scene whose root holds the script covers the
	// common `to="."` case directly.
	visitRefs := func(id string, yield func(*graph.Edge) (bool, error)) error {
		for _, e := range g.GetOutEdges(id) {
			more, err := yield(e)
			if err != nil || !more {
				return err
			}
		}
		return nil
	}
	readNode := func(id string) (*graph.Node, error) { return g.GetNode(id), nil }
	scripts := make(map[string]string, len(pending))
	scriptFiles := make(map[string]bool)
	for _, e := range pending {
		if _, done := scripts[e.From]; done {
			continue
		}
		fileID, _ := gdScriptOfSceneNode(e.From, visitRefs, readNode)
		scripts[e.From] = fileID
		if fileID != "" {
			scriptFiles[fileID] = true
		}
	}
	members := gdMembersByFileSet(g, scriptFiles)

	resolved := 0
	var reindex []graph.EdgeReindex
	for _, e := range pending {
		fileID := scripts[e.From]
		if fileID == "" {
			continue
		}
		method, ok := e.Meta["godot_method"].(string)
		if !ok || method == "" {
			continue
		}
		target := members[fileID][method]
		if target == "" {
			continue
		}
		oldTo := e.To
		e.To = target
		// Every hop is written out in the scene file and matched exactly.
		e.Origin = graph.OriginASTResolved
		e.Confidence = 0.9
		e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeReferences, 0.9)
		StampSynthesized(e, SynthGodotConnection)
		if oldTo == target {
			// Already on the right node by a weaker route; only the tier
			// and provenance change, so the identity has to be refreshed
			// in place rather than re-keyed.
			reindex = append(reindex, graph.EdgeReindex{
				Edge: e, OldFrom: e.From, OldTo: oldTo,
				OldFilePath: e.FilePath, OldLine: e.Line, RefreshIdentity: true,
			})
			continue
		}
		reindex = append(reindex, graph.EdgeReindex{Edge: e, OldTo: oldTo})
		resolved++
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	return resolved
}

// gdConnectionEdges collects the `[connection]` edges the scene extractor
// emitted, whether or not a weaker pass has already bound them.
func gdConnectionEdges(g graph.Store) []*graph.Edge {
	var out []*graph.Edge
	for e := range g.EdgesByKind(graph.EdgeReferences) {
		if e == nil || e.Meta == nil {
			continue
		}
		if conn, _ := e.Meta["godot_connection"].(bool); conn {
			out = append(out, e)
		}
	}
	return out
}

// gdScriptOfSceneNode returns the graph file ID of the GDScript attached
// to a scene node, or to the scene's own file when the node itself
// carries none — Godot resolves a connection's target against the scene
// it is declared in, whose root script is the usual holder.
func gdScriptOfSceneNode(nodeID string, visitRefs func(string, func(*graph.Edge) (bool, error)) error,
	readNode func(string) (*graph.Node, error),
) (string, error) {
	if id, err := gdScriptRefTarget(nodeID, visitRefs, readNode); err != nil || id != "" {
		return id, err
	}
	sceneFile, _, ok := strings.Cut(nodeID, "::")
	if !ok {
		return "", nil
	}
	return gdScriptRefTarget(sceneFile, visitRefs, readNode)
}

// gdScriptRefTarget returns the single GDScript file a node references,
// or "" when it references none or several — two scripts on one node is
// malformed, and guessing between them is worse than declining.
func gdScriptRefTarget(id string, visitRefs func(string, func(*graph.Edge) (bool, error)) error,
	readNode func(string) (*graph.Node, error),
) (string, error) {
	found := ""
	ambiguous := false
	err := visitRefs(id, func(e *graph.Edge) (bool, error) {
		if ambiguous {
			return false, nil
		}
		if e == nil || e.Kind != graph.EdgeReferences || graph.IsUnresolvedTarget(e.To) {
			return true, nil
		}
		n, err := readNode(e.To)
		if err != nil {
			return false, err
		}
		if n == nil || n.Kind != graph.KindFile || n.Language != "gdscript" {
			return true, nil
		}
		if found != "" && found != n.ID {
			ambiguous = true
			return false, nil
		}
		found = n.ID
		return true, nil
	})
	if err != nil || ambiguous {
		return "", err
	}
	return found, nil
}

package resolver

import (
	"iter"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Receiver-type gating for GDScript member-call attribution.
//
// A GDScript call states its receiver — `Items.apply(...)` — and the
// extractor stamps that name on the edge. When no member of that receiver
// can be found, a weak resolver tier still answers with whatever
// same-named method sits nearest the caller, and the receiver evidence is
// silently discarded. With `apply` declared in several unrelated classes,
// the caller list of any one of them then contains calls that belong to
// the others:
//
//	# reported as callers of Carry.apply, but the source lines read:
//	Items.apply(state, av, item_id)
//	Effects.apply(world, state, reward)
//
// For blast radius this is the dangerous direction: the count looks
// reassuring while pointing at the wrong code. This pass demotes such
// edges to the speculative tier so they drop out of every default query
// and min_tier filter, mirroring the C# receiver gate.
//
// A demotion is only taken when the mismatch is PROVABLE. Inherited
// dispatch (`Child.method()` landing on `Parent.method`) is a legitimate
// bind and is preserved by walking the receiver's `extends` chain; a
// chain the extractor could not read as one in-repo class name is opaque
// and declines to judge. Autoload and preload-alias receivers are checked
// against the script they actually name.
func DemoteGDScriptReceiverMismatches(g graph.Store) int {
	if g == nil {
		return 0
	}
	classes := gdClassIndex(g)
	if len(classes) == 0 {
		return 0
	}
	autoloads := gdAutoloadScripts(g)
	aliases := gdPreloadAliases(g)

	var reindex []graph.EdgeReindex
	for _, kind := range []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences} {
		for e := range g.EdgesByKind(kind) {
			if !gdReceiverMismatch(g, e, classes, autoloads, aliases) {
				continue
			}
			gdDemoteReceiverMismatch(e)
			reindex = append(reindex, graph.EdgeReindex{
				Edge: e, OldFrom: e.From, OldTo: e.To,
				OldFilePath: e.FilePath, OldLine: e.Line, RefreshIdentity: true,
			})
		}
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	return len(reindex)
}

func gdDemoteReceiverMismatch(e *graph.Edge) {
	e.Origin = graph.OriginSpeculative
	if e.Meta == nil {
		e.Meta = map[string]any{}
	}
	e.Meta[graph.MetaSpeculative] = true
	e.Meta["demoted"] = "receiver_type_mismatch"
}

type gdKindNodes interface {
	NodesByKind(graph.NodeKind) iter.Seq[*graph.Node]
}

type gdNodeLookup interface {
	GetNode(string) *graph.Node
}

// gdClass is one GDScript class the gate can reason about: the file that
// declares it, its declared base, and whether that base is readable.
type gdClass struct {
	fileID string
	parent string
	// opaque marks a class whose base the extractor could not reduce to a
	// single in-repo class name — an `extends "res://…"`, a dotted inner
	// class, or an inner class whose own base was never parsed. Its
	// ancestry is unknown, so no mismatch against it is provable.
	opaque bool
}

// gdClassIndex maps each GDScript class name to its declaration. A name
// declared by two different files is ambiguous — the gate cannot tell
// which one a receiver means, so it is marked opaque rather than dropped,
// which keeps it from being read as "no such class exists".
func gdClassIndex(g gdKindNodes) map[string]*gdClass {
	out := map[string]*gdClass{}
	for n := range g.NodesByKind(graph.KindType) {
		if n == nil || n.Language != "gdscript" || n.Meta == nil {
			continue
		}
		switch kind, _ := n.Meta["gd_kind"].(string); kind {
		case "class_name", "inner_class":
		default:
			continue
		}
		fileID, _, _ := strings.Cut(n.ID, "::")
		parent, _ := n.Meta["gd_extends"].(string)
		opaque, _ := n.Meta["gd_extends_opaque"].(bool)
		if prev, dup := out[n.Name]; dup {
			if prev.fileID != fileID {
				prev.opaque = true
			}
			continue
		}
		out[n.Name] = &gdClass{fileID: fileID, parent: parent, opaque: opaque}
	}
	return out
}

// gdReceiverMismatch reports whether a resolved GDScript edge binds to a
// member that its stated receiver provably does not own.
func gdReceiverMismatch(
	g gdNodeLookup,
	e *graph.Edge,
	classes map[string]*gdClass,
	autoloads map[string]string,
	aliases map[string]map[string]string,
) bool {
	if e == nil || e.Meta == nil || e.To == "" || graph.IsUnresolvedTarget(e.To) {
		return false
	}
	if _, already := e.Meta["demoted"]; already {
		return false
	}
	recv, _ := e.Meta["receiver_type"].(string)
	if recv == "" {
		return false
	}
	target := g.GetNode(e.To)
	if target == nil || target.Language != "gdscript" {
		return false
	}
	targetFile, _, _ := strings.Cut(target.ID, "::")
	// A binding inside the caller's own file needs no receiver evidence:
	// the definition is right there, and `self`-shaped receivers on a
	// script with no `class_name` legitimately land on it.
	if targetFile == gdEdgeSourceFile(e) {
		return false
	}

	// The receiver names a script directly — an autoload singleton or a
	// file-local preload alias. Whether that is the script the edge landed
	// on is a fact, not a guess, either way.
	if fileID, ok := autoloads[recv]; ok && fileID != "" {
		return fileID != targetFile
	}
	if fileID := aliases[gdEdgeSourceFile(e)][recv]; fileID != "" {
		return fileID != targetFile
	}

	owner := ""
	if target.Meta != nil {
		owner, _ = target.Meta["receiver"].(string)
	}
	if owner == recv {
		return false
	}

	recvClass, known := classes[recv]
	if known && recvClass.opaque {
		return false
	}
	if !known {
		// No in-repo class, autoload or alias carries this name — it is an
		// engine type (`Input`, `OS`) or a global this index never saw. No
		// in-repo method can be its member, so any bind here is a phantom.
		// The one exception is a receiver the caller's own file declares as
		// a plain variable: that is a local alias whose target the graph
		// does not track, and a mismatch is not provable.
		return !gdReceiverLocallyDeclared(g, e, recv)
	}
	if owner == "" {
		// A plain function in a script with no `class_name`, reached from
		// another file by a receiver naming a class. The autoload and alias
		// routes above are the only legitimate ways to name such a script,
		// and neither claimed this edge.
		return true
	}
	// Inherited dispatch: walk the receiver's declared base chain. Any
	// unreadable link makes the ancestry unknown and the verdict unsafe.
	for seen, cur := map[string]bool{recv: true}, recvClass; cur != nil; {
		if cur.opaque {
			return false
		}
		if cur.parent == "" {
			return true
		}
		if cur.parent == owner {
			return false
		}
		next, ok := classes[cur.parent]
		if !ok {
			// The base is an engine class (`Node`, `Resource`); the chain
			// leaves the repo, so no in-repo owner can be above it.
			return true
		}
		if seen[cur.parent] {
			return false // a cycle in declared bases proves nothing
		}
		seen[cur.parent] = true
		cur = next
	}
	return true
}

// gdReceiverLocallyDeclared reports whether the calling file declares the
// receiver name itself — a `var`/`const` holding a value whose type the
// graph does not track. Such a receiver is a local alias, not a claim
// about a class, so no mismatch can be proven from it.
func gdReceiverLocallyDeclared(g gdNodeLookup, e *graph.Edge, recv string) bool {
	n := g.GetNode(gdEdgeSourceFile(e) + "::" + recv)
	return n != nil && n.Language == "gdscript" && n.Kind == graph.KindVariable
}

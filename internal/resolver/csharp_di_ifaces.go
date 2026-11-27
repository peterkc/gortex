package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// ResolveCSharpDIImplementedInterfaces expands Autofac
// AsImplementedInterfaces() registrations into concrete interface→impl
// bindings. The extractor cannot see the interface set in the call text
// (`RegisterType<Foo>().AsImplementedInterfaces()` binds Foo to whatever
// Foo implements), so it emits a marker EdgeProvides (binding
// "asImplementedInterfaces", no provides_for) that the provides-index
// ignores. This pass joins each marker with the impl's resolved
// EdgeImplements set — the same information Autofac reflects over at
// runtime — and emits one useClass binding per implemented interface,
// the shape buildProvidesForIndex consumes.
//
// Runs with the framework synthesizers, after the implements-producing
// passes, so the join sees the settled hierarchy (including implements
// edges the partial-class merge folded onto the canonical fragment).
func ResolveCSharpDIImplementedInterfaces(g graph.Store) int {
	return ResolveCSharpDIImplementedInterfacesScoped(g, nil)
}

// ResolveCSharpDIImplementedInterfacesScoped limits partial work to
// markers whose registration file lives in a changed repository. A nil
// scope preserves the full/cold whole-graph behavior.
func ResolveCSharpDIImplementedInterfacesScoped(g graph.Store, scope map[string]bool) int {
	if g == nil {
		return 0
	}

	// One provides scan yields both the markers to expand and the dedup
	// set that makes a re-run (or an overlapping explicit .As<>) a no-op.
	var markers []*graph.Edge
	existing := map[string]struct{}{}
	for _, e := range frameworkRepoEdges(g, scope, graph.EdgeProvides) {
		if e.Meta == nil {
			continue
		}
		switch b, _ := e.Meta[graph.MetaDIBinding].(string); b {
		case graph.DIBindingAsImplemented:
			markers = append(markers, e)
		case graph.DIBindingUseClass:
			pf, _ := e.Meta[graph.MetaDIProvidesFor].(string)
			existing[csharpDIBindingKey(e.From, pf, providesTargetName(e.To))] = struct{}{}
		}
	}
	if len(markers) == 0 {
		return 0
	}

	added := 0
	for _, m := range markers {
		implName := providesTargetName(m.To)
		if implName == "" {
			continue
		}
		// The marker's To stays an unresolved:: stub (the provides-index
		// never binds it), so locate the impl by an exact, same-repo,
		// unique csharp type lookup — ambiguity means skip, never guess.
		// Merged-away partial fragments are excluded so the join reads
		// the canonical node's full implements set.
		repoPrefix := ""
		if fileNode := g.GetNode(m.From); fileNode != nil {
			repoPrefix = fileNode.RepoPrefix
		}
		var impl *graph.Node
		for _, cand := range g.FindNodesByNameInRepo(implName, repoPrefix) {
			if !csharpDIImplementation(cand) {
				continue
			}
			if impl != nil {
				impl = nil // ambiguous
				break
			}
			impl = cand
		}
		if impl == nil {
			continue
		}

		for _, e := range g.GetOutEdges(impl.ID) {
			if !csharpDIDeclaredImplements(e) {
				continue
			}
			// The interface must land on an in-repo node: it supplies the
			// synthesized edge's distinct provenance site (the store keys
			// edges on (From,To,Kind,FilePath,Line), and every binding of
			// this marker shares the first three), and it filters the
			// framework interfaces (IDisposable, …) Autofac technically
			// registers but no find_implementations caller wants.
			iface := csharpDIResolveIface(g, e.To, impl.RepoPrefix)
			if iface == nil {
				continue
			}
			key := csharpDIBindingKey(m.From, iface.Name, implName)
			if _, dup := existing[key]; dup {
				continue
			}
			existing[key] = struct{}{}
			g.AddEdge(csharpDIProvideEdge(m.From, implName, iface))
			added++
		}
	}
	return added
}

func csharpDIBindingKey(from, iface, impl string) string {
	return from + "\x00" + iface + "\x00" + impl
}

func csharpDIImplementation(n *graph.Node) bool {
	return n != nil && n.Language == "csharp" && n.Kind == graph.KindType && n.Meta[metaPartialMergedInto] == nil
}

func csharpDIInterface(n *graph.Node) bool {
	return n != nil && n.Language == "csharp" && n.Kind == graph.KindInterface
}

// Same-shaped types do not constitute source-declared DI registrations.
func csharpDIDeclaredImplements(e *graph.Edge) bool {
	return e.Kind == graph.EdgeImplements && e.Meta["via"] != MetaViaMethodSetInference
}

func csharpDIProvideEdge(from, impl string, iface *graph.Node) *graph.Edge {
	return &graph.Edge{
		From:     from,
		To:       graph.UnresolvedMarker + impl,
		Kind:     graph.EdgeProvides,
		FilePath: iface.FilePath,
		Line:     iface.StartLine,
		Origin:   graph.OriginASTInferred,
		Meta: map[string]any{
			graph.MetaDIBinding:     graph.DIBindingUseClass,
			graph.MetaDIProvidesFor: iface.Name,
			graph.MetaDISource:      "autofac",
			graph.MetaDIImplFQCN:    impl,
			MetaSynthesizedBy:       SynthCSharpDIIfaces,
		},
	}
}

// csharpDIResolveIface lands an implements-edge target on its in-repo
// interface node: directly when the resolver has already bound it, else
// by an exact, same-repo, unique name lookup (the base-list may still
// carry `unresolved::` stubs when the synthesizers run). Nil when the
// interface is not indexed in this repo — external framework interfaces
// are deliberately not bound.
func csharpDIResolveIface(g graph.Store, to, repoPrefix string) *graph.Node {
	if !graph.IsUnresolvedTarget(to) {
		n := g.GetNode(to)
		if csharpDIInterface(n) {
			return n
		}
		return nil
	}
	name := graph.UnresolvedName(to)
	if name == "" {
		return nil
	}
	var found *graph.Node
	for _, cand := range g.FindNodesByNameInRepo(name, repoPrefix) {
		if !csharpDIInterface(cand) {
			continue
		}
		if found != nil {
			return nil // ambiguous — skip, never guess
		}
		found = cand
	}
	return found
}

// providesTargetName reduces a provides/implements edge target to the
// bare type name the provides-index groups by: `unresolved::Foo` → Foo,
// `path/File.cs::Foo` → Foo, `Foo` → Foo.
func providesTargetName(to string) string {
	if graph.IsUnresolvedTarget(to) {
		return graph.UnresolvedName(to)
	}
	if cut := strings.LastIndex(to, "::"); cut >= 0 {
		return to[cut+2:]
	}
	return to
}

package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// SwiftUI directory-convention name-resolution. sourcekit-lsp resolves most
// Swift references, but its coverage is partial; this pass is the fallback that
// binds residual unresolved references to the SwiftUI definition their name +
// directory convention implies: a `*ViewModel` to /ViewModels/, a `*View` to
// /Views/, a `*Store`/`*Manager` to /Stores/ or /Managers/, and a bare
// PascalCase model to /Models/. Only references from Swift files are touched,
// and the bare-model case binds only when the definition actually lives under
// a /Models/ directory (so a SwiftUI built-in like Text/Button is never
// mis-bound to an unrelated same-named type).

var (
	swiftUIViewDirs      = []string{"/Views/", "/View/"}
	swiftUIViewModelDirs = []string{"/ViewModels/", "/ViewModel/"}
	swiftUIStoreDirs     = []string{"/Stores/", "/Store/", "/Managers/", "/Manager/"}
	swiftUIModelDirs     = []string{"/Models/", "/Model/"}
)

// ResolveSwiftUIRefs binds residual unresolved SwiftUI references to their
// directory-located definitions. Returns the count bound.
func ResolveSwiftUIRefs(g graph.Store) int {
	if g == nil {
		return 0
	}
	resolved := 0
	var reindex []graph.EdgeReindex
	for _, kind := range []graph.EdgeKind{graph.EdgeInstantiates, graph.EdgeReferences, graph.EdgeTypedAs, graph.EdgeCalls} {
		for e := range g.EdgesByKind(kind) {
			name, dirs, modelOnly, ok := swiftUIRefShape(e)
			if !ok {
				continue
			}
			fromFile := ""
			if n := g.GetNode(e.From); n != nil {
				fromFile = n.FilePath
			}
			if !swiftUIRefSource(fromFile) {
				continue
			}
			targetID, conf := ResolveByConvention(g, name, "", dirs, fromFile)
			if targetID == "" {
				continue
			}
			if modelOnly {
				tn := g.GetNode(targetID)
				if tn == nil || !swiftUIModelPath(tn.FilePath) {
					continue
				}
			}
			reindex = append(reindex, swiftUIRefRebind(e, targetID, conf))
			resolved++
		}
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	return resolved
}

func swiftUIRefShape(e *graph.Edge) (name string, dirs []string, modelOnly, ok bool) {
	if e == nil || !graph.IsUnresolvedTarget(e.To) {
		return "", nil, false, false
	}
	name = graph.UnresolvedName(e.To)
	if name == "" || strings.ContainsRune(name, '.') {
		return "", nil, false, false
	}
	dirs, modelOnly, ok = swiftUIDirsFor(name)
	return name, dirs, modelOnly, ok
}

func swiftUIRefSource(path string) bool {
	return strings.HasSuffix(path, ".swift")
}

func swiftUIModelPath(path string) bool {
	return swiftUIPathHasDir(path, swiftUIModelDirs)
}

func swiftUIRefRebind(e *graph.Edge, target string, conf float64) graph.EdgeReindex {
	oldTo := e.To
	e.To = target
	e.Origin = graph.OriginASTInferred
	e.Confidence = conf
	e.ConfidenceLabel = graph.ConfidenceLabelFor(e.Kind, conf)
	StampSynthesized(e, SynthSwiftUIResolve)
	return graph.EdgeReindex{Edge: e, OldTo: oldTo}
}

// swiftUIDirsFor classifies a reference name into its convention directories;
// modelOnly marks the bare-PascalCase model fallback, which is additionally
// gated on the resolved definition living under a /Models/ directory.
func swiftUIDirsFor(name string) (dirs []string, modelOnly bool, ok bool) {
	switch {
	case strings.HasSuffix(name, "ViewModel"):
		return swiftUIViewModelDirs, false, true
	case strings.HasSuffix(name, "View"):
		return swiftUIViewDirs, false, true
	case strings.HasSuffix(name, "Store"), strings.HasSuffix(name, "Manager"):
		return swiftUIStoreDirs, false, true
	}
	if c := name[0]; c >= 'A' && c <= 'Z' {
		return swiftUIModelDirs, true, true
	}
	return nil, false, false
}

// swiftUIPathHasDir reports whether path contains any of the directory segments.
func swiftUIPathHasDir(path string, dirs []string) bool {
	for _, d := range dirs {
		if strings.Contains(path, d) {
			return true
		}
	}
	return false
}

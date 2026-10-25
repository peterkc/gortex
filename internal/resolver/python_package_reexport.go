package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// PythonReExportFileMeta retains the package whose binding selected the call.
const PythonReExportFileMeta = "python_reexport_file"

// resolvePythonPackageReExport follows one exact relative from-import in a
// linked package __init__.py. A directory import alone is not symbol evidence.
func (cr *CrossRepoResolver) resolvePythonPackageReExport(e *graph.Edge, module, symbol string) bool {
	callerWS := cr.callerWorkspaceID(e)
	var match *graph.Node
	packageFile := ""
	for _, imp := range cr.graph.GetOutEdges(cr.callerFileID(e)) {
		if imp.Kind != graph.EdgeImports || !imp.CrossRepo || !strings.HasSuffix(imp.To, "/__init__.py") {
			continue
		}
		stub, _ := imp.Meta[PythonImportStubMeta].(string)
		spec, _ := imp.Meta["python_import"].(string)
		if stub == "" {
			continue
		}
		pkg := cr.cachedGetNode(imp.To)
		if pkg == nil || !cr.crossWorkspaceEligible(callerWS, candidateWorkspaceID(pkg), spec) {
			continue
		}
		name := symbol
		if module != spec {
			name = pythonImportedName(module)
			if name == "" || module != spec+"."+name {
				continue
			}
		}
		if !pythonModuleFileMatches(pkg.FilePath, spec) {
			continue
		}
		for _, reexport := range cr.graph.GetOutEdges(pkg.ID) {
			if reexport.Kind != graph.EdgeImports {
				continue
			}
			exports, _ := reexport.Meta["python_reexports"].(map[string]any)
			name, _ := exports[name].(string)
			if name == "" {
				continue
			}
			// The extractor's stem is repo-relative even when the per-repo pass
			// has parked the relative import before multi-repo prefixing.
			stem, _ := reexport.Meta["python_reexport_stem"].(string)
			if stem == "" {
				return false
			}
			found := false
			for _, suffix := range []string{".py", "/__init__.py"} {
				candidate := cr.cachedGetNode(pkg.RepoPrefix + "/" + stem + suffix + "::" + name)
				if candidate == nil {
					continue
				}
				if candidate.RepoPrefix != pkg.RepoPrefix ||
					(candidate.Kind != graph.KindFunction && candidate.Kind != graph.KindType) ||
					!cr.crossWorkspaceEligible(callerWS, candidateWorkspaceID(candidate), spec) {
					return false
				}
				if match != nil && match.ID != candidate.ID {
					return false
				}
				match, found = candidate, true
				packageFile = pkg.ID
			}
			if !found {
				return false
			}
		}
	}
	if match == nil {
		return false
	}
	e.To, e.CrossRepo = match.ID, true
	e.Meta[PythonReExportFileMeta] = packageFile
	return true
}

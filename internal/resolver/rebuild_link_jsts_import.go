package resolver

import (
	"path"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// The checked callbacks return an empty string only for a proved native miss.
// pathAlias receives the module alone and returns a repo-prefixed graph stem.
// rewriteNpm receives and returns the raw import payload.
type rebuildJSTSImportInputs struct {
	pathAlias     func(caller, module string) (string, error)
	rewriteNpm    func(caller, raw string) (string, error)
	heldPoint     func(id string, file bool) (string, error)
	markIsolation func(caller, spec, targetFile, deferred, cause string) error
}

// RebuildJSTSSelectedFile finds the native point-tier file for a selected
// alias stem or relative npm rewrite. It reads the same live graph points.
func RebuildJSTSSelectedFile(caller, selected, cause string, node func(string) (*graph.Node, error)) (string, error) {
	stem := selected
	if cause == "npm-candidate" {
		module, _ := splitImportSpecSymbol(selected)
		stem = expandJSTSSpecifier(nil, caller, module)
	}
	var inputErr error
	var file string
	probeJSTSFile(func(id string) *graph.Node {
		if inputErr != nil {
			return nil
		}
		var n *graph.Node
		n, inputErr = node(id)
		if n != nil && n.Kind == graph.KindFile {
			file = n.FilePath
		}
		return n
	}, stem)
	return file, inputErr
}

// resolveRebuildJSTSImport reads one selected TS/JS import site without editing
// it. The source and all point probes must see the same private node view.
// With no target, the next tier gets the effective spec only on nil error.
func resolveRebuildJSTSImport(edge *graph.Edge, source *graph.Node, spec string, node func(string) (*graph.Node, error), in rebuildJSTSImportInputs) (*graph.Node, string, error) {
	caller := jsTSImportCallerFile(edge)
	if spec == "" || !isJSTSPath(caller) {
		return nil, spec, nil
	}
	if source == nil || source.Language == "" {
		return nil, "", rebuildImportNeeds(edge, spec, "indexed TS/JS caller language")
	}
	switch source.Language {
	case "go":
		// Native can enter the point tier by path but gate the target by
		// the source node's Go language. This slice cannot apply that gate.
		return nil, "", rebuildImportNeeds(edge, spec, "Go package ownership for TS/JS point probes")
	case "javascript", "typescript":
	default:
		return nil, spec, nil
	}
	switch jsTSExt(caller) {
	case ".vue", ".svelte", ".astro":
		return nil, "", rebuildImportNeeds(edge, spec, "selected TS/JS caller (not an SFC host)")
	}

	var inputErr error
	var target *graph.Node
	var heldLanguage string
	module, symbol := splitImportSpecSymbol(spec)
	cause, event := "ts-alias", module
	if strings.HasPrefix(module, "./") || strings.HasPrefix(module, "../") {
		cause, event = "held-endpoint", spec
	}
	mark := func(targetFile string) error {
		if heldLanguage == "" || in.markIsolation == nil {
			return nil
		}
		return in.markIsolation(caller, event, targetFile, heldLanguage, cause)
	}
	getNode := func(id string) *graph.Node {
		if inputErr != nil {
			return nil
		}
		if node == nil {
			inputErr = rebuildImportNeeds(edge, spec, "checked graph point lookup")
			return nil
		}
		var n *graph.Node
		n, inputErr = node(id)
		if inputErr != nil {
			return nil
		}
		if n == nil {
			if heldLanguage == "" && in.heldPoint != nil {
				file := symbol == "" || !strings.HasSuffix(id, "::"+symbol)
				heldLanguage, inputErr = in.heldPoint(id, file)
			}
			return nil
		}
		if n.ID != id {
			inputErr = rebuildImportNeeds(edge, spec, "matching graph point identity")
			return nil
		}
		// A symbol miss leaves the preceding file hit available.
		target = n
		return n
	}
	alias := func(caller, module string) string {
		if in.pathAlias == nil {
			inputErr = rebuildImportNeeds(edge, spec, "checked TS path-alias/baseUrl result")
			return ""
		}
		var stem string
		stem, inputErr = in.pathAlias(caller, module)
		if inputErr != nil {
			return ""
		}
		return stem
	}
	id := resolveJSTSImportTarget(getNode, alias, caller, spec)
	if inputErr != nil {
		return nil, "", inputErr
	}
	if id != "" {
		return target, spec, mark(target.FilePath)
	}
	if err := mark(""); err != nil {
		return nil, "", err
	}

	// Native npm's cold indexer uses the raw FilePath (no From fallback),
	// case-sensitive extensions and the whole payload, including ::export.
	if strings.HasPrefix(spec, ".") || strings.HasPrefix(spec, "/") {
		return nil, spec, nil
	}
	switch path.Ext(edge.FilePath) {
	case ".ts", ".tsx", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs":
	default:
		return nil, spec, nil
	}
	if in.rewriteNpm == nil {
		return nil, "", rebuildImportNeeds(edge, spec, "checked npm alias/workspace/exports rewrite result")
	}
	effective, rewritten := rewriteNpmAliasImport(func(caller, raw string) string {
		var value string
		value, inputErr = in.rewriteNpm(caller, raw)
		return value
	}, edge.FilePath, spec)
	if inputErr != nil {
		return nil, "", inputErr
	}
	if !rewritten {
		return nil, effective, nil
	}
	heldLanguage = ""
	_, symbol = splitImportSpecSymbol(effective)
	cause, event = "npm-candidate", spec
	id = resolveJSTSImportTarget(getNode, alias, caller, effective)
	if inputErr != nil {
		return nil, "", inputErr
	}
	if id == "" {
		return nil, effective, mark("")
	}
	return target, effective, mark(target.FilePath)
}

package resolver

import (
	"path/filepath"

	"github.com/zzet/gortex/internal/graph"
)

// classifyBuiltin gives unresolved JS/TS or Python methods their legacy
// name-only UI category. A category containing "/" means shared across
// multiple builtin types, which the UI displays as "multi". Go stdlib
// calls use parser import aliases instead. See graph.LegacyBuiltinCategory
// for the tables.
func classifyBuiltin(method, lang string) (string, bool) {
	return graph.LegacyBuiltinCategory(method, lang)
}

// langFromFilePath infers a language family from a file extension.
// Returns "" when the extension isn't one we classify — callers should
// fall back to the normal Unresolved path in that case.
func langFromFilePath(p string) string {
	switch filepath.Ext(p) {
	case ".js", ".jsx", ".mjs", ".cjs":
		return "js"
	case ".ts", ".tsx", ".mts", ".cts":
		return "ts"
	case ".py":
		return "py"
	}
	return ""
}

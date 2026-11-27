package resolver

import (
	"path/filepath"

	"github.com/zzet/gortex/internal/graph"
)

// Built-in classifier for unresolved method calls.
//
// When resolveMethodCall can't attribute `x.Method()` to any indexed
// definition, we consult these maps so the flow trace UI can label
// `arr.push(x)` as `builtin::js::array::push` instead of the useless
// "unresolved::*.push". Coverage focuses on methods that actually
// show up in captured flows (String / Array / DOM / Promise / Map /
// Set for JS-family; str / list / dict / set for Python). Go stdlib
// is *not* in these maps — it's attributed via import aliases in
// the parser (see `extractCalls` in internal/parser/languages/golang.go).
//
// Value is a category string used purely for UI grouping. `/` means
// the method name is shared across multiple built-in types and we
// can't disambiguate without receiver-type inference. The UI treats
// anything with a `/` as "multi — take your pick".

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

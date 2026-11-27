package graph

import (
	"slices"
	"strings"
)

// PlatformCatalogRevision identifies the immutable table used by new builds.
// Keep each released revision available when adding a new one.
const PlatformCatalogRevision uint32 = 1

// PlatformBuiltin is a catalog entry, not a graph node or a scope assertion.
// Category is the canonical ID category (empty for Go functions).
type PlatformBuiltin struct {
	Language string
	Category string
	Name     string
}

func (p PlatformBuiltin) ID(repo string) string {
	if p.Category == "" {
		return StubID(repo, StubKindBuiltin, p.Language, p.Name)
	}
	return StubID(repo, StubKindBuiltin, p.Language, p.Category, p.Name)
}

// D94 keeps the effective native Go set, including the Go 1.21 additions.
var platformGoV1 = map[string][]string{
	"":      {"append", "cap", "clear", "close", "complex", "copy", "delete", "imag", "len", "make", "max", "min", "new", "panic", "print", "println", "real", "recover"},
	"type":  {"any", "bool", "byte", "comparable", "complex64", "complex128", "error", "float32", "float64", "int", "int8", "int16", "int32", "int64", "rune", "string", "uint", "uint8", "uint16", "uint32", "uint64", "uintptr"},
	"const": {"true", "false", "iota", "nil"},
}

// IsGoBuiltinFunction identifies bare names worth checking in the parser.
// Membership says nothing about a binding in this file or package.
func IsGoBuiltinFunction(name string) bool {
	entry, ok := platformGoByIDV1["go::"+name]
	return ok && entry.Category == ""
}

var platformGoByIDV1 = func() map[string]PlatformBuiltin {
	out := make(map[string]PlatformBuiltin)
	for category, names := range platformGoV1 {
		for _, name := range names {
			entry := PlatformBuiltin{Language: "go", Category: category, Name: name}
			out[strings.TrimPrefix(entry.ID(""), "builtin::")] = entry
		}
	}
	return out
}()

type platformWebGroup struct {
	family, form, category string
	members                []string
}

// Closed V1 exact list. Do not infer exact API membership from a legacy label.
var platformWebV1 = []platformWebGroup{
	// ECMA-262 Array.prototype methods.
	{"array", "instance", "array", []string{"slice"}},
	// ECMA-262 Array constructor methods.
	{"array", "static", "array.static", []string{"from", "isArray", "of"}},
	// ECMA-262 String.prototype methods.
	{"string", "instance", "string", []string{"slice"}},
	// ECMA-262 String constructor methods.
	{"string", "static", "string.static", []string{"fromCharCode", "fromCodePoint"}},
	// ECMA-262 Map.prototype methods; size is an accessor, not a Call.
	{"map", "instance", "map", []string{"get", "set", "has", "delete", "clear"}},
	// ECMA-262 Set.prototype methods; size is an accessor, not a Call.
	{"set", "instance", "set", []string{"add", "has", "delete", "clear"}},
	// ECMA-262 Promise.prototype methods.
	{"promise", "instance", "promise", []string{"then", "catch", "finally"}},
	// ECMA-262 Promise constructor methods.
	{"promise", "static", "promise.static", []string{"resolve", "reject", "all", "allSettled", "race", "any"}},
	// ECMA-262 Number.prototype methods.
	{"number", "instance", "number", []string{"toFixed", "toPrecision", "toExponential"}},
	// ECMA-262 Number constructor methods.
	{"number", "static", "number.static", []string{"parseFloat", "parseInt", "isNaN", "isFinite", "isInteger"}},
	// ECMA-262 JSON object methods.
	{"json", "static", "json", []string{"parse", "stringify"}},
	// ECMA-262 Object constructor methods.
	{"object", "static", "object", []string{"assign", "freeze", "create", "getPrototypeOf", "setPrototypeOf", "defineProperty", "defineProperties", "keys", "values", "entries"}},
	// WHATWG Console logging functions.
	{"console", "static", "console", []string{"log", "warn", "error", "debug", "info"}},
}

func catalogLanguage(lang string) string {
	switch lang {
	case "javascript":
		return "js"
	case "typescript":
		return "ts"
	default:
		return lang
	}
}

// LookupPlatformMember finds an exact catalog entry for a proved JS/TS receiver.
// An unknown, shadowed, imported or user receiver does not match.
func LookupPlatformMember(revision uint32, hint BuiltinReceiverHint, member string) (PlatformBuiltin, bool) {
	lang := catalogLanguage(hint.Language)
	if revision != 1 || (lang != "js" && lang != "ts") || hint.Module != "" {
		return PlatformBuiltin{}, false
	}
	for _, group := range platformWebV1 {
		if hint.Family == group.family && hint.Form == group.form && slices.Contains(group.members, member) {
			return PlatformBuiltin{Language: lang, Category: group.category, Name: member}, true
		}
	}
	return PlatformBuiltin{}, false
}

// LookupPlatformCandidate preserves the legacy name-only ID as a probable label.
// In particular, a grouped label is not a list of verified receiver methods.
func LookupPlatformCandidate(revision uint32, language, name string) (PlatformBuiltin, bool) {
	if revision != 1 {
		return PlatformBuiltin{}, false
	}
	lang := catalogLanguage(language)
	category, ok := LegacyBuiltinCategory(name, lang)
	if !ok {
		return PlatformBuiltin{}, false
	}
	return PlatformBuiltin{Language: lang, Category: category, Name: name}, true
}

// LookupPlatformCall applies the legacy name gate and the checked receiver
// evidence for a JS/TS Call. An empty status means no legacy candidate;
// unresolved means a same-language hint contradicts the builtin fallback.
func LookupPlatformCall(revision uint32, language, name string, hint BuiltinReceiverHint) (PlatformBuiltin, LinkStatus) {
	lang := catalogLanguage(language)
	if lang != "js" && lang != "ts" {
		return PlatformBuiltin{}, ""
	}
	candidate, ok := LookupPlatformCandidate(revision, lang, name)
	if !ok {
		return PlatformBuiltin{}, ""
	}
	if catalogLanguage(hint.Language) == lang {
		switch hint.Form {
		case "import", "shadowed", "user":
			return PlatformBuiltin{}, LinkStatusUnresolved
		}
		if exact, ok := LookupPlatformMember(revision, hint, name); ok {
			return exact, LinkStatusPlatform
		}
	}
	return candidate, LinkStatusProbablePlatform
}

// LookupPlatformBuiltin checks a canonical exact ID. It does not validate the
// repository boundary, site, or syntax evidence needed to admit an edge.
func LookupPlatformBuiltin(revision uint32, id string) (PlatformBuiltin, bool) {
	if revision != 1 || !IsBuiltinStub(id) {
		return PlatformBuiltin{}, false
	}
	repo := StubRepoPrefix(id)
	tail := strings.TrimPrefix(id, StubID(repo, StubKindBuiltin)+"::")
	if entry, ok := platformGoByIDV1[tail]; ok {
		return entry, true
	}
	lang, categoryName, ok := strings.Cut(tail, "::")
	if !ok || (lang != "js" && lang != "ts") {
		return PlatformBuiltin{}, false
	}
	category, name, ok := strings.Cut(categoryName, "::")
	if !ok {
		return PlatformBuiltin{}, false
	}
	for _, group := range platformWebV1 {
		if group.category == category && slices.Contains(group.members, name) {
			return PlatformBuiltin{Language: lang, Category: category, Name: name}, true
		}
	}
	return PlatformBuiltin{}, false
}

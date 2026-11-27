package contracts

import (
	"encoding/gob"
	"strings"
)

// Shape is stored in graph.Node.Meta (a map[string]any), which gets
// gob-encoded into the daemon snapshot. Gob refuses to encode a concrete
// type reached through an interface slot unless that type is registered,
// so the first node with a shape would abort the snapshot write with
// "type not registered for interface: contracts.Shape". Register the
// pointer form because that's what ExtractShape returns and what the
// indexer writes into Meta.
func init() {
	gob.Register((*Shape)(nil))
}

// Shape captures the externally-visible structure of a type that's
// referenced as a contract's request / response body. It's attached to
// the type's graph node (via `Meta["shape"]`) so downstream tooling —
// the UI schema pane, a `contracts validate` pass, change-impact
// analysis — can diff field lists without re-reading source.
//
// We serialise the shape explicitly rather than relying on whoever
// reads the node to re-parse the file, because field-level diffing
// needs to work even when only one side of a cross-repo contract is
// still indexed.
type Shape struct {
	// Kind is the syntactic flavour of the definition:
	// "struct" (Go), "interface" | "type" (TS), "class" (Python),
	// "class" (Java/Kotlin), "message" (proto).
	Kind   string       `json:"kind"`
	Fields []ShapeField `json:"fields"`
	// Notes carries extractor diagnostics that aren't a hard error
	// but tell a reader why the shape might be incomplete.
	Notes []string `json:"notes,omitempty"`
}

// ShapeField is one field on a type.
type ShapeField struct {
	// Name is the wire name (serialised form). For Go this is the JSON
	// tag when present, else the struct field name lower-cased? No —
	// we preserve the declared field name and put the wire tag in
	// JSONTag. Consumers choose which to display.
	Name string `json:"name"`
	// Type is a textual type expression — "string", "int64",
	// "[]User", "Profile", "Optional[int]", "List<User>". May be a
	// bare identifier (upgradeable to a symbol ID later) or a
	// compound expression.
	Type string `json:"type"`
	// JSONTag is the `json:"..."` value on Go struct tags,
	// `@JsonProperty("...")` on Java / Jackson, `alias="..."` on
	// Pydantic, `json_name = "..."` on proto. Empty when none.
	JSONTag string `json:"json_tag,omitempty"`
	// Required is true when the field must be present on the wire.
	// Go: tag has no `omitempty` AND field is not a pointer.
	// TS:  no trailing `?` and not unioned with `null` / `undefined`.
	// Python: no default value AND type isn't Optional / | None.
	// Java: no `@Nullable` / `Optional<T>`.
	// Proto3: default is optional; `required` synthesises the flag.
	Required bool `json:"required"`
	// Repeated is true for list / array / slice types.
	Repeated bool `json:"repeated,omitempty"`
	// Comment is the adjacent docstring / line comment when present.
	// We keep at most one line; long comments get truncated so the
	// graph payload stays compact.
	Comment string `json:"comment,omitempty"`
}

// ExtractShape dispatches to the correct language-specific extractor
// based on the type's file extension. Returns nil when the file isn't
// a supported language or the extractor couldn't find any fields.
// Callers attach the returned Shape to the type node's meta.
func ExtractShape(filePath string, src []byte, startLine, endLine int) *Shape {
	if startLine <= 0 {
		return nil
	}
	extract := ShapeExtractorForPath(filePath)
	if extract == nil {
		return nil
	}
	return extract(strings.Split(string(src), "\n"), startLine, endLine)
}

// ShapeExtractorForPath selects the native shape extractor for a path.
// The returned function takes source split on newlines so a file census can
// reuse the same lines for all declarations. Unsupported paths return nil.
func ShapeExtractorForPath(filePath string) func(lines []string, startLine, endLine int) *Shape {
	switch detectLanguage(filePath) {
	case "go":
		return extractGoShape
	case "typescript", "javascript":
		return extractTSShape
	case "python":
		return extractPythonShape
	case "java", "kotlin":
		return extractJavaShape
	case "dart":
		return extractDartShape
	case "rust":
		return extractRustShape
	case "csharp":
		return extractCSharpShape
	}
	if strings.HasSuffix(filePath, ".proto") {
		return extractProtoShape
	}
	return nil
}

// sliceBody returns the source lines from start_line to end_line
// (1-based, inclusive). When end_line is zero or less than start the
// parser didn't record a span — caller should use a brace-walked
// fallback or skip.
func sliceBody(lines []string, startLine, endLine int) string {
	if startLine <= 0 || startLine > len(lines) {
		return ""
	}
	end := endLine
	if end <= 0 || end > len(lines) {
		end = len(lines)
	}
	if end < startLine {
		end = startLine
	}
	return strings.Join(lines[startLine-1:end], "\n")
}

// braceBody is the fallback when a type node's end_line isn't
// recorded. Walk from start_line forward, count braces, and stop when
// the first one closes.
func braceBody(lines []string, startLine, maxLines int) string {
	if startLine <= 0 || startLine > len(lines) {
		return ""
	}
	depth := 0
	opened := false
	end := startLine
	limit := startLine + maxLines
	if limit > len(lines) {
		limit = len(lines)
	}
	for i := startLine - 1; i < limit; i++ {
		line := lines[i]
		for _, ch := range line {
			if ch == '{' {
				depth++
				opened = true
			} else if ch == '}' && opened {
				depth--
			}
		}
		end = i + 1
		if opened && depth <= 0 {
			break
		}
	}
	return strings.Join(lines[startLine-1:end], "\n")
}

// truncateComment keeps a field's doc comment to a reasonable size so
// the shape payload doesn't balloon on heavily-annotated types.
func truncateComment(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 120 {
		s = s[:117] + "..."
	}
	return s
}

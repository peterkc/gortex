package contracts

import (
	"regexp"
	"strings"
)

// goStructBodyRe locates the body of a `type <Name> struct { ... }`
// declaration starting from start_line. We use it only as a quick
// shortcut when end_line isn't recorded — normally the parser gives
// us an accurate span and we slice that directly.
var goStructOpenRe = regexp.MustCompile(`(?s)type\s+\w+\s+struct\s*\{`)

// goFieldRe parses one Go struct field line. Capture groups:
//
//	1: field name (or comma-separated names)
//	2: type expression (with optional leading * / [] / map[])
//	3: raw tag body including backticks when present
//
// Embedded fields (`sync.Mutex`, `io.Reader`) don't match this regex;
// we skip them by checking that we got two non-empty groups.
var goFieldRe = regexp.MustCompile(
	"^\\s*([A-Z][A-Za-z0-9_, ]*?)\\s+" + // 1: field name(s) — exported only
		"(\\*?(?:\\[\\]|map\\[[^\\]]+\\])?[A-Za-z_][\\w.\\[\\]*]*)" + // 2: type expr
		"(?:\\s+`([^`]*)`)?\\s*(?://\\s*(.*))?$", // 3: tag, 4: comment
)

// extractGoShape reads a Go `type X struct { ... }` and returns its
// exported field list. Non-exported fields (lower-case first letter)
// are dropped because they never appear on the wire. Embedded fields
// are skipped — they require cross-file resolution we don't do here.
func extractGoShape(sourceLines []string, startLine, endLine int) *Shape {
	body := sliceBody(sourceLines, startLine, endLine)
	if body == "" {
		return nil
	}
	// Normalise: find the opening brace. Anything before it (package
	// docs, `type` keyword, the name) is noise.
	openIdx := strings.Index(body, "{")
	if openIdx < 0 {
		// No opening brace in the slice — fall back to brace walk so
		// types whose end_line == start_line still produce a shape.
		body = braceBody(sourceLines, startLine, 300)
		openIdx = strings.Index(body, "{")
		if openIdx < 0 {
			return nil
		}
	}
	// Balance braces to find the close that matches.
	depth := 0
	closeIdx := -1
	for i := openIdx; i < len(body); i++ {
		switch body[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				closeIdx = i
			}
		}
		if closeIdx >= 0 {
			break
		}
	}
	if closeIdx < 0 {
		return nil
	}
	inner := body[openIdx+1 : closeIdx]

	shape := &Shape{Kind: "struct"}
	// Track brace depth across the inner body so we don't hoist
	// fields of nested anonymous structs into the parent shape.
	// Lines are scanned at depth 0 (= immediately inside the outer
	// struct's `{`); when an inline `Stats struct { ... }` opens,
	// depth goes to 1 and child lines are suppressed until the
	// closing `}` returns depth to 0. Without this, a parent that
	// happens to share field names with its inline anonymous
	// substruct (e.g. dashboardSnapshot's outer `Repos`/`Caveats`
	// shadowed by Stats.Repos/Stats.Caveats) produces duplicate
	// field names in the shape — the dashboard then renders two
	// React children with the same key.
	depth = 0
	for _, rawLine := range strings.Split(inner, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "//") {
			updateGoBraceDepth(line, &depth)
			continue
		}
		if depth == 0 {
			emitGoStructField(line, shape)
		}
		updateGoBraceDepth(line, &depth)
	}
	if len(shape.Fields) == 0 {
		return nil
	}
	return shape
}

// updateGoBraceDepth adjusts the running brace depth from one line
// of source. Quoted strings and runes don't carry depth-changing
// braces in well-formed Go struct definitions, so a naive count
// suffices for the lines we care about. A `//` comment short-circuits
// the rest of the line.
func updateGoBraceDepth(line string, depth *int) {
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '{':
			*depth++
		case '}':
			if *depth > 0 {
				*depth--
			}
		case '/':
			if i+1 < len(line) && line[i+1] == '/' {
				return
			}
		}
	}
}

// emitGoStructField runs goFieldRe over one line and appends every
// resulting field (a single decl can declare multiple comma-separated
// names) to shape.
func emitGoStructField(line string, shape *Shape) {
	m := goFieldRe.FindStringSubmatch(line)
	if m == nil {
		return
	}
	names := strings.Split(m[1], ",")
	typeExpr := strings.TrimSpace(m[2])
	tag := m[3]
	comment := ""
	if len(m) > 4 {
		comment = truncateComment(m[4])
	}
	jsonTag, required, repeated, effectiveType := goFieldMetadata(typeExpr, tag)
	for _, rawName := range names {
		name := strings.TrimSpace(rawName)
		if name == "" || !isExportedGo(name) {
			continue
		}
		wireName := name
		if jsonTag != "" && jsonTag != "-" {
			wireName = jsonTag
		}
		shape.Fields = append(shape.Fields, ShapeField{
			Name:     wireName,
			Type:     effectiveType,
			JSONTag:  tag,
			Required: required,
			Repeated: repeated,
			Comment:  comment,
		})
	}
}

// goFieldMetadata turns a type expression + raw struct tag into the
// pieces ShapeField exposes.
//
//	typeExpr  — the declared Go type text
//	tag       — raw tag body without the surrounding backticks
//
// Returns: wire name from json tag (empty = use field name), required
// (true iff tag has no `omitempty` and the type isn't a pointer),
// repeated (slice / array type), and the effective type name with
// leading `*` / `[]` stripped for diffing friendliness.
func goFieldMetadata(typeExpr, tag string) (jsonName string, required, repeated bool, effective string) {
	// Parse json:"..." out of the raw tag. Tag syntax is
	// `json:"name,opt1,opt2" xml:"..."` with space separators.
	for _, entry := range strings.Fields(tag) {
		if !strings.HasPrefix(entry, "json:") {
			continue
		}
		v := strings.TrimPrefix(entry, "json:")
		v = strings.Trim(v, `"`)
		parts := strings.Split(v, ",")
		jsonName = parts[0]
		for _, p := range parts[1:] {
			if p == "omitempty" {
				// Marked optional.
				required = false
				return jsonName, required, strings.HasPrefix(typeExpr, "[]"), strings.TrimPrefix(strings.TrimPrefix(typeExpr, "*"), "[]")
			}
		}
		break
	}

	pointer := strings.HasPrefix(typeExpr, "*")
	repeated = strings.HasPrefix(typeExpr, "[]")
	effective = strings.TrimPrefix(strings.TrimPrefix(typeExpr, "*"), "[]")
	// A pointer without `omitempty` is still effectively optional in
	// most Go JSON handling, so we call it not-required. The explicit
	// `omitempty` branch above short-circuits before this; here we
	// infer from type.
	required = !pointer
	if jsonName == "-" {
		// Field is explicitly excluded from JSON.
		jsonName = ""
		required = false
	}
	return jsonName, required, repeated, effective
}

func isExportedGo(name string) bool {
	if name == "" {
		return false
	}
	r := name[0]
	return r >= 'A' && r <= 'Z'
}

var _ = goStructOpenRe // reserved for future "find next struct" helper.

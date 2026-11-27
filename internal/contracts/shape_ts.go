package contracts

import (
	"regexp"
	"strings"
)

// tsFieldRe matches a single member line inside a TS interface or
// type-literal:
//
//	name?: Type                      → optional
//	readonly name: Type              → required (readonly flag ignored)
//	'quoted-name': Type              → required, wire name is the literal
//	name: string | null              → optional (nullable union)
//	name: Array<Foo>                 → repeated
//
// Capture groups:
//
//	1: field name (may include quotes)
//	2: "?" when optional declared at the field level
//	3: type expression to the end of the line, trailing comma / semicolon trimmed
var tsFieldRe = regexp.MustCompile(`^\s*(?:readonly\s+)?(['"]?[\w$-]+['"]?)\s*(\??)\s*:\s*(.+?)\s*[,;]?\s*(?://\s*(.*))?$`)

// extractTSShape reads a TypeScript interface or type literal and
// returns its declared members.
//
// Supported shapes:
//
//	interface Foo { a: string; b?: number }
//	type Foo = { a: string; b?: number }
//	interface Foo extends Bar { a: string }
//	class Foo { a: string; b?: number; method(): void { ... } }
//
// Methods on classes are filtered out by the line pattern: lines
// containing `(` before the first `:` don't match tsFieldRe.
func extractTSShape(sourceLines []string, startLine, endLine int) *Shape {
	body := sliceBody(sourceLines, startLine, endLine)
	if body == "" {
		body = braceBody(sourceLines, startLine, 300)
	}
	openIdx := strings.Index(body, "{")
	if openIdx < 0 {
		return nil
	}
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

	kind := "interface"
	firstLine := body[:openIdx]
	if strings.Contains(firstLine, " type ") || strings.HasPrefix(strings.TrimSpace(firstLine), "type ") {
		kind = "type"
	} else if strings.Contains(firstLine, " class ") || strings.HasPrefix(strings.TrimSpace(firstLine), "class ") {
		kind = "class"
	}

	shape := &Shape{Kind: kind}
	// Track brace depth across the inner body so we don't hoist
	// fields of a nested anonymous object into the parent shape.
	// Lines are scanned at depth 0 (= immediately inside the outer
	// `{`); when an inline `stats: { ... }` opens, depth goes to 1
	// and child lines are suppressed until the closing `}` returns
	// depth to 0. Without this, a parent that happens to share field
	// names with its inline anonymous substruct (e.g. DashboardSnapshot's
	// outer `repos`/`caveats` shadowed by Stats.repos/Stats.caveats)
	// produces duplicate field names in the shape — the dashboard
	// then renders two React children with the same key.
	depth = 0
	for _, rawLine := range strings.Split(inner, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "//") {
			updateTSBraceDepth(line, &depth)
			continue
		}
		if depth == 0 {
			emitTSShapeField(line, shape)
		}
		updateTSBraceDepth(line, &depth)
	}
	if len(shape.Fields) == 0 {
		return nil
	}
	return shape
}

// emitTSShapeField runs tsFieldRe over one source line and appends a
// matching field to shape. Method members (`foo(): R`, `foo(args): R
// { ... }`) are skipped because their `(` comes before any `:` —
// arrow-typed properties (`foo: (a: A) => R`) still pass since the
// `:` is the FIRST punctuator. When the matched type expression is
// just an opening `{` (the head of an inline-nested object literal
// whose body the brace-depth gate suppressed), we normalise the
// recorded type to `{...}` so the dashboard renders a sensible
// label instead of a stray brace.
func emitTSShapeField(line string, shape *Shape) {
	colonIdx := strings.Index(line, ":")
	parenIdx := strings.Index(line, "(")
	if parenIdx >= 0 && (colonIdx < 0 || parenIdx < colonIdx) {
		return
	}
	m := tsFieldRe.FindStringSubmatch(line)
	if m == nil {
		return
	}
	nameRaw := strings.Trim(m[1], `'"`)
	optional := m[2] == "?"
	typeExpr := strings.TrimSpace(m[3])
	comment := ""
	if len(m) > 4 {
		comment = truncateComment(m[4])
	}
	if typeExpr == "{" {
		typeExpr = "{...}"
	}
	required := !optional && !tsTypeIsNullable(typeExpr)
	repeated := tsTypeIsRepeated(typeExpr)
	shape.Fields = append(shape.Fields, ShapeField{
		Name:     nameRaw,
		Type:     typeExpr,
		Required: required,
		Repeated: repeated,
		Comment:  comment,
	})
}

// updateTSBraceDepth adjusts the running brace depth from one line
// of source. Strings and template literals can contain literal
// braces, but an interface / type-literal body that uses them
// outside an obvious template tag is rare enough we treat them as
// real braces; the resulting mis-counts only suppress emit at the
// next line, which is a conservative loss vs. the duplicate-key
// regression. A `//` comment short-circuits the rest of the line.
func updateTSBraceDepth(line string, depth *int) {
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

// tsTypeIsNullable returns true for union types that include `null`
// or `undefined`. Those are optional on the wire even without the
// trailing `?` on the field name.
func tsTypeIsNullable(t string) bool {
	parts := strings.Split(t, "|")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "null" || p == "undefined" {
			return true
		}
	}
	return false
}

// tsTypeIsRepeated returns true for `Foo[]`, `Array<Foo>`, or
// `ReadonlyArray<Foo>`.
func tsTypeIsRepeated(t string) bool {
	t = strings.TrimSpace(t)
	if strings.HasSuffix(t, "[]") {
		return true
	}
	if strings.HasPrefix(t, "Array<") || strings.HasPrefix(t, "ReadonlyArray<") {
		return true
	}
	return false
}

package contracts

import (
	"regexp"
	"strings"
)

// pyFieldRe captures a Python class attribute with a type annotation.
// Pydantic V1/V2 models and plain typed dataclasses both use this
// form. Capture groups:
//
//	1: attribute name
//	2: type expression (up to the optional `=` or end of line)
//	3: default value expression (after `=`) — empty when no default
var pyFieldRe = regexp.MustCompile(`^\s*([A-Za-z_][\w]*)\s*:\s*([^=#\n]+?)(?:\s*=\s*(.+?))?\s*(?:#\s*(.*))?$`)

// pyFieldAliasRe pulls alias="wire" out of a Pydantic Field(...) call,
// which is how users rename a Python-legal attribute to a JSON-valid
// wire name.
var pyFieldAliasRe = regexp.MustCompile(`alias\s*=\s*['"]([^'"]+)['"]`)

// extractPythonShape reads the body of a class definition and returns
// its typed attributes. Attributes without a type annotation are
// skipped because we can't record a useful type for them. Methods
// are skipped by requiring `:` before any `(`.
func extractPythonShape(sourceLines []string, startLine, endLine int) *Shape {
	body := sliceBody(sourceLines, startLine, endLine)
	if body == "" {
		return nil
	}
	lines := strings.Split(body, "\n")
	if len(lines) == 0 {
		return nil
	}

	// Determine the indent of the class body so we can stop scanning
	// at the next declaration at or below the class indent. Python
	// classes have `class Foo(Bar):` on the first line and the body
	// on subsequent indented lines.
	classIndent := leadingWhitespace(lines[0])
	shape := &Shape{Kind: "class"}

	for i, raw := range lines {
		if i == 0 {
			continue // class header
		}
		if strings.TrimSpace(raw) == "" {
			continue
		}
		indent := leadingWhitespace(raw)
		if len(indent) <= len(classIndent) {
			// De-dent — end of class body.
			break
		}
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "#") {
			continue
		}
		// Skip methods: def / async def.
		if strings.HasPrefix(line, "def ") || strings.HasPrefix(line, "async def ") {
			continue
		}
		// Skip decorator lines at field scope.
		if strings.HasPrefix(line, "@") {
			continue
		}
		// Require a `:` to appear before any `(`.
		colonIdx := strings.Index(line, ":")
		if colonIdx < 0 {
			continue
		}
		parenIdx := strings.Index(line, "(")
		if parenIdx >= 0 && parenIdx < colonIdx {
			continue
		}
		m := pyFieldRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		typeExpr := strings.TrimSpace(m[2])
		defaultExpr := ""
		comment := ""
		if len(m) > 3 {
			defaultExpr = strings.TrimSpace(m[3])
		}
		if len(m) > 4 {
			comment = truncateComment(m[4])
		}
		if typeExpr == "" {
			continue
		}
		alias := ""
		if defaultExpr != "" {
			if am := pyFieldAliasRe.FindStringSubmatch(defaultExpr); len(am) > 1 {
				alias = am[1]
			}
		}
		required := defaultExpr == "" && !pyTypeIsOptional(typeExpr)
		repeated := pyTypeIsRepeated(typeExpr)
		wireName := name
		if alias != "" {
			wireName = alias
		}
		shape.Fields = append(shape.Fields, ShapeField{
			Name:     wireName,
			Type:     typeExpr,
			JSONTag:  alias,
			Required: required,
			Repeated: repeated,
			Comment:  comment,
		})
	}
	if len(shape.Fields) == 0 {
		return nil
	}
	return shape
}

func pyTypeIsOptional(t string) bool {
	t = strings.TrimSpace(t)
	if strings.HasPrefix(t, "Optional[") {
		return true
	}
	// `str | None` / `None | str` — Python 3.10+ union syntax.
	for _, p := range strings.Split(t, "|") {
		if strings.TrimSpace(p) == "None" {
			return true
		}
	}
	return false
}

func pyTypeIsRepeated(t string) bool {
	t = strings.TrimSpace(t)
	// list[Foo], List[Foo], tuple[Foo, ...], Sequence[Foo]
	for _, pfx := range []string{"list[", "List[", "tuple[", "Tuple[", "Sequence[", "Iterable[", "set[", "Set[", "FrozenSet["} {
		if strings.HasPrefix(t, pfx) {
			return true
		}
	}
	return false
}

func leadingWhitespace(s string) string {
	for i, r := range s {
		if r != ' ' && r != '\t' {
			return s[:i]
		}
	}
	return s
}

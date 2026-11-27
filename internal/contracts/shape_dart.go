package contracts

import (
	"regexp"
	"strings"
)

// Dart class field forms we recognise:
//
//	final String id;
//	final String? nickname;           // nullable
//	final List<String> tags;
//	final int count;
//	@JsonKey(name: 'user_id') final String userId;
//
// The shape extractor is intentionally narrow — full Dart parsing
// belongs in the tree-sitter extractor. This is the regex-based
// snapshot of declared instance fields that back a fromJson/toJson
// contract.
var (
	dartFieldRe = regexp.MustCompile(
		`^\s*(?:final\s+|const\s+|static\s+|late\s+)*([A-Za-z_][\w<>,\s?]*?)\s+([A-Za-z_]\w*)\s*(?:=\s*[^;]+)?\s*;`,
	)
	dartJSONKeyRe = regexp.MustCompile(`@JsonKey\(\s*name\s*:\s*['"]([^'"]+)['"]`)
)

// extractDartShape reads a Dart class body and returns its declared
// instance fields. Constructor parameters and method declarations are
// skipped — we look only for top-level `<Type> <name>;` entries in
// the class body.
func extractDartShape(sourceLines []string, startLine, endLine int) *Shape {
	body := sliceBody(sourceLines, startLine, endLine)
	if body == "" {
		body = braceBody(sourceLines, startLine, 400)
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

	lines := strings.Split(inner, "\n")
	shape := &Shape{Kind: "class"}
	var pendingAnnotations []string
	nesting := 0

	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		if strings.HasPrefix(line, "@") {
			pendingAnnotations = append(pendingAnnotations, line)
			continue
		}
		// Track nested method bodies so we don't misread local
		// variable declarations inside them as fields.
		opens := strings.Count(line, "{")
		closes := strings.Count(line, "}")
		if nesting > 0 {
			nesting += opens - closes
			pendingAnnotations = nil
			continue
		}
		if strings.Contains(line, "(") && strings.Contains(line, "{") {
			// method / ctor body opens on this line.
			nesting += opens - closes
			pendingAnnotations = nil
			continue
		}
		// Skip lines that look like method / ctor declarations
		// without a body (abstract, arrow-body, constructor).
		if looksLikeDartMethod(line) {
			pendingAnnotations = nil
			continue
		}
		m := dartFieldRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		typeExpr := strings.TrimSpace(m[1])
		name := m[2]
		if typeExpr == "" || !isDartUserField(typeExpr) {
			pendingAnnotations = nil
			continue
		}
		nullable := strings.HasSuffix(typeExpr, "?")
		typeExpr = strings.TrimSuffix(typeExpr, "?")

		jsonAlias := ""
		for _, ann := range pendingAnnotations {
			if mm := dartJSONKeyRe.FindStringSubmatch(ann); len(mm) > 1 {
				jsonAlias = mm[1]
			}
		}
		wireName := name
		if jsonAlias != "" {
			wireName = jsonAlias
		}
		shape.Fields = append(shape.Fields, ShapeField{
			Name:     wireName,
			Type:     stripGenericsSimple(typeExpr),
			JSONTag:  jsonAlias,
			Required: !nullable,
			Repeated: dartTypeIsRepeated(typeExpr),
		})
		pendingAnnotations = nil
	}
	if len(shape.Fields) == 0 {
		return nil
	}
	return shape
}

// looksLikeDartMethod trips on signatures like
//
//	String get name;
//	void save();
//	Foo copyWith({String? x});
//
// by requiring a `(` before any `;`. Field declarations never have
// parens before the terminator.
func looksLikeDartMethod(line string) bool {
	paren := strings.Index(line, "(")
	if paren < 0 {
		return false
	}
	semi := strings.Index(line, ";")
	if semi >= 0 && paren > semi {
		return false
	}
	return true
}

func isDartUserField(typ string) bool {
	typ = strings.TrimSpace(typ)
	if typ == "" {
		return false
	}
	// Skip obvious keywords that can appear at start of a class-body
	// line but aren't fields.
	for _, kw := range []string{"return", "if", "else", "for", "while", "switch", "try"} {
		if strings.HasPrefix(typ, kw) {
			return false
		}
	}
	return true
}

func stripGenericsSimple(t string) string {
	t = strings.TrimSpace(t)
	if i := strings.Index(t, "<"); i >= 0 {
		return strings.TrimSpace(t[:i])
	}
	return t
}

func dartTypeIsRepeated(t string) bool {
	t = strings.TrimSpace(t)
	for _, pfx := range []string{"List<", "Set<", "Iterable<"} {
		if strings.HasPrefix(t, pfx) {
			return true
		}
	}
	return false
}

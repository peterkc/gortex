package contracts

import (
	"regexp"
	"strings"
)

// csPropertyRe captures a C# auto-property declaration:
//
//	public string Id { get; set; }
//	public string? Nickname { get; init; }
//	[JsonPropertyName("id")]
//	public required Guid Id { get; set; }
//	public List<User> Friends { get; set; } = new();
//
// The visibility + optional `required` / `readonly` / `static` prefixes
// are consumed; capture 1 is the type, capture 2 is the name.
var csPropertyRe = regexp.MustCompile(
	`^\s*(?:public|internal|protected|private)\s+(?:(?:required|readonly|static|virtual|override|abstract)\s+)*([A-Za-z_][\w<>,.?\s]*?)\s+([A-Za-z_]\w*)\s*(?:\{|=>)`,
)

// csJsonPropertyRe extracts the wire name from `[JsonPropertyName("x")]`
// or the older Newtonsoft `[JsonProperty("x")]`.
var csJsonPropertyRe = regexp.MustCompile(`\[(?:JsonPropertyName|JsonProperty)\(\s*"([^"]+)"`)

// csJsonIgnoreRe marks a property as excluded from the wire.
var csJsonIgnoreRe = regexp.MustCompile(`\[JsonIgnore`)

// extractCSharpShape reads a C# class / record body and returns its
// properties. Fields are not returned (fields aren't serialised by
// System.Text.Json by default; properties are). Records with
// positional parameters are captured through their synthesised
// properties — most modern DTOs use this form.
func extractCSharpShape(sourceLines []string, startLine, endLine int) *Shape {
	body := sliceBody(sourceLines, startLine, endLine)
	if body == "" {
		body = braceBody(sourceLines, startLine, 400)
	}
	// Positional records: `public record UserResp(string Id, string Email);`
	// Detected separately — no `{` body, all props in the paren group.
	if recShape := extractCSharpPositionalRecord(body); recShape != nil {
		return recShape
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
	var pendingAttrs []string
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "//") || strings.HasPrefix(line, "///") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			pendingAttrs = append(pendingAttrs, line)
			continue
		}
		m := csPropertyRe.FindStringSubmatch(line)
		if m == nil {
			pendingAttrs = nil
			continue
		}
		typeExpr := strings.TrimSpace(m[1])
		name := m[2]

		// Parse attributes.
		var jsonTag string
		ignored := false
		for _, ann := range pendingAttrs {
			if jm := csJsonPropertyRe.FindStringSubmatch(ann); len(jm) > 1 {
				jsonTag = jm[1]
			}
			if csJsonIgnoreRe.MatchString(ann) {
				ignored = true
			}
		}
		pendingAttrs = nil
		if ignored {
			continue
		}

		nullable := strings.HasSuffix(typeExpr, "?")
		typeExpr = strings.TrimSuffix(typeExpr, "?")
		repeated := strings.HasPrefix(typeExpr, "List<") ||
			strings.HasPrefix(typeExpr, "IEnumerable<") ||
			strings.HasPrefix(typeExpr, "ICollection<") ||
			strings.HasPrefix(typeExpr, "IReadOnlyList<") ||
			strings.HasSuffix(typeExpr, "[]")

		wireName := name
		if jsonTag != "" {
			wireName = jsonTag
		}
		shape.Fields = append(shape.Fields, ShapeField{
			Name:     wireName,
			Type:     stripGenericsCSharp(typeExpr),
			JSONTag:  jsonTag,
			Required: !nullable,
			Repeated: repeated,
		})
	}
	if len(shape.Fields) == 0 {
		return nil
	}
	return shape
}

// csRecordHeadRe matches a positional record declaration:
//
//	public record UserResp(string Id, string Email);
//
// Capture 1 is the parameter list.
var csRecordHeadRe = regexp.MustCompile(
	`\brecord\s+(?:struct\s+)?\w+\s*\(\s*([^)]+)\s*\)`,
)

func extractCSharpPositionalRecord(body string) *Shape {
	m := csRecordHeadRe.FindStringSubmatch(body)
	if len(m) < 2 {
		return nil
	}
	shape := &Shape{Kind: "class"}
	for _, p := range splitTopLevelArgsCS(m[1]) {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		fields := strings.Fields(p)
		if len(fields) < 2 {
			continue
		}
		typeExpr := fields[len(fields)-2]
		name := fields[len(fields)-1]
		nullable := strings.HasSuffix(typeExpr, "?")
		typeExpr = strings.TrimSuffix(typeExpr, "?")
		repeated := strings.HasPrefix(typeExpr, "List<") || strings.HasSuffix(typeExpr, "[]")
		shape.Fields = append(shape.Fields, ShapeField{
			Name:     name,
			Type:     stripGenericsCSharp(typeExpr),
			Required: !nullable,
			Repeated: repeated,
		})
	}
	if len(shape.Fields) == 0 {
		return nil
	}
	return shape
}

// splitTopLevelArgsCS is the C# twin of the Go arg splitter — commas
// inside `<...>` or `(...)` don't terminate an argument.
func splitTopLevelArgsCS(s string) []string {
	var out []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '<', '(', '[':
			depth++
		case '>', ')', ']':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	if last := strings.TrimSpace(s[start:]); last != "" {
		out = append(out, last)
	}
	return out
}

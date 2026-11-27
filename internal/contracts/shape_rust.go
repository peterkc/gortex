package contracts

import (
	"regexp"
	"strings"
)

// rustFieldLineRe captures a serde-serialisable struct field line.
// Visibility qualifier + name + type:
//
//	pub name: String,
//	pub(crate) name: Option<String>,
//	name: Vec<T>,                     (private — still indexable)
//
// Generic arguments are kept in the capture so container detection
// (`Vec<T>`, `HashMap<K,V>`) and optionality (`Option<T>`) can be
// read out of the type string.
var rustFieldLineRe = regexp.MustCompile(
	`^\s*(?:pub(?:\([^)]*\))?\s+)?(\w+)\s*:\s*([A-Za-z_][\w:]*(?:<[^>]+>)?)\s*,?\s*$`,
)

// rustSerdeAttrRe catches serde attribute macros on the preceding
// lines. `#[serde(rename = "x")]`, `#[serde(skip_serializing_if = "...", default)]`.
var rustSerdeAttrRe = regexp.MustCompile(`#\[serde\(([^)]*)\)\]`)

// rustSerdeRenameRe isolates the `rename = "wire"` assignment inside
// a serde attribute body.
var rustSerdeRenameRe = regexp.MustCompile(`rename\s*=\s*"([^"]+)"`)

// extractRustShape reads a Rust struct body and returns its fields.
// Enum variants are not currently handled — contracts rarely use
// enum bodies directly, and when they do (tagged unions) they need
// bespoke serde handling that's beyond scope here.
func extractRustShape(sourceLines []string, startLine, endLine int) *Shape {
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
	shape := &Shape{Kind: "struct"}
	var pendingAttrs []string
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "//") || strings.HasPrefix(line, "///") {
			continue
		}
		if strings.HasPrefix(line, "#[") {
			pendingAttrs = append(pendingAttrs, line)
			continue
		}
		m := rustFieldLineRe.FindStringSubmatch(line)
		if m == nil {
			pendingAttrs = nil
			continue
		}
		name := m[1]
		typeExpr := strings.TrimSpace(m[2])

		// serde attribute parsing.
		var serdeTag string
		skipSerializing := false
		hasDefault := false
		for _, ann := range pendingAttrs {
			am := rustSerdeAttrRe.FindStringSubmatch(ann)
			if am == nil {
				continue
			}
			contents := am[1]
			if rm := rustSerdeRenameRe.FindStringSubmatch(contents); len(rm) > 1 {
				serdeTag = rm[1]
			}
			if strings.Contains(contents, "skip_serializing_if") {
				skipSerializing = true
			}
			if strings.Contains(contents, "default") {
				hasDefault = true
			}
		}
		pendingAttrs = nil

		nullable := strings.HasPrefix(typeExpr, "Option<") || skipSerializing || hasDefault
		repeated := strings.HasPrefix(typeExpr, "Vec<") ||
			strings.HasPrefix(typeExpr, "HashSet<") ||
			strings.HasPrefix(typeExpr, "BTreeSet<")
		effective := stripGenericsRust(typeExpr)
		// Peel one level of Option / Box / Vec for the display type
		// so `Option<String>` shows as `String` with ?, `Vec<Item>`
		// shows as `Item[]`, mirroring other languages.
		if strings.HasPrefix(typeExpr, "Option<") && strings.HasSuffix(typeExpr, ">") {
			effective = stripGenericsRust(strings.TrimSuffix(strings.TrimPrefix(typeExpr, "Option<"), ">"))
		}
		if repeated {
			// Strip the outer container.
			idx := strings.Index(typeExpr, "<")
			if idx >= 0 {
				effective = stripGenericsRust(strings.TrimSuffix(typeExpr[idx+1:], ">"))
			}
		}

		wireName := name
		if serdeTag != "" {
			wireName = serdeTag
		}
		shape.Fields = append(shape.Fields, ShapeField{
			Name:     wireName,
			Type:     stripRustPath(effective),
			JSONTag:  serdeTag,
			Required: !nullable,
			Repeated: repeated,
		})
	}
	if len(shape.Fields) == 0 {
		return nil
	}
	return shape
}

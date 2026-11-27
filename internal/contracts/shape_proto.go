package contracts

import (
	"regexp"
	"strings"
)

// protoFieldRe matches a proto3 field inside a message body:
//
//	string email = 1;
//	optional string password = 2;
//	repeated Tag tags = 3;
//	map<string, Foo> items = 4;
//	google.protobuf.Timestamp created = 5 [json_name = "createdAt"];
var protoFieldRe = regexp.MustCompile(
	`^\s*(optional\s+|repeated\s+|required\s+)?` +
		`((?:map<[^>]+>)|[A-Za-z_][\w.]*)\s+` +
		`(\w+)\s*=\s*\d+\s*` +
		`(?:\[([^\]]*)\])?\s*;`,
)

var protoJSONNameRe = regexp.MustCompile(`json_name\s*=\s*"([^"]+)"`)

// extractProtoShape reads a proto `message` body and returns its
// fields. Only the outermost depth is walked — nested messages
// (embedded definitions) get their own contract rows elsewhere.
func extractProtoShape(sourceLines []string, startLine, endLine int) *Shape {
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

	shape := &Shape{Kind: "message"}
	nesting := 0
	for _, raw := range strings.Split(inner, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		// Track nested message / enum blocks to skip their fields.
		if strings.HasSuffix(line, "{") {
			nesting++
			continue
		}
		if line == "}" {
			if nesting > 0 {
				nesting--
			}
			continue
		}
		if nesting > 0 {
			continue
		}
		m := protoFieldRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		mod := strings.TrimSpace(m[1])
		typeExpr := m[2]
		name := m[3]
		optionsExpr := ""
		if len(m) > 4 {
			optionsExpr = m[4]
		}
		jsonName := ""
		if optionsExpr != "" {
			if mm := protoJSONNameRe.FindStringSubmatch(optionsExpr); len(mm) > 1 {
				jsonName = mm[1]
			}
		}
		wireName := name
		if jsonName != "" {
			wireName = jsonName
		}
		repeated := strings.HasPrefix(typeExpr, "map<") || mod == "repeated"
		// proto3 non-optional scalar fields use defaults rather than
		// true "required" semantics, but from an API-contract view
		// we treat `optional` / missing-at-runtime as optional and
		// everything else as required. `required` is proto2 only.
		required := mod != "optional"
		shape.Fields = append(shape.Fields, ShapeField{
			Name:     wireName,
			Type:     typeExpr,
			JSONTag:  jsonName,
			Required: required,
			Repeated: repeated,
		})
	}
	if len(shape.Fields) == 0 {
		return nil
	}
	return shape
}

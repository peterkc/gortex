package contracts

import (
	"regexp"
	"strings"
)

// javaFieldRe captures a single Java / Kotlin field declaration:
//
//	(access)? (static|final)? (Type) (name) (= default)? ;
//	(access)? val|var name: Type = default
//
// Jackson's @JsonProperty and Bean Validation's @NotNull / @Nullable
// are consumed separately — we pick them up from the raw line window
// above the field.
var (
	javaFieldLineRe = regexp.MustCompile(
		`^\s*(?:(?:public|private|protected)\s+)?(?:(?:static|final)\s+)*([A-Za-z_][\w<>,\s.?]*)\s+(\w+)\s*(?:=\s*[^;]+)?\s*;`,
	)
	kotlinFieldLineRe = regexp.MustCompile(
		`^\s*(?:(?:public|private|protected|internal)\s+)?(?:val|var)\s+(\w+)\s*:\s*([A-Za-z_][\w<>,\s.?]*?)\s*(?:=.+)?$`,
	)
	javaJSONPropertyRe = regexp.MustCompile(`@JsonProperty\(\s*(?:value\s*=\s*)?"([^"]+)"`)
	javaNullableRe     = regexp.MustCompile(`@(?:Nullable|org\.\w+\.Nullable)\b`)
)

// extractJavaShape reads a Java / Kotlin class body and returns its
// fields. Field-level annotations (@JsonProperty, @Nullable) on the
// preceding lines are attached to the field that follows.
func extractJavaShape(sourceLines []string, startLine, endLine int) *Shape {
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
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		if strings.HasPrefix(line, "@") {
			pendingAnnotations = append(pendingAnnotations, line)
			continue
		}
		// Skip nested class / enum / method bodies (we're not
		// recursing). A line ending in `{` that doesn't also close
		// on the same line means we'd be entering a nested scope —
		// but since we only scan the outermost depth-1 body, brace
		// counting above already limits us. Inner braces appear
		// inside method bodies, which we want to skip entirely.
		// Crude but effective: if the line contains `(` that isn't
		// part of a type annotation, treat as a method and skip.
		if looksLikeJavaMethod(line) {
			pendingAnnotations = nil
			continue
		}

		// Java field
		if m := javaFieldLineRe.FindStringSubmatch(line); m != nil {
			typeExpr := strings.TrimSpace(m[1])
			name := m[2]
			if typeExpr == "" || !isJavaUserField(typeExpr) {
				pendingAnnotations = nil
				continue
			}
			jsonAlias := ""
			nullable := strings.Contains(typeExpr, "?") // Kotlin `T?` leaks into Java regex sometimes.
			for _, ann := range pendingAnnotations {
				if mm := javaJSONPropertyRe.FindStringSubmatch(ann); len(mm) > 1 {
					jsonAlias = mm[1]
				}
				if javaNullableRe.MatchString(ann) {
					nullable = true
				}
			}
			wireName := name
			if jsonAlias != "" {
				wireName = jsonAlias
			}
			shape.Fields = append(shape.Fields, ShapeField{
				Name:     wireName,
				Type:     stripJavaGenerics(typeExpr),
				JSONTag:  jsonAlias,
				Required: !nullable,
				Repeated: javaTypeIsRepeated(typeExpr),
			})
			pendingAnnotations = nil
			continue
		}

		// Kotlin field
		if m := kotlinFieldLineRe.FindStringSubmatch(line); m != nil {
			name := m[1]
			typeExpr := strings.TrimSpace(m[2])
			nullable := strings.HasSuffix(typeExpr, "?")
			typeExpr = strings.TrimSuffix(typeExpr, "?")
			jsonAlias := ""
			for _, ann := range pendingAnnotations {
				if mm := javaJSONPropertyRe.FindStringSubmatch(ann); len(mm) > 1 {
					jsonAlias = mm[1]
				}
				if javaNullableRe.MatchString(ann) {
					nullable = true
				}
			}
			wireName := name
			if jsonAlias != "" {
				wireName = jsonAlias
			}
			shape.Fields = append(shape.Fields, ShapeField{
				Name:     wireName,
				Type:     stripJavaGenerics(typeExpr),
				JSONTag:  jsonAlias,
				Required: !nullable,
				Repeated: javaTypeIsRepeated(typeExpr),
			})
			pendingAnnotations = nil
			continue
		}

		pendingAnnotations = nil
	}
	if len(shape.Fields) == 0 {
		return nil
	}
	return shape
}

// looksLikeJavaMethod detects method declarations. Heuristic: the line
// contains an open paren whose position is well before any `=` or
// terminating `;`, and the char before the paren is an identifier
// character. Covers both `public void foo()` and `void foo(` starts.
func looksLikeJavaMethod(line string) bool {
	paren := strings.Index(line, "(")
	if paren <= 0 {
		return false
	}
	// `foo(` as an expression assignment is rare at class scope;
	// methods dominate.
	prev := line[paren-1]
	if prev == ' ' {
		// `new Foo ( ...` — unlikely in class body.
		return false
	}
	return true
}

func isJavaUserField(typ string) bool {
	typ = strings.TrimSpace(typ)
	if typ == "" {
		return false
	}
	// Skip obvious keywords that appear at beginning-of-line but
	// aren't fields.
	for _, kw := range []string{"return", "if", "else", "for", "while", "switch", "try", "catch"} {
		if strings.HasPrefix(typ, kw) {
			return false
		}
	}
	return true
}

func stripJavaGenerics(typ string) string {
	typ = strings.TrimSpace(typ)
	if i := strings.Index(typ, "<"); i >= 0 {
		return strings.TrimSpace(typ[:i])
	}
	return typ
}

func javaTypeIsRepeated(typ string) bool {
	typ = strings.TrimSpace(typ)
	if strings.HasSuffix(typ, "[]") {
		return true
	}
	for _, pfx := range []string{"List<", "Set<", "Collection<", "Iterable<", "ArrayList<", "HashSet<", "LinkedList<"} {
		if strings.HasPrefix(typ, pfx) {
			return true
		}
	}
	return false
}

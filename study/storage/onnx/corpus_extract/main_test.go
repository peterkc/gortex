package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestExtractFunctionsMethodsUnicodeAndNestedBodies(t *testing.T) {
	source := `// license notice
package fixture

// Décode preserves ordinary strings for the caller.
//
// Second paragraph stays in provenance.
func Décode(value string) string {
    // secret comment
    nested := func() string { return "ordinary string" }
    return value + nested()
}

type T struct{}
/* Method documentation has more than thirty characters. */
func (t *T) Read() string { return "/* not a comment */" }
`
	file, err := extract(request{Path: "src/a.go", Source: source})
	if err != nil {
		t.Fatal(err)
	}
	rows := file.Declarations
	if !strings.Contains(file.SourceComments, "secret comment") {
		t.Fatal("file notices missing", file.SourceComments)
	}
	if len(rows) != 2 || rows[0].Name != "Décode" || rows[1].Name != "(*T).Read" || rows[1].Kind != "method" {
		t.Fatal(rows)
	}
	first := rows[0]
	if first.Line != 7 || first.EndLine != 11 || source[first.Start:first.End] != source[strings.Index(source, "func Décode"):strings.Index(source, "\n}\n")+2] {
		t.Fatal("wrong exact span", first)
	}
	if strings.Contains(first.Body, "secret comment") || !strings.Contains(first.Body, `"ordinary string"`) || !strings.Contains(first.Body, "nested := func()") {
		t.Fatal(first.Body)
	}
	if !strings.HasPrefix(first.Documentation, "Décode preserves") || first.Notice != "// license notice\n" {
		t.Fatal(first)
	}
	if first.Signature != "func Décode(value string) string" || !strings.Contains(rows[1].Body, `"/* not a comment */"`) {
		t.Fatal(rows)
	}
}

func TestExtractCRLFCommentsPreservePositions(t *testing.T) {
	source := "package f\r\nfunc F() { /* secret\r\ncomment */ println(\"text\") // c\r\n}\r\n"
	file, err := extract(request{Path: "src/f.go", Source: source})
	if err != nil {
		t.Fatal(err)
	}
	rows := file.Declarations
	body := rows[0].Body
	if strings.Contains(body, "secret") || strings.Contains(body, "comment") || len(body) != rows[0].End-rows[0].Start || !strings.Contains(body, "\r\n") {
		t.Fatal(body)
	}
}

func TestMalformedAndTrailingRequestRejectWithoutRows(t *testing.T) {
	for _, input := range []string{
		`{"path":"src/a.go","source":"package f; func broken("}`,
		`{"path":"src/a.go","source":"package f"} {}`,
		`{"path":"src/a.go","source":"package f","extra":1}`,
	} {
		var output bytes.Buffer
		if err := run(strings.NewReader(input), &output); err == nil || output.Len() != 0 {
			t.Fatal("malformed request produced output", input, output.String())
		}
	}
}

func TestPhysicalLinesIgnoreLineDirectives(t *testing.T) {
	file, err := extract(request{Path: "src/directive.go", Source: "package f\n//line fake.go:900\nfunc F() {}\n"})
	if err != nil {
		t.Fatal(err)
	}
	if file.Declarations[0].Line != 3 || file.Declarations[0].EndLine != 3 {
		t.Fatal("adjusted rather than physical source span", file.Declarations[0])
	}
}

func TestDirectiveOnlyDocumentationIsNotQueryText(t *testing.T) {
	source := `package f
//go:linkname OnlyDirectives local.symbol.with.more.than.thirty.characters
//go:nosplit
//export OnlyDirectives
//line fake.go:900
func OnlyDirectives() {}

// Ordinary prose remains documentation for the public function.
//go:noinline
func Documented() {}
`
	file, err := extract(request{Path: "src/directives.go", Source: source})
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Declarations) != 2 || file.Declarations[0].Documentation != "" {
		t.Fatal("directives became documentation", file.Declarations)
	}
	if file.Declarations[1].Documentation != "Ordinary prose remains documentation for the public function.\n" {
		t.Fatal("ordinary documentation changed", file.Declarations[1])
	}
	if !strings.Contains(file.SourceComments, "//export OnlyDirectives") {
		t.Fatal("directive provenance lost", file.SourceComments)
	}
}

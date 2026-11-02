// corpus-extract reads one frozen Go file as JSON and emits declaration spans.
package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"io"
	"os"
	"strings"
)

type request struct {
	Path   string `json:"path"`
	Source string `json:"source"`
}
type declaration struct {
	Path          string `json:"path"`
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	Start         int    `json:"start"`
	End           int    `json:"end"`
	Line          int    `json:"line"`
	EndLine       int    `json:"end_line"`
	Signature     string `json:"signature"`
	Body          string `json:"body"`
	Documentation string `json:"documentation"`
	Notice        string `json:"notice"`
}

type extraction struct {
	Declarations   []declaration `json:"declarations"`
	SourceComments string        `json:"source_comments"`
}

func extract(req request) (extraction, error) {
	source := []byte(req.Source)
	files := token.NewFileSet()
	file, err := parser.ParseFile(files, req.Path, source, parser.ParseComments|parser.AllErrors)
	if err != nil {
		return extraction{}, err
	}
	positions := files.File(file.Pos())
	blank := append([]byte(nil), source...)
	comments := make([]string, 0)
	var scan scanner.Scanner
	scan.Init(positions, source, nil, scanner.ScanComments)
	for {
		pos, kind, literal := scan.Scan()
		if kind == token.EOF {
			break
		}
		if kind != token.COMMENT {
			continue
		}
		// scanner strips CR from comment literals. Use the source delimiter
		// to find its byte span rather than the normalized literal length.
		start := positions.Offset(pos)
		end := start
		if strings.HasPrefix(literal, "//") {
			for end < len(source) && source[end] != '\n' {
				end++
			}
		} else {
			relative := strings.Index(string(source[start+2:]), "*/")
			if relative < 0 {
				return extraction{}, fmt.Errorf("unterminated comment")
			}
			end = start + 2 + relative + 2
		}
		comments = append(comments, string(source[start:end]))
		for i := start; i < end; i++ {
			if blank[i] != '\n' && blank[i] != '\r' {
				blank[i] = ' '
			}
		}
	}
	result := make([]declaration, 0)
	for _, node := range file.Decls {
		function, ok := node.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		start, end := positions.Offset(function.Pos()), positions.Offset(function.End())
		name, kind := function.Name.Name, "function"
		if function.Recv != nil {
			receiver := function.Recv.List[0].Type
			name = "(" + string(source[positions.Offset(receiver.Pos()):positions.Offset(receiver.End())]) + ")." + name
			kind = "method"
		}
		result = append(result, declaration{
			Path: req.Path, Name: name, Kind: kind, Start: start, End: end,
			Line:      positions.PositionFor(function.Pos(), false).Line,
			EndLine:   positions.PositionFor(function.End(), false).Line,
			Signature: strings.TrimRight(string(blank[start:positions.Offset(function.Body.Pos())]), " \t\r\n"),
			Body:      string(blank[start:end]), Documentation: function.Doc.Text(),
			Notice: string(source[:positions.Offset(file.Package)]),
		})
	}
	return extraction{Declarations: result, SourceComments: strings.Join(comments, "\n")}, nil
}

func run(in io.Reader, out io.Writer) error {
	decoder := json.NewDecoder(io.LimitReader(in, 32<<20))
	decoder.DisallowUnknownFields()
	var req request
	if err := decoder.Decode(&req); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("trailing request data")
	}
	if req.Path == "" {
		return fmt.Errorf("source path required")
	}
	result, err := extract(req)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}
func main() {
	if err := run(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

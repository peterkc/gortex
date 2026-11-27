package languages

import (
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// astroFrontmatterRe matches the leading `--- ... ---` code fence of an Astro
// component. Group 1 is the frontmatter body, which is always TypeScript.
var astroFrontmatterRe = regexp.MustCompile(`(?s)\A\s*---[^\n]*\r?\n(.*?)\r?\n[ \t]*---`)

// AstroExtractor extracts Astro components: a file node plus one always-exported
// component node, the leading `--- ... ---` frontmatter delegated to the
// TypeScript extractor, and any client-side <script> blocks delegated to the
// TS/JS extractor — all rebased into host-file coordinates.
type AstroExtractor struct {
	ts *TypeScriptExtractor
	js *JavaScriptExtractor
}

// NewAstroExtractor constructs an Astro component extractor.
func NewAstroExtractor() *AstroExtractor {
	return &AstroExtractor{ts: NewTypeScriptExtractor(), js: NewJavaScriptExtractor()}
}

func (e *AstroExtractor) Language() string     { return "astro" }
func (e *AstroExtractor) Extensions() []string { return []string{".astro"} }

func (e *AstroExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	return e.ExtractWithOptions(filePath, src, parser.ExtractionOptions{})
}

func (e *AstroExtractor) ExtractWithOptions(filePath string, src []byte, opts parser.ExtractionOptions) (*parser.ExtractionResult, error) {
	result := &parser.ExtractionResult{}
	lineCount := 1 + strings.Count(string(src), "\n")

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: lineCount, Language: "astro",
	}
	result.Nodes = append(result.Nodes, fileNode)

	componentName := markupComponentName(filePath)
	componentID := filePath + "::" + componentName
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: componentID, Kind: graph.KindType, Name: componentName,
		FilePath: filePath, StartLine: 1, EndLine: lineCount, Language: "astro",
		Meta: map[string]any{"component": true, "exported": true, "ui_component": "astro", "type_flavor": "component"},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: componentID, Kind: graph.EdgeDefines, FilePath: filePath, Line: 1,
	})

	// Frontmatter is always TypeScript.
	if m := astroFrontmatterRe.FindSubmatchIndex(src); m != nil {
		if contentStart, contentEnd := m[2], m[3]; contentStart >= 0 {
			lineOffset := strings.Count(string(src[:contentStart]), "\n")
			delegateInlineScriptSlice(e.ts, src[contentStart:contentEnd], lineOffset, filePath, fileNode.ID, "astro", result, opts)
		}
	}
	// Client-side <script> blocks (in addition to the frontmatter).
	carveAndDelegateScripts(src, filePath, fileNode.ID, "astro", e.ts, e.js, result, opts)
	mineTemplateComponentUsages(src, filePath, componentID, "astro", result)
	applyFrameworkTemplatePasses(src, filePath, componentID, "astro", result)
	return result, nil
}

package languages

import (
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// SvelteExtractor extracts Svelte components: a file node plus one always-exported
// component node, with every <script> / <script context="module"> block carved
// out and delegated to the TypeScript or JavaScript extractor so the component's
// logic lands in the graph at its real source lines.
type SvelteExtractor struct {
	ts *TypeScriptExtractor
	js *JavaScriptExtractor
}

// NewSvelteExtractor constructs a Svelte component extractor.
func NewSvelteExtractor() *SvelteExtractor {
	return &SvelteExtractor{ts: NewTypeScriptExtractor(), js: NewJavaScriptExtractor()}
}

func (e *SvelteExtractor) Language() string     { return "svelte" }
func (e *SvelteExtractor) Extensions() []string { return []string{".svelte"} }

func (e *SvelteExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	return e.ExtractWithOptions(filePath, src, parser.ExtractionOptions{})
}

func (e *SvelteExtractor) ExtractWithOptions(filePath string, src []byte, opts parser.ExtractionOptions) (*parser.ExtractionResult, error) {
	result := &parser.ExtractionResult{}
	lineCount := 1 + strings.Count(string(src), "\n")

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: lineCount, Language: "svelte",
	}
	result.Nodes = append(result.Nodes, fileNode)

	componentName := markupComponentName(filePath)
	componentID := filePath + "::" + componentName
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: componentID, Kind: graph.KindType, Name: componentName,
		FilePath: filePath, StartLine: 1, EndLine: lineCount, Language: "svelte",
		Meta: map[string]any{"component": true, "exported": true, "ui_component": "svelte", "type_flavor": "component"},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: componentID, Kind: graph.EdgeDefines, FilePath: filePath, Line: 1,
	})

	carveAndDelegateScripts(src, filePath, fileNode.ID, "svelte", e.ts, e.js, result, opts)
	mineTemplateComponentUsages(src, filePath, componentID, "svelte", result)
	applyFrameworkTemplatePasses(src, filePath, componentID, "svelte", result)
	// `$store` auto-subscriptions in markup resolve to the imported store.
	mineSvelteStoreSubscriptions(src, filePath, componentID, result)
	return result, nil
}

// markupComponentName derives a component name from a single-file-component path
// (Counter.svelte -> Counter, pages/index.astro -> index). Shared by the
// Svelte/Astro extractors.
func markupComponentName(filePath string) string {
	base := filepath.Base(filePath)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

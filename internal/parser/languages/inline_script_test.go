package languages

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

func TestInlineScriptDelegateOffsets(t *testing.T) {
	const script = "function greet() {\n  return 1\n}\n"
	const lineOffset = 4 // the carved block starts on host line 5 (0-based row 4)

	result := &parser.ExtractionResult{}
	delegateInlineScriptSlice(NewJavaScriptExtractor(), []byte(script), lineOffset, "App.vue", "App.vue", "vue", result, parser.ExtractionOptions{})

	var greet *graph.Node
	for _, n := range result.Nodes {
		if strings.Contains(n.ID, "#script:") && n.Kind == graph.KindFile {
			t.Errorf("synthetic virtual file node was not dropped: %s", n.ID)
		}
		if n.Name == "greet" {
			greet = n
		}
	}
	if greet == nil {
		t.Fatalf("delegated function 'greet' was not extracted; got %d nodes", len(result.Nodes))
	}
	if greet.FilePath != "App.vue" {
		t.Errorf("FilePath = %q, want App.vue", greet.FilePath)
	}
	if greet.Language != "vue" {
		t.Errorf("Language = %q, want vue (relabeled)", greet.Language)
	}
	if greet.StartLine != 1+lineOffset {
		t.Errorf("StartLine = %d, want %d (1-based slice line + offset)", greet.StartLine, 1+lineOffset)
	}
	if greet.Meta["inline_script"] != true {
		t.Errorf("Meta[inline_script] = %v, want true", greet.Meta["inline_script"])
	}

	// No node or edge may still reference the synthetic virtual file id.
	const virtual = "App.vue#script:5"
	for _, e := range result.Edges {
		if e.From == virtual {
			t.Errorf("an edge still points from the virtual file node: %+v", e)
		}
		if e.FilePath != "" && e.FilePath != "App.vue" {
			t.Errorf("edge FilePath not rebased to host: %q", e.FilePath)
		}
	}

	// Whitespace-only content is a no-op.
	empty := &parser.ExtractionResult{}
	delegateInlineScriptSlice(NewJavaScriptExtractor(), []byte("   \n  "), 0, "x.vue", "x.vue", "vue", empty, parser.ExtractionOptions{})
	if len(empty.Nodes) != 0 || len(empty.Edges) != 0 {
		t.Errorf("whitespace script should be a no-op, got %d nodes %d edges", len(empty.Nodes), len(empty.Edges))
	}
}

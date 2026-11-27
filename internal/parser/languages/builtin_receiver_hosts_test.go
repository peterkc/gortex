package languages

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

func TestBuiltinReceiverFactsInInlineScriptHosts(t *testing.T) {
	for _, tc := range []struct {
		ext                 parser.Extractor
		file, tag, delegate string
	}{
		{NewVueExtractor(), "View.vue", `<script setup lang="ts">`, "typescript"},
		{NewSvelteExtractor(), "View.svelte", `<script>`, "javascript"},
		{NewAstroExtractor(), "View.astro", `<script lang="ts">`, "typescript"},
		{NewHTMLExtractor(), "view.html", `<script>`, "javascript"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			src := []byte("<!-- host -->\n" + tc.tag + "\nfunction run() {\n  [1].slice(0);\n}\n</script>\n")
			check := func(opts parser.ExtractionOptions) *graph.Edge {
				t.Helper()
				result, err := parser.Extract(tc.ext, tc.file, src, opts)
				require.NoError(t, err)
				defer result.ReleaseTree()
				var calls []*graph.Edge
				for _, e := range result.Edges {
					if e.Kind == graph.EdgeCalls && e.To == "unresolved::*.slice" {
						calls = append(calls, e)
					}
				}
				require.Len(t, calls, 1)
				return calls[0]
			}
			raw := check(parser.ExtractionOptions{CaptureBuiltinReceiverFacts: true})
			hint, ok := raw.BuiltinReceiverHint()
			require.True(t, ok)
			require.Equal(t, tc.delegate, hint.Language)
			require.Equal(t, "array", hint.Family)
			require.Equal(t, "instance", hint.Form)
			require.Equal(t, tc.file, raw.FilePath)
			require.Equal(t, 4, raw.Line)
			require.NotContains(t, raw.Meta, graph.MetaLinkStatus)
			native := check(parser.ExtractionOptions{})
			_, ok = native.BuiltinReceiverHint()
			require.False(t, ok)
			require.Equal(t, graph.EdgeIdentityFor(native), graph.EdgeIdentityFor(raw))
		})
	}
}

package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestPythonReuseKeepsFreshCallStubWithoutConfirmation(t *testing.T) {
	prior := &graph.Edge{Meta: map[string]any{
		"confirmed_from_origin": "ast",
		"semantic_source":       "python-lsp",
		"python_call_stub":      "unresolved::extern::PIL.image.f::f",
	}}
	captured := reuseSemanticMeta(prior)
	values := map[reuseKey]*reuseVal{{}: {semanticMeta: captured}}
	stripReuseSemanticMeta(values)
	require.Equal(t, map[string]any{"python_call_stub": "unresolved::extern::PIL.image.f::f"}, captured)
	fresh := &graph.Edge{To: "provider/PIL/image.py::f"}
	applyReuseSemanticMeta(fresh, captured, "unresolved::extern::PIL.image::f")
	require.Equal(t, "unresolved::extern::PIL.image::f", fresh.Meta["python_call_stub"])
	require.NotContains(t, fresh.Meta, "semantic_source")
	require.NotContains(t, fresh.Meta, "confirmed_from_origin")
}

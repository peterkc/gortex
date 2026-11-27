package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestGDAutoloadDeclKind(t *testing.T) {
	n := &graph.Node{
		Name: "Clock", Kind: graph.KindType, Language: "godot_project",
		Meta: map[string]any{"gd_kind": "autoload", "gd_script": "src/Clock.gd"},
	}
	name, script := gdAutoloadDecl(n)
	require.Equal(t, "Clock", name)
	require.Equal(t, "src/Clock.gd", script)
	n.Kind = graph.KindVariable
	name, script = gdAutoloadDecl(n)
	require.Empty(t, name)
	require.Empty(t, script)
}

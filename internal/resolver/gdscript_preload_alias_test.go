package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// gdAliasConst declares `const <alias> = preload("<path>")` in a script.
func gdAliasConst(g *graph.Graph, file, alias, path string) {
	g.AddNode(&graph.Node{
		ID: file + "::" + alias, Kind: graph.KindVariable, Name: alias,
		FilePath: file, Language: "gdscript",
		Meta: map[string]any{"const": true, "gd_preload": path},
	})
}

func gdPreloadGraph(t *testing.T) *graph.Graph {
	t.Helper()
	g := graph.New()
	gdScriptFile(g, "src/util/Maths.gd", "clampf")
	gdScriptFile(g, "src/ui/Hud.gd", "draw")
	gdAliasConst(g, "src/ui/Hud.gd", "M", "res://src/util/Maths.gd")
	return g
}

// A script with no `class_name` is reached by the alias its callers
// declare for it. The alias is file-local — the same name one directory
// over means a different script — so nothing but the const's own declared
// path connects the two.
func TestResolveGDScriptPreloadAliases_BindsAliasedMember(t *testing.T) {
	g := gdPreloadGraph(t)
	gdRecvCall(g, "src/ui/Hud.gd::draw", "src/ui/Hud.gd", "clampf", "M")

	require.Equal(t, 1, ResolveGDScriptPreloadAliases(g))
	e := gdCallEdge(g, "src/ui/Hud.gd::draw", "src/util/Maths.gd::clampf")
	require.NotNil(t, e)
	assert.Equal(t, graph.OriginASTResolved, e.Origin)
	assert.Equal(t, SynthGodotPreloadAlias, e.Meta[MetaSynthesizedBy])
}

// The alias belongs to the file that declares it. Another script using
// the same name for something else must not ride it.
func TestResolveGDScriptPreloadAliases_IsFileLocal(t *testing.T) {
	g := gdPreloadGraph(t)
	gdScriptFile(g, "src/ui/Other.gd", "paint")
	gdRecvCall(g, "src/ui/Other.gd::paint", "src/ui/Other.gd", "clampf", "M")

	assert.Zero(t, ResolveGDScriptPreloadAliases(g))
	assert.Nil(t, gdCallEdge(g, "src/ui/Other.gd::paint", "src/util/Maths.gd::clampf"))
}

// A member the loaded script does not declare is left unresolved rather
// than bound to the file — the alias proves which script, not which name.
func TestResolveGDScriptPreloadAliases_UnknownMemberStaysUnresolved(t *testing.T) {
	g := gdPreloadGraph(t)
	gdRecvCall(g, "src/ui/Hud.gd::draw", "src/ui/Hud.gd", "missing", "M")

	assert.Zero(t, ResolveGDScriptPreloadAliases(g))
	assert.NotNil(t, gdCallEdge(g, "src/ui/Hud.gd::draw", "unresolved::*.missing"))
}

// A path matching two indexed scripts names neither.
func TestResolveGDScriptPreloadAliases_AmbiguousPathRefuses(t *testing.T) {
	g := gdPreloadGraph(t)
	gdScriptFile(g, "vendor/src/util/Maths.gd", "clampf")
	gdRecvCall(g, "src/ui/Hud.gd::draw", "src/ui/Hud.gd", "clampf", "M")

	assert.Zero(t, ResolveGDScriptPreloadAliases(g))
}

func TestResolveGDScriptPreloadAliases_NoAliasesIsANoOp(t *testing.T) {
	g := graph.New()
	gdScriptFile(g, "src/ui/Hud.gd", "draw")
	assert.Zero(t, ResolveGDScriptPreloadAliases(g))
}

// The preload turn sees only the residual Call left by autoload.
func TestResolveGDScriptPreloadAliases_AfterAutoload(t *testing.T) {
	g := gdPreloadGraph(t)
	const caller = "src/ui/Hud.gd::draw"
	gdScriptFile(g, "src/core/Game.gd", "clampf")
	gdAutoloadNode(g, "M", "src/core/Game.gd")
	gdRecvCall(g, caller, "src/ui/Hud.gd", "clampf", "M")

	require.Equal(t, 1, ResolveGDScriptAutoloads(g))
	assert.Zero(t, ResolveGDScriptPreloadAliases(g))
	e := gdCallEdge(g, caller, "src/core/Game.gd::clampf")
	require.NotNil(t, e)
	assert.Equal(t, SynthGodotAutoload, e.Meta[MetaSynthesizedBy])
	assert.Nil(t, gdCallEdge(g, caller, "src/util/Maths.gd::clampf"))
}

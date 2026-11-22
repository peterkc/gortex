package resolver

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildGoExternalAttributionMaterializesSavedTargets(t *testing.T) {
	const (
		goCaller     = "repo/main.go::Run"
		otherCaller  = "other/main.go::Run"
		tsCaller     = "repo/client.ts::Client"
		recording    = "repo/generated.go" // No file node or parse receipt for this recording path.
		upper        = "repo/record.GO"
		clientPath   = "example.test/sdk/client"
		clientModule = "module::go:" + clientPath
		dep          = "dep::" + clientPath + "::Dial"
		external     = "external::" + clientPath + "::Lookup"
		stdlibA      = "repo::stdlib::encoding/json::Marshal"
		stdlibB      = "other::stdlib::encoding/json::Marshal"
		keptSymbol   = "repo::stdlib::encoding/json::Unmarshal"
		stdModuleA   = "repo::module::go:encoding/json"
		stdModuleB   = "other::module::go:encoding/json"
		toolsPath    = "example.test/tools"
		pinnedPath   = "example.test/pinned"
	)
	uppercaseFile := &graph.Node{ID: upper, Kind: graph.KindFile, Name: "record.GO", FilePath: upper, Language: "go", RepoPrefix: "repo"}
	weak := rebuildGoExternalModule(clientModule, "old client", "", "external", clientPath)
	weak.QualName = "replace weak payload"
	weak.Meta["payload"] = "remove me on upgrade"
	equal := rebuildGoExternalModule(stdModuleA, "old json", "repo", "stdlib", "encoding/json")
	equal.Meta["payload"] = "preserve equal role"
	strong := rebuildGoExternalModule("module::go:"+pinnedPath, "old pinned", "", "dep", pinnedPath)
	strong.Meta["payload"] = "preserve stronger role"
	incumbent := rebuildGoExternalSymbol(keptSymbol, "Unmarshal", "repo", "stdlib", "encoding/json")
	incumbent.QualName = "preserve existing target"
	stableNodes := []*graph.Node{
		uppercaseFile,
		{ID: goCaller, Kind: graph.KindFunction, Name: "Run", FilePath: "repo/main.go", Language: "go", RepoPrefix: "repo"},
		{ID: otherCaller, Kind: graph.KindFunction, Name: "Run", FilePath: "other/main.go", Language: "go", RepoPrefix: "other"},
		{ID: tsCaller, Kind: graph.KindFunction, Name: "Client", FilePath: "repo/client.ts", Language: "typescript", RepoPrefix: "repo"},
		equal, strong, incumbent,
	}
	seed := append(slices.Clone(stableNodes), weak)
	wantNodes := append(slices.Clone(stableNodes),
		rebuildGoExternalModule(clientModule, "client", "", "dep", clientPath),
		rebuildGoExternalModule(stdModuleB, "json", "other", "stdlib", "encoding/json"),
		rebuildGoExternalModule("module::go:"+toolsPath, "tools", "", "external", toolsPath),
		rebuildGoExternalSymbol(dep, "Dial", "", "dep", clientPath),
		rebuildGoExternalSymbol(external, "Lookup", "", "external", clientPath),
		rebuildGoExternalSymbol(stdlibA, "Marshal", "repo", "stdlib", "encoding/json"),
		rebuildGoExternalSymbol(stdlibB, "Marshal", "other", "stdlib", "encoding/json"),
	)

	var input []*graph.Edge
	for i, row := range []struct {
		from, to, file string
		kind           graph.EdgeKind
		payload        string
	}{
		{tsCaller, dep, recording, graph.EdgeArgOf, "Go recording even without a Go caller or receipt"},
		{goCaller, external, recording, graph.EdgeReferences, "first external reference"},
		{goCaller, external, recording, graph.EdgeReferences, "second external reference"},
		{goCaller, stdlibA, recording, graph.EdgeCalls, "repo-specific stdlib"},
		{otherCaller, stdlibB, "other/main.go", graph.EdgeCalls, "other repo's stdlib"},
		{goCaller, keptSymbol, recording, graph.EdgeCalls, "existing symbol"},
		{goCaller, "external::" + toolsPath, recording, graph.EdgeReferences, "package only"},
		{goCaller, "external::" + pinnedPath, recording, graph.EdgeReferences, "weaker than existing module"},
		{goCaller, "external::example.test/upper::Use", upper, graph.EdgeCalls, "case-sensitive suffix"},
		{goCaller, "external::example.test/backup::Use", recording + ".bak", graph.EdgeCalls, "exact suffix"},
		{goCaller, "external::example.test/import-only", recording, graph.EdgeImports, "disallowed kind"},
		{keptSymbol, stdModuleA, "repo/old-site.go", graph.EdgeMemberOf, "keep exact membership at old site"},
		{external, clientModule, recording, graph.EdgeReferences, "same endpoints but not membership"},
	} {
		input = append(input, &graph.Edge{From: row.from, To: row.to, FilePath: row.file, Line: i + 1, Kind: row.kind,
			Confidence: 0.75, ConfidenceLabel: "confirmed", Origin: "fixture", Tier: "semantic",
			Meta: map[string]any{"payload": row.payload}})
	}
	wantEdges := append(slices.Clone(input),
		&graph.Edge{From: dep, To: clientModule, Kind: graph.EdgeMemberOf, Origin: graph.OriginASTResolved},
		&graph.Edge{From: external, To: clientModule, Kind: graph.EdgeMemberOf, Origin: graph.OriginASTResolved},
		&graph.Edge{From: stdlibA, To: stdModuleA, Kind: graph.EdgeMemberOf, Origin: graph.OriginASTResolved},
		&graph.Edge{From: stdlibB, To: stdModuleB, Kind: graph.EdgeMemberOf, Origin: graph.OriginASTResolved},
	)

	build := openRebuildBareScopeFixture(t)
	// A real Go file node and saved receipt do not admit a non-.go recording path.
	require.NoError(t, build.AddParsedFile("repo", store_pebble.ParseFacts{
		File:  graph.FileMetaRow{FilePath: upper, NodeCount: 1, ContentHash: "upper-receipt"},
		Nodes: []*graph.Node{uppercaseFile},
	}))
	build.AddBatch(seed[1:], input)
	requireRebuildGoExternalAttribution(t, build, seed, input, wantNodes, wantEdges)
}

func TestRebuildGoExternalAttributionRequiresNamedGo(t *testing.T) {
	const file = "repo/main.go"
	seed := []*graph.Node{{ID: file, Kind: graph.KindFile, FilePath: file, Language: "go", RepoPrefix: "repo"}}
	input := []*graph.Edge{{From: file, To: "dep::example.test/sdk::Run", Kind: graph.EdgeCalls, FilePath: file, Line: 3}}
	build := openRebuildBareScopeFixture(t)
	build.AddBatch(seed, input)
	requireRebuildGoExternalAttribution(t, build, seed, input, seed, input)
}

func TestRebuildGoExternalAttributionNativeGraphPrefix(t *testing.T) {
	const file, source = "site.go", "site.go::F"
	const stdlib, collision = "dep::stdlib::fmt::Println", "dep::module::go:fmt"
	const depModule, stalePath = "dep::module::go:module", "example.test/stale"
	const staleID = "module::go:" + stalePath
	base := &graph.Node{ID: source, Kind: graph.KindFunction, Name: "F", FilePath: file, Language: "go"}
	input := []*graph.Edge{
		{From: source, To: stdlib, Kind: graph.EdgeCalls, FilePath: file, Line: 1},
		{From: source, To: collision, Kind: graph.EdgeCalls, FilePath: file, Line: 2},
		{From: source, To: "dep::" + stalePath, Kind: graph.EdgeReferences, FilePath: file, Line: 3},
	}
	wantEdges := append(slices.Clone(input),
		&graph.Edge{From: stdlib, To: collision, Kind: graph.EdgeMemberOf, Origin: graph.OriginASTResolved},
		&graph.Edge{From: collision, To: depModule, Kind: graph.EdgeMemberOf, Origin: graph.OriginASTResolved},
	)
	for _, present := range []bool{false, true} {
		name := "absent_at_entry"
		if present {
			name = "present_at_entry"
		}
		t.Run(name, func(t *testing.T) {
			seed := func() []*graph.Node {
				nodes := []*graph.Node{base, rebuildGoExternalModule(staleID, "old", "stale-repo", "external", stalePath)}
				if present {
					nodes = append(nodes, rebuildGoExternalModule(collision, "old fmt", "dep", "external", "fmt"))
				}
				return nodes
			}
			want := func(collisionRepo, staleRepo string) []*graph.Node {
				shared := rebuildGoExternalSymbol(collision, "go:fmt", collisionRepo, "dep", "module")
				if present {
					shared = rebuildGoExternalModule(collision, "fmt", "dep", "stdlib", "fmt")
				}
				return []*graph.Node{base, shared,
					rebuildGoExternalModule(depModule, "module", "dep", "dep", "module"),
					rebuildGoExternalModule(staleID, "stale", staleRepo, "dep", stalePath),
					rebuildGoExternalSymbol(stdlib, "Println", "dep", "stdlib", "fmt"),
				}
			}
			// Native Graph keeps the prior nonempty prefix on an empty-prefix
			// write. In the absent case, the earlier module creates that prior.
			g := graph.New()
			g.AddBatch(seed(), input)
			New(g).attributeGoExternalCalls()
			assertNodes := want("dep", "stale-repo")
			require.ElementsMatch(t, assertNodes, g.AllNodes())
			require.ElementsMatch(t, wantEdges, g.AllEdges())

			sql := openRebuildGoBuiltinSQLite(t)
			require.NoError(t, sql.AddBatchChecked(seed(), input))
			New(sql).attributeGoExternalCalls()
			require.ElementsMatch(t, want("", ""), sql.AllNodes())
			require.ElementsMatch(t, wantEdges, sql.AllEdges())

			build := openRebuildBareScopeFixture(t)
			build.AddBatch(seed(), input)
			require.NoError(t, runRebuildGoExternalCalls(t.Context(), build))
			require.ElementsMatch(t, assertNodes, rebuildGoExternalSavedNodes(t, build))
			require.ElementsMatch(t, wantEdges, rebuildFnPointerSaved(t, build))
		})
	}
}

func TestRebuildGoExternalAttributionNULRecordingPath(t *testing.T) {
	const sqlOnly, graphOnly = "dep::example.test/sql::Use", "dep::example.test/graph::Use"
	nodes := []*graph.Node{{ID: "F", Kind: graph.KindFunction, Name: "F", Language: "go"}}
	edges := []*graph.Edge{
		{From: "F", To: sqlOnly, Kind: graph.EdgeCalls, FilePath: "site.go\x00tail", Line: 1},
		{From: "F", To: graphOnly, Kind: graph.EdgeCalls, FilePath: "site\x00.go", Line: 2},
	}
	g := graph.New()
	g.AddBatch(nodes, edges)
	sql := openRebuildGoBuiltinSQLite(t)
	require.NoError(t, sql.AddBatchChecked(nodes, edges))
	// Exercise the pinned SQLite driver and actual discovery with both
	// saved spellings, not the host CLI or a substituted SELECT predicate.
	require.ElementsMatch(t, edges, sql.AllEdges())
	require.Equal(t, []string{sqlOnly}, sql.DistinctExternalTargets(goExternalAttribKinds, ".go"))
	require.Equal(t, []string{graphOnly}, g.DistinctExternalTargets(goExternalAttribKinds, ".go"))

	build := openRebuildBareScopeFixture(t)
	ordinary := &graph.Edge{From: "A", To: "dep::example.test/ordinary::Use", Kind: graph.EdgeCalls, FilePath: "ordinary.go", Line: 1}
	input := append([]*graph.Edge{ordinary}, edges...)
	build.AddBatch(nodes, input)
	err := runRebuildGoExternalCalls(t.Context(), build)
	require.ErrorIs(t, err, ErrRebuildGoExternalRecordingPath)
	require.Equal(t, nodes, rebuildGoExternalSavedNodes(t, build))
	require.ElementsMatch(t, input, rebuildFnPointerSaved(t, build))
}

func rebuildGoExternalModule(id, name, repo, role, importPath string) *graph.Node {
	return &graph.Node{ID: id, Kind: graph.KindModule, Name: name, Language: "go", RepoPrefix: repo,
		Meta: map[string]any{"ecosystem": "go", "role": role, "import_path": importPath}}
}

func rebuildGoExternalSymbol(id, name, repo, role, importPath string) *graph.Node {
	return &graph.Node{ID: id, Kind: graph.KindFunction, Name: name, Language: "go", RepoPrefix: repo,
		Meta: map[string]any{"external": true, "module_role": role, "module_path": importPath}}
}

func requireRebuildGoExternalAttribution(t *testing.T, build *store_pebble.BuildStore, seed []*graph.Node, input []*graph.Edge, wantNodes []*graph.Node, wantEdges []*graph.Edge) {
	t.Helper()
	sql := openRebuildGoBuiltinSQLite(t)
	require.NoError(t, sql.AddBatchChecked(seed, input))
	require.ElementsMatch(t, seed, sql.AllNodes(), "SQLite input nodes")
	require.ElementsMatch(t, input, sql.AllEdges(), "SQLite input edges")
	require.ElementsMatch(t, seed, rebuildGoExternalSavedNodes(t, build), "Pebble input nodes")
	require.ElementsMatch(t, input, rebuildFnPointerSaved(t, build), "Pebble input edges")

	New(sql).attributeGoExternalCalls()
	nativeNodes, nativeEdges := sql.AllNodes(), sql.AllEdges()
	require.ElementsMatch(t, wantNodes, nativeNodes, "SQLite output nodes")
	require.ElementsMatch(t, wantEdges, nativeEdges, "SQLite output edges")

	require.NoError(t, runRebuildGoExternalCalls(t.Context(), build))
	require.ElementsMatch(t, nativeNodes, rebuildGoExternalSavedNodes(t, build), "Pebble output nodes")
	require.ElementsMatch(t, nativeEdges, rebuildFnPointerSaved(t, build), "Pebble output edges")
	_, err := build.OpenSnapshot()
	require.ErrorIs(t, err, store_pebble.ErrUnpublishedBuild)
}

func rebuildGoExternalSavedNodes(t *testing.T, build *store_pebble.BuildStore) []*graph.Node {
	t.Helper()
	var nodes []*graph.Node
	require.NoError(t, build.RebuildView().VisitNodes(t.Context(), func(n *graph.Node) error {
		nodes = append(nodes, n)
		return nil
	}))
	return nodes
}

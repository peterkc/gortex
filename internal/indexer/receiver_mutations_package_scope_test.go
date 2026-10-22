package indexer

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	store_sqlite "github.com/zzet/gortex/internal/graph/store_sqlite"
)

func receiverScopeNode(id string, kind graph.NodeKind, name, file, language, scope string) *graph.Node {
	return &graph.Node{ID: id, Kind: kind, Name: name, FilePath: file, Language: language,
		Meta: map[string]any{"receiver": "Node", "scope_pkg": scope}}
}

func checkReceiverScopePaths(t *testing.T, nodes []*graph.Node, edges []*graph.Edge, want []indirectMutSpec) {
	t.Helper()
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.AddBatch(nodes, edges)
	g := graph.New()
	g.AddBatch(nodes, edges)
	var methods []*graph.Node
	for _, n := range nodes {
		if n.Kind == graph.KindMethod {
			methods = append(methods, n)
		}
	}
	sortIndirectMutationSpecs(want)
	for _, name := range []string{"projected", "graph", "scoped-graph", "scoped-sqlite"} {
		t.Run(name, func(t *testing.T) {
			var got []indirectMutSpec
			switch name {
			case "projected":
				got = indirectMutationEdges(s)
			case "graph":
				got = indirectMutationEdges(g)
			case "scoped-graph":
				got, _ = indirectMutationEdgesForMethods(g, methods)
			case "scoped-sqlite":
				got, _ = indirectMutationEdgesForMethods(s, methods)
			}
			sortIndirectMutationSpecs(got)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("mutations:\n got %#v\nwant %#v", got, want)
			}
		})
	}
}

func TestReceiverMutationsPackageScope(t *testing.T) {
	var nodes []*graph.Node
	var edges []*graph.Edge
	var want []indirectMutSpec
	for _, folder := range []string{"a", "b"} {
		file := folder + "/type.go"
		for _, method := range []string{"Set", "Wrap", "Append", "Cross", "BadSeed", "BadWrap"} {
			nodes = append(nodes, receiverScopeNode(folder+method, graph.KindMethod, method, file, "go", ""))
		}
		nodes = append(nodes, receiverScopeNode(folder+"field", graph.KindField, "inner", file, "go", ""))
		other := "a"
		if folder == "a" {
			other = "b"
		}
		edges = append(edges,
			&graph.Edge{From: folder + "Set", To: folder + "field", Kind: graph.EdgeWrites},
			&graph.Edge{From: folder + "Wrap", To: folder + "Set", Kind: graph.EdgeCalls, Meta: map[string]any{"recv_self": true}},
			&graph.Edge{From: folder + "Append", To: "external::Lock", Kind: graph.EdgeCalls, Meta: map[string]any{"recv_field": "inner"}},
			&graph.Edge{From: folder + "Cross", To: other + "Set", Kind: graph.EdgeCalls, Meta: map[string]any{"recv_self": true}},
			&graph.Edge{From: folder + "BadSeed", To: other + "field", Kind: graph.EdgeWrites},
			&graph.Edge{From: folder + "BadWrap", To: folder + "BadSeed", Kind: graph.EdgeCalls, Meta: map[string]any{"recv_self": true}},
		)
		want = append(want, indirectMutSpec{from: folder + "Wrap", to: folder + "field", via: "Set"},
			indirectMutSpec{from: folder + "Append", to: folder + "field", via: "Lock"})
	}
	checkReceiverScopePaths(t, nodes, edges, want)
}

func TestReceiverMutationsScopePkgAndLanguage(t *testing.T) {
	for _, tc := range []struct {
		name, methodLanguage, fieldLanguage, methodScope, fieldScope string
		match                                                        bool
	}{
		{"shared-jvm-package", "java", "java", "com.example", "com.example", true},
		{"other-language", "java", "kotlin", "com.example", "com.example", false},
		{"other-package", "java", "java", "com.example", "com.other", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodes := []*graph.Node{
				receiverScopeNode("caller", graph.KindMethod, "Caller", "test/Node.java", tc.methodLanguage, tc.methodScope),
				receiverScopeNode("field", graph.KindField, "inner", "main/Node.java", tc.fieldLanguage, tc.fieldScope),
			}
			edges := []*graph.Edge{{From: "caller", To: "external::Lock", Kind: graph.EdgeCalls, Meta: map[string]any{"recv_field": "inner"}}}
			var want []indirectMutSpec
			if tc.match {
				want = []indirectMutSpec{{from: "caller", to: "field", via: "Lock"}}
			}
			checkReceiverScopePaths(t, nodes, edges, want)
		})
	}
}

func TestReceiverMutationsOtherLanguageKeepsReceiverMatch(t *testing.T) {
	nodes := []*graph.Node{
		receiverScopeNode("set", graph.KindMethod, "Set", "main/Node.swift", "swift", ""),
		receiverScopeNode("wrap", graph.KindMethod, "Wrap", "extensions/Node.swift", "swift", ""),
		receiverScopeNode("append", graph.KindMethod, "Append", "extensions/Node.swift", "swift", ""),
		receiverScopeNode("field", graph.KindField, "inner", "main/Node.swift", "swift", ""),
	}
	for _, node := range nodes {
		delete(node.Meta, "scope_pkg")
	}
	edges := []*graph.Edge{
		{From: "set", To: "field", Kind: graph.EdgeWrites},
		{From: "wrap", To: "set", Kind: graph.EdgeCalls, Meta: map[string]any{"recv_self": true}},
		{From: "append", To: "external::Lock", Kind: graph.EdgeCalls, Meta: map[string]any{"recv_field": "inner"}},
	}
	want := []indirectMutSpec{
		{from: "wrap", to: "field", via: "Set"},
		{from: "append", to: "field", via: "Lock"},
	}
	checkReceiverScopePaths(t, nodes, edges, want)
}

func TestReceiverMutationsNoScopeNoEdge(t *testing.T) {
	for _, methodFile := range []string{"type.go", ""} {
		t.Run("method-file="+methodFile, func(t *testing.T) {
			nodes := []*graph.Node{
				receiverScopeNode("caller", graph.KindMethod, "Caller", methodFile, "go", ""),
				receiverScopeNode("field", graph.KindField, "inner", "", "go", ""),
			}
			edges := []*graph.Edge{{From: "caller", To: "external::Lock", Kind: graph.EdgeCalls, Meta: map[string]any{"recv_field": "inner"}}}
			checkReceiverScopePaths(t, nodes, edges, nil)
		})
	}
}

func TestReceiverMutationsRepositoryScope(t *testing.T) {
	for _, language := range []string{"go", "java", "swift"} {
		for _, tc := range []struct {
			name, methodRepo, fieldRepo string
			match                       bool
		}{
			{"same-repo", "a", "a", true},
			{"other-repo", "a", "b", false},
			{"single-repo", "", "", true},
			{"unprefixed-field", "a", "", false},
		} {
			t.Run(language+"/"+tc.name, func(t *testing.T) {
				scope := ""
				if language == "java" {
					scope = "com.example"
				}
				caller := receiverScopeNode("caller", graph.KindMethod, "Caller", filepath.Join(tc.methodRepo, "src", "type"), language, scope)
				caller.RepoPrefix = tc.methodRepo
				field := receiverScopeNode("field", graph.KindField, "inner", filepath.Join(tc.fieldRepo, "src", "type"), language, scope)
				field.RepoPrefix = tc.fieldRepo
				edges := []*graph.Edge{{From: caller.ID, To: "external::Lock", Kind: graph.EdgeCalls, Meta: map[string]any{"recv_field": "inner"}}}
				var want []indirectMutSpec
				if tc.match {
					want = []indirectMutSpec{{from: caller.ID, to: field.ID, via: "Lock"}}
				}
				checkReceiverScopePaths(t, []*graph.Node{caller, field}, edges, want)
			})
		}
	}
}

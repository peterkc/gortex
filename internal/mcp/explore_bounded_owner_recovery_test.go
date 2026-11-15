package mcp

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

type legacyOnlyExploreReader struct{ graph.Reader }

func TestExploreCausalChangeOwnerHonorsExactFileCapScopeAndOverlay(t *testing.T) {
	build := func(rows int) (*graph.Graph, *graph.Node, *graph.Node) {
		memory := graph.New()
		const path = "src/Builder.php"
		leaf := divergentDefaultTestNode(path+"::Builder.build", graph.KindMethod, "build", path, "function build(): Owner")
		owner := divergentDefaultTestNode(path+"::Owner", graph.KindType, "Owner", path, "class Owner")
		nodes := []*graph.Node{leaf, owner}
		for index := len(nodes); index < rows; index++ {
			nodes = append(nodes, divergentDefaultTestNode(
				fmt.Sprintf("%s::noise-%03d", path, index), graph.KindVariable, "noise", path, "",
			))
		}
		memory.AddBatch(nodes, nil)
		return memory, leaf, owner
	}

	memory, leaf, owner := build(exploreCausalChangeFileNodeCap)
	got := exploreCausalChangeOwner(context.Background(), "change Owner builder", leaf, memory, graph.LocalizationNodeScope{})
	if got == nil || got.ID != owner.ID {
		t.Fatalf("exact-cap owner = %#v, want %s", got, owner.ID)
	}
	if got := exploreCausalChangeOwner(
		context.Background(), "change Owner builder", leaf, memory,
		graph.LocalizationNodeScope{WorkspaceID: "other"},
	); got != nil {
		t.Fatalf("out-of-scope owner was promoted: %#v", got)
	}
	layer := graph.NewOverlayLayer()
	layer.MarkFile(leaf.FilePath, true)
	if got := exploreCausalChangeOwner(
		context.Background(), "change Owner builder", leaf,
		graph.NewOverlaidView(memory, layer), graph.LocalizationNodeScope{},
	); got != nil {
		t.Fatalf("overlay tombstone reintroduced durable owner: %#v", got)
	}

	overflow, overflowLeaf, _ := build(exploreCausalChangeFileNodeCap + 1)
	if got := exploreCausalChangeOwner(
		context.Background(), "change Owner builder", overflowLeaf, overflow, graph.LocalizationNodeScope{},
	); got != nil {
		t.Fatalf("97-row causal file produced partial owner evidence: %#v", got)
	}
	if got := exploreCausalChangeOwner(
		context.Background(), "change Owner builder", leaf,
		legacyOnlyExploreReader{Reader: memory}, graph.LocalizationNodeScope{},
	); got != nil {
		t.Fatalf("reader without bounded file capability used a legacy fallback: %#v", got)
	}
}

func buildDivergentRankedCallableFiles(rowsPerFile []int) (*graph.Graph, []exploreTarget) {
	memory := graph.New()
	targets := make([]exploreTarget, 0, len(rowsPerFile))
	for fileIndex, rows := range rowsPerFile {
		path := fmt.Sprintf("src/Owner%d.php", fileIndex)
		ownerName := fmt.Sprintf("Owner%d", fileIndex)
		owner := divergentDefaultTestNode(path+"::"+ownerName, graph.KindType, ownerName, path, "class "+ownerName)
		callable := divergentDefaultTestNode(path+"::"+ownerName+".permissionHandler", graph.KindMethod, "permissionHandler", path, "function permissionHandler()")
		constructor := divergentDefaultTestNode(path+"::"+ownerName+".__construct", graph.KindMethod, "__construct", path, "function __construct($filePermission = null)")
		nodes := []*graph.Node{owner, callable, constructor}
		for index := len(nodes); index < rows; index++ {
			nodes = append(nodes, divergentDefaultTestNode(
				fmt.Sprintf("%s::noise-%03d", path, index), graph.KindVariable, "noise", path, "",
			))
		}
		memory.AddBatch(nodes, nil)
		targets = append(targets, exploreTarget{node: callable})
	}
	return memory, targets
}

func TestExploreDivergentDefaultRankedFilesHonorPerFileAndTotalCaps(t *testing.T) {
	terms := exploreTerminalTerms("permission handler")
	for _, test := range []struct {
		name         string
		rows         []int
		wantBases    int
		wantComplete bool
	}{
		{name: "64 row file", rows: []int{64}, wantBases: 1, wantComplete: true},
		{name: "65 row file", rows: []int{65}},
		{name: "96 rows total", rows: []int{32, 32, 32}, wantBases: 3, wantComplete: true},
		{name: "97 rows total", rows: []int{33, 32, 32}},
	} {
		t.Run(test.name, func(t *testing.T) {
			memory, targets := buildDivergentRankedCallableFiles(test.rows)
			bases, complete := exploreDivergentDefaultBasesFromRankedCallables(
				context.Background(), terms, targets, memory, graph.LocalizationNodeScope{}, time.Now().Add(30*time.Second),
			)
			if complete != test.wantComplete || len(bases) != test.wantBases {
				t.Fatalf("bases/complete = %d/%v, want %d/%v", len(bases), complete, test.wantBases, test.wantComplete)
			}
		})
	}
}

func divergentProjectionBase(index int) exploreDivergentDefaultBase {
	path := fmt.Sprintf("src/Base%d.php", index)
	ownerName := fmt.Sprintf("Base%d", index)
	return exploreDivergentDefaultBase{
		constructor: divergentDefaultTestNode(path+"::"+ownerName+".__construct", graph.KindMethod, "__construct", path, "function __construct($filePermission = null)"),
		owner:       divergentDefaultTestNode(path+"::"+ownerName, graph.KindType, ownerName, path, "class "+ownerName),
	}
}

func addDivergentProjectionEndpoints(memory *graph.Graph, base exploreDivergentDefaultBase, calls, extenders int, prefix string) {
	for index := 0; index < calls; index++ {
		id := fmt.Sprintf("src/%s-call-%02d.php::Child.__construct", prefix, index)
		memory.AddNode(divergentDefaultTestNode(id, graph.KindMethod, "__construct", fmt.Sprintf("src/%s-call-%02d.php", prefix, index), "function __construct($filePermission = 0644)"))
		memory.AddEdge(&graph.Edge{From: id, To: base.constructor.ID, Kind: graph.EdgeCalls})
	}
	for index := 0; index < extenders; index++ {
		id := fmt.Sprintf("src/%s-type-%02d.php::Child", prefix, index)
		memory.AddNode(divergentDefaultTestNode(id, graph.KindType, "Child", fmt.Sprintf("src/%s-type-%02d.php", prefix, index), "class Child"))
		memory.AddEdge(&graph.Edge{From: id, To: base.owner.ID, Kind: graph.EdgeExtends})
	}
}

func TestProjectExploreDivergentDefaultOwnersHonorsEdgeAndEndpointCaps(t *testing.T) {
	base := divergentProjectionBase(0)
	memory := graph.New()
	addDivergentProjectionEndpoints(memory, base, exploreDefaultOwnerEdgeCap, exploreDefaultOwnerEdgeCap, "exact")
	projection, complete := projectExploreDivergentDefaultOwners(context.Background(), memory, []exploreDivergentDefaultBase{base})
	if !complete || len(projection.nodes) != exploreDefaultOwnerEndpointCap {
		t.Fatalf("exact endpoint cap = %d/%v, want %d/true", len(projection.nodes), complete, exploreDefaultOwnerEndpointCap)
	}

	overEdge := graph.New()
	addDivergentProjectionEndpoints(overEdge, base, exploreDefaultOwnerEdgeCap+1, 0, "edge")
	if projection, complete := projectExploreDivergentDefaultOwners(context.Background(), overEdge, []exploreDivergentDefaultBase{base}); complete || len(projection.nodes) != 0 {
		t.Fatalf("17th caller returned partial projection: %#v, %v", projection, complete)
	}

	second := divergentProjectionBase(1)
	overUnion := graph.New()
	addDivergentProjectionEndpoints(overUnion, base, exploreDefaultOwnerEdgeCap, exploreDefaultOwnerEdgeCap, "union")
	addDivergentProjectionEndpoints(overUnion, second, 1, 0, "extra")
	if projection, complete := projectExploreDivergentDefaultOwners(context.Background(), overUnion, []exploreDivergentDefaultBase{base, second}); complete || len(projection.nodes) != 0 {
		t.Fatalf("33rd endpoint returned partial projection: %#v, %v", projection, complete)
	}

	missing := graph.New()
	missing.AddEdge(&graph.Edge{From: "missing-node", To: base.constructor.ID, Kind: graph.EdgeCalls})
	if projection, complete := projectExploreDivergentDefaultOwners(context.Background(), missing, []exploreDivergentDefaultBase{base}); complete || len(projection.nodes) != 0 {
		t.Fatalf("missing exact refetch returned partial projection: %#v, %v", projection, complete)
	}
}

func TestExploreDivergentDefaultOwnerUsesRequestScopeAndOverlayView(t *testing.T) {
	task := `StreamHandler::write reports permission denied; find the divergent filePermission default and owning type`
	fixture := monologDivergentDefaultFixture()
	targets := fixture.targets[:1]

	got := promoteExploreDivergentDefaultOwner(
		context.Background(), task, targets, fixture.store,
		graph.LocalizationNodeScope{WorkspaceID: "other"}, 3, divergentDefaultTestSource, 30*time.Second,
	)
	if len(got) != 1 || got[0].node.ID != fixture.write.ID {
		t.Fatalf("out-of-scope fallback promoted owner: %#v", got)
	}

	layer := graph.NewOverlayLayer()
	layer.MarkFile(fixture.baseType.FilePath, true)
	got = promoteExploreDivergentDefaultOwner(
		context.Background(), task, targets, graph.NewOverlaidView(fixture.store, layer),
		graph.LocalizationNodeScope{}, 3, divergentDefaultTestSource, 30*time.Second,
	)
	if len(got) != 1 || got[0].node.ID != fixture.write.ID {
		t.Fatalf("overlay-hidden base owner was reintroduced: %#v", got)
	}

	got = promoteExploreDivergentDefaultOwner(
		context.Background(), task, targets, legacyOnlyExploreReader{Reader: fixture.store},
		graph.LocalizationNodeScope{}, 3, divergentDefaultTestSource, 30*time.Second,
	)
	if len(got) != 1 || got[0].node.ID != fixture.write.ID {
		t.Fatalf("reader without bounded capability used legacy full-row fallback: %#v", got)
	}
}

func TestOptionalBoundedRecoveryCannotCertifyRequiredNameAbsence(t *testing.T) {
	ctx := context.Background()
	base := graph.New()
	node := &graph.Node{ID: "src/a.go::Owner", Name: "Owner", Kind: graph.KindType, FilePath: "src/a.go"}
	base.AddNode(node)
	exact := &exactNameAnchorCountingReader{Reader: base}
	stripped := legacyOnlyExploreReader{Reader: exact}
	for _, test := range []struct {
		name   string
		reader graph.Reader
		usable bool
	}{
		{"supported", exact, true}, {"stripped", stripped, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Given a known node, optional recovery needs bounded proof, never a legacy full-row read.
			// When proof is unavailable, false means unknown; it cannot certify that Owner is absent.
			byName, usable := boundedLocalizationExactName(ctx, test.reader, "Owner", graph.LocalizationNodeScope{}, 4)
			if usable != test.usable || (usable && (len(byName.Nodes) != 1 || byName.Nodes[0].ID != node.ID)) {
				t.Fatalf("optional exact projection=%#v/%t", byName, usable)
			}
			byFile, usable := boundedLocalizationFileNodes(ctx, test.reader, localizationFileBudgetFor(ctx), node.FilePath, graph.LocalizationNodeScope{}, 4)
			if usable != test.usable || (usable && (len(byFile.Nodes) != 1 || byFile.Nodes[0].ID != node.ID)) {
				t.Fatalf("optional file projection=%#v/%t", byFile, usable)
			}
		})
	}
	// Given a required coherent name read, the same missing proof must remain a visible refusal.
	nodes, more, err := nameOnlySymbolCandidates(ctx, stripped, "Owner", 4, query.QueryOptions{})
	if !errors.Is(err, projectionSuperseded) || len(nodes) != 0 || more {
		t.Fatalf("required stripped read=%#v/%t/%v", nodes, more, err)
	}
	view := graph.NewOverlaidView(stripped, graph.NewOverlayLayer())
	nodes, more, err = nameOnlySymbolCandidates(ctx, view, "Owner", 4, query.QueryOptions{})
	if err == nil || err.Error() != "graph: selected base lacks coherent folded-name candidates" || len(nodes) != 0 || more {
		t.Fatalf("required overlay read=%#v/%t/%v", nodes, more, err)
	}
}

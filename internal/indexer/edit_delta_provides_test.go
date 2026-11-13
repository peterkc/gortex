package indexer

import (
	"context"
	"fmt"
	"iter"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
)

// The provides rows a delta's resolver indexes are the stack's rows, kept per
// stack, with the delta's change set as its view holds it: every delta's
// answer equals a scan of its own view, a later delta over the same stack does
// not scan, and the earlier delta's changed file is served as the stack holds
// it once a later delta leaves it unchanged.
func TestEditDeltaProvidesRowsAreKeptPerStack(t *testing.T) {
	resetEditDeltaProvides()
	t.Cleanup(resetEditDeltaProvides)
	provides := func(file, from, forName, useClass string) ([]*graph.Node, *graph.Edge) {
		p := "repo/" + file
		nodes := []*graph.Node{
			{ID: p, Kind: graph.KindFile, Name: file, FilePath: p, RepoPrefix: "repo", Language: "typescript"},
			{ID: p + "::" + from, Kind: graph.KindType, Name: from, FilePath: p, RepoPrefix: "repo", Language: "typescript"},
		}
		edge := &graph.Edge{From: p + "::" + from, To: graph.UnresolvedMarker + useClass, Kind: graph.EdgeProvides, FilePath: p, Line: 3,
			Meta: map[string]any{graph.MetaDIProvidesFor: forName, graph.MetaDIBinding: graph.DIBindingUseClass}}
		return nodes, edge
	}
	build := func(files map[string][3]string) *graph.Graph {
		g := graph.New()
		keys := make([]string, 0, len(files))
		for k := range files {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, file := range keys {
			v := files[file]
			nodes, edge := provides(file, v[0], v[1], v[2])
			g.AddBatch(nodes, []*graph.Edge{edge})
		}
		return g
	}
	render := func(byRepo map[string][]*graph.Edge) string {
		var rows []string
		for repo, edges := range byRepo {
			for _, e := range edges {
				rows = append(rows, fmt.Sprintf("%s %s %s %v", repo, e.From, e.To, e.Meta[graph.MetaDIProvidesFor]))
			}
		}
		sort.Strings(rows)
		return strings.Join(rows, "\n")
	}
	scan := func(g *graph.Graph) string {
		out := make(map[string][]*graph.Edge)
		for _, row := range scanProvidesRows(g) {
			out[row.repo] = append(out[row.repo], row.edge)
		}
		return render(out)
	}
	stackFiles := map[string][3]string{
		"x.ts": {"XModule", "Logger", "ConsoleLogger"},
		"y.ts": {"YModule", "Store", "MemoryStore"},
	}
	stack := build(stackFiles)
	deltaOver := func(changes map[string][3]string, changed ...string) (*editDeltaProvides, *graph.Graph) {
		files := make(map[string][3]string)
		for k, v := range stackFiles {
			files[k] = v
		}
		for k, v := range changes {
			files[k] = v
		}
		view := build(files)
		idx := &Indexer{repoPrefix: "repo", graph: view, resolver: resolver.New(view)}
		return installEditDeltaProvides(idx, stack, "stack", changed), view
	}

	// The first delta changes x.ts's binding: a miss, answered as its view.
	first, view1 := deltaOver(map[string][3]string{"x.ts": {"XModule", "Logger", "FileLogger"}}, "x.ts")
	if got, want := render(first.rows()), scan(view1); got != want || first.cached {
		t.Fatalf("first delta (cached=%t):\n%s\nwant:\n%s", first.cached, got, want)
	}
	// The second delta changes y.ts only: x.ts is back as the stack holds it.
	second, view2 := deltaOver(map[string][3]string{"y.ts": {"YModule", "Store", "DiskStore"}}, "y.ts")
	got, want := render(second.rows()), scan(view2)
	if got != want || !second.cached {
		t.Fatalf("second delta (cached=%t):\n%s\nwant:\n%s", second.cached, got, want)
	}
	if !strings.Contains(got, "ConsoleLogger") || strings.Contains(got, "FileLogger") {
		t.Fatalf("the first delta's change leaked into the stack's rows:\n%s", got)
	}
	// A delta that adds a file with a binding serves it.
	third, view3 := deltaOver(map[string][3]string{"z.ts": {"ZModule", "Clock", "SystemClock"}}, "z.ts")
	if got, want := render(third.rows()), scan(view3); got != want || !strings.Contains(got, "SystemClock") {
		t.Fatalf("third delta:\n%s\nwant:\n%s", got, want)
	}
}

func TestEditDeltaProvidesReadsNoAdjacency(t *testing.T) {
	// Given a dirty chain whose layers carry nodes outside their covered paths.
	r := newChainOverlayRun(t)
	d := r.depths[len(r.depths)-1]
	p := r.pair(d)
	idx := &Indexer{repoPrefix: builderRepoPrefix, graph: p.cached, resolver: resolver.New(p.cached)}
	o := installEditDeltaProvides(idx, p.cacheBase, p.key, []string{"app/caller.go"})
	if o == nil {
		t.Fatal("no provides source")
	}
	cache := editDeltaBaseCache(p.key)
	baseHits, baseMisses := cache.StackBaseAdjacencyStats()
	layerHits, layerMisses := cache.StackLayerAdjacencyStats()

	// When the provides source reads the deepest chain's view.
	got := o.rows()

	// Then it requests no adjacency and returns the same rows as a full scan.
	afterBaseHits, afterBaseMisses := cache.StackBaseAdjacencyStats()
	afterLayerHits, afterLayerMisses := cache.StackLayerAdjacencyStats()
	if afterBaseHits+afterBaseMisses != baseHits+baseMisses || afterLayerHits+afterLayerMisses != layerHits+layerMisses {
		t.Errorf("provides requested adjacency: base %d -> %d, layers %d -> %d",
			baseHits+baseMisses, afterBaseHits+afterBaseMisses, layerHits+layerMisses, afterLayerHits+afterLayerMisses)
	}
	want := make(map[string][]*graph.Edge)
	for _, row := range scanProvidesRows(p.plain) {
		want[row.repo] = append(want[row.repo], row.edge)
	}
	if len(want) == 0 {
		t.Fatal("fixture precondition: no provides rows")
	}
	chainOverlaySame(t, "provides", renderChainEdgeMap(got), renderChainEdgeMap(want))
}

// providesScanGraph counts whole-stack provides reads without changing the view.
type providesScanGraph struct {
	*graph.Graph
	scans int
}

func (g *providesScanGraph) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	if kind == graph.EdgeProvides {
		g.scans++
	}
	return g.Graph.EdgesByKind(kind)
}

// providesChainLayer can place a source outside the paths the layer covers.
type providesChainLayer struct {
	*graph.OverlayLayer
	generation int64
	detached   *graph.Node
}

func (l *providesChainLayer) GenerationID() int64 { return l.generation }

func (l *providesChainLayer) OwnsNodeIdentity(id string) bool {
	return (l.detached != nil && id == l.detached.ID) || l.OverlayLayer.OwnsNodeIdentity(id)
}

func (l *providesChainLayer) NodeByID(id string) *graph.Node {
	if l.detached != nil && id == l.detached.ID {
		return l.detached
	}
	return l.OverlayLayer.NodeByID(id)
}

func TestEditDeltaProvidesPrewarmedChain(t *testing.T) {
	for _, depth := range []int{1, 3} {
		for _, scenario := range []string{"unchanged", "source_only_in_chain", "own_add", "own_evict"} {
			t.Run(fmt.Sprintf("depth_%d/%s", depth, scenario), func(t *testing.T) {
				resetChainOverlayCaches()
				t.Cleanup(resetChainOverlayCaches)
				b := builderNewBuilder(builderOpenStore(t, "provides"))
				source := &graph.Node{ID: "repo/provider.ts::Module", Kind: graph.KindType,
					Name: "Module", FilePath: "repo/provider.ts", RepoPrefix: "repo"}
				edge := &graph.Edge{From: source.ID, To: graph.UnresolvedMarker + "Service",
					Kind: graph.EdgeProvides, FilePath: source.FilePath, Line: 3}
				g := &providesScanGraph{Graph: graph.New()}
				if scenario != "source_only_in_chain" {
					g.AddNode(source)
				}
				g.AddEdge(edge)
				base := commitLayerBase{Reader: g, stack: []int64{1000001}}
				key, ok := editDeltaBaseCacheKey(base, b.Store)
				if !ok {
					t.Fatal("no stack key")
				}

				// Given a commit stack whose provides pre-warm has finished.
				if !b.PrewarmEditDeltaStack(context.Background(), openFixed(base), "repo", "", "", nil) {
					t.Fatal("pre-warm did not run")
				}
				entry, ok := cachedEditDeltaProvides(key)
				if !ok || g.scans == 0 {
					t.Fatal("fixture precondition: provides step did not scan and cache the stack")
				}
				if scenario == "source_only_in_chain" && len(entry.rows) != 0 {
					t.Fatal("fixture precondition: source was already placed below the chain")
				}
				var view graph.Reader = base
				var generations []int64
				for i := 0; i < depth; i++ {
					layer := &providesChainLayer{OverlayLayer: graph.NewOverlayLayer(), generation: int64(1000002 + i)}
					if scenario == "source_only_in_chain" && i == 0 {
						layer.detached = source
					}
					view = graph.NewOverlaidViewWithLayer(view, layer)
					generations = append(generations, layer.generation)
				}
				dw := graph.NewDeltaWriter(view, nil)
				dw.SetBaseProjectionCache(editDeltaBaseCache(key))
				dw.SetChainGenerations(generations, nil)
				if _, ok := dw.ChainSplitBase(); !ok {
					t.Fatal("fixture precondition: chain cannot split")
				}
				switch scenario {
				case "own_add":
					added := &graph.Node{ID: "repo/added.ts::Added", Kind: graph.KindType,
						Name: "Added", FilePath: "repo/added.ts", RepoPrefix: "repo"}
					dw.AddBatch([]*graph.Node{added}, []*graph.Edge{{From: added.ID,
						To: graph.UnresolvedMarker + "AddedService", Kind: graph.EdgeProvides, FilePath: added.FilePath, Line: 4}})
				case "own_evict":
					dw.EvictFile(source.FilePath)
				}
				idx := &Indexer{repoPrefix: "repo", graph: dw, resolver: resolver.New(dw)}
				o := installEditDeltaProvides(idx, base, key, nil)

				// When the first chained delta builds its provides rows.
				g.scans = 0
				got := o.rows()

				// Then it reuses the warm edges, not a below-chain scan.
				if g.scans != 0 {
					t.Errorf("pre-warmed chain ran %d below-chain provides scans, want 0", g.scans)
				}
				if !o.cached {
					t.Error("first chained delta missed the pre-warmed entry")
				}
				want := make(map[string][]*graph.Edge)
				for _, row := range scanProvidesRows(dw) {
					want[row.repo] = append(want[row.repo], row.edge)
				}
				wantCount := 1
				switch scenario {
				case "own_add":
					wantCount = 2
				case "own_evict":
					wantCount = 0
				}
				if len(want["repo"]) != wantCount {
					t.Fatalf("fixture precondition: scan has %d provides rows, want %d", len(want["repo"]), wantCount)
				}
				chainOverlaySame(t, "provides", renderChainEdgeMap(got), renderChainEdgeMap(want))
				editDeltaProvidesCache.Lock()
				entries := append([]editDeltaProvidesEntry(nil), editDeltaProvidesCache.entries...)
				editDeltaProvidesCache.Unlock()
				if len(entries) != 1 || entries[0].key != key {
					t.Errorf("pre-warmed stack holds %d provides entries, want one under the plain key", len(entries))
				}
			})
		}
	}
}

func TestEditDeltaProvidesChainEntryUpgrade(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty_%t", empty), func(t *testing.T) {
			resetEditDeltaProvides()
			t.Cleanup(resetEditDeltaProvides)
			g := &providesScanGraph{Graph: graph.New()}
			if !empty {
				source := &graph.Node{ID: "repo/provider.ts::Module", Kind: graph.KindType,
					Name: "Module", FilePath: "repo/provider.ts", RepoPrefix: "repo"}
				g.AddBatch([]*graph.Node{source}, []*graph.Edge{{From: source.ID,
					To: graph.UnresolvedMarker + "Service", Kind: graph.EdgeProvides, FilePath: source.FilePath}})
			}
			layer := &providesChainLayer{OverlayLayer: graph.NewOverlayLayer(), generation: 1}
			dw := graph.NewDeltaWriter(graph.NewOverlaidViewWithLayer(g, layer), nil)
			dw.SetBaseProjectionCache(graph.NewBaseProjectionCache())
			dw.SetChainGenerations([]int64{1}, nil)
			if _, ok := dw.ChainSplitBase(); !ok {
				t.Fatal("fixture precondition: chain cannot split")
			}
			key := t.Name()
			// Given the rows-only entry a no-split fallback stores.
			storeEditDeltaProvides(key, scanProvidesRows(g))
			idx := &Indexer{repoPrefix: "repo", graph: dw, resolver: resolver.New(dw)}
			o := installEditDeltaProvides(idx, g, key, nil)

			// When a later chained delta can split, it loads the missing edges.
			g.scans = 0
			got := o.rows()
			if g.scans != 1 || o.cached {
				t.Errorf("rows-only entry: scans=%d cached=%t, want one scan and a miss", g.scans, o.cached)
			}
			want := make(map[string][]*graph.Edge)
			for _, row := range scanProvidesRows(dw) {
				want[row.repo] = append(want[row.repo], row.edge)
			}
			if !empty && len(want["repo"]) != 1 {
				t.Fatal("fixture precondition: scan has no provides row")
			}
			chainOverlaySame(t, "upgraded provides", renderChainEdgeMap(got), renderChainEdgeMap(want))

			// Then even an empty edge set is complete and needs no reload.
			g.scans = 0
			got = o.rows()
			if g.scans != 0 || !o.cached {
				t.Errorf("complete entry: scans=%d cached=%t, want no scan and a hit", g.scans, o.cached)
			}
			chainOverlaySame(t, "reused provides", renderChainEdgeMap(got), renderChainEdgeMap(want))
			editDeltaProvidesCache.Lock()
			entries := append([]editDeltaProvidesEntry(nil), editDeltaProvidesCache.entries...)
			editDeltaProvidesCache.Unlock()
			if len(entries) != 1 || entries[0].key != key || !entries[0].hasEdges {
				t.Errorf("upgraded stack holds %d provides entries, want one complete entry under the plain key", len(entries))
			}
		})
	}
}

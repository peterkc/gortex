package resolver

import (
	"cmp"
	"fmt"
	"iter"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

const rebuildGRPCFixtureFile = "p/stubs.go"

func rebuildGRPCFixture(extra int) store_pebble.ParseFacts {
	const caller, reg = "p/client.go::call", "p/server.go::register"
	const first, second, fallback, skipped = "p/a.go::srv", "p/b.go::srv", "p/f.go::other", "p/u.go::unimplemented"
	const iface = "p/i.go::GreeterServer"
	node := func(id, name string, kind graph.NodeKind) *graph.Node {
		return &graph.Node{ID: id, Name: name, Kind: kind, Language: "go", FilePath: rebuildGRPCFixtureFile}
	}
	nodes := []*graph.Node{
		node(caller, "call", graph.KindFunction), node(reg, "register", graph.KindFunction),
		node(first, "server", graph.KindType), node(second, "server", graph.KindType),
		node(fallback, "other", graph.KindType), node(skipped, "UnimplementedGreeterServer", graph.KindType),
		node(iface, "GreeterServer", graph.KindInterface),
		node(first+".Hello", "Hello", graph.KindMethod), node(second+".Hello", "Hello", graph.KindMethod),
		node(fallback+".World", "World", graph.KindMethod), node(skipped+".World", "World", graph.KindMethod),
	}
	edge := func(from, to string, kind graph.EdgeKind, line int, meta map[string]any) *graph.Edge {
		return &graph.Edge{From: from, To: to, Kind: kind, FilePath: rebuildGRPCFixtureFile, Line: line, Meta: meta}
	}
	stub := func(to, method string, line int) *graph.Edge {
		return edge(caller, to, graph.EdgeCalls, line, map[string]any{
			"via": "grpc.stub", "grpc_service": "Greeter", "grpc_method": method,
		})
	}
	old := stub("old::Gone", "Gone", 12)
	old.Origin = graph.OriginASTResolved
	old.Confidence = 0.9
	old.Meta["grpc_resolution"] = old.Origin
	StampSynthesized(old, SynthGRPCStub)
	stationary := stub(first+".Hello", "Hello", 13)
	stationary.Meta["unrelated"] = "keep stationary"
	winner := stub(grpcStubPlaceholder("Greeter", "Hello"), "Hello", 10)
	winner.Meta["unrelated"] = "old-key first"
	loser := stub("z::another-old-target", "Hello", 10)
	loser.Meta["grpc_service"] = "Alias"
	loser.Meta["unrelated"] = "native first"
	incumbent := edge(caller, first+".Hello", graph.EdgeCalls, 14, map[string]any{"unrelated": "stationary destination"})
	edges := []*graph.Edge{
		edge(reg, "unresolved::RegisterGreeterServer", graph.EdgeCalls, 5,
			map[string]any{"grpc_register_service": "Greeter", "grpc_register_impl": "server"}),
		edge(reg, "unresolved::RegisterAliasServer", graph.EdgeCalls, 6,
			map[string]any{"grpc_register_service": "Alias", "grpc_register_impl": "server"}),
		edge(first+".Hello", first, graph.EdgeMemberOf, 1, nil), edge(second+".Hello", second, graph.EdgeMemberOf, 1, nil),
		edge(fallback+".World", fallback, graph.EdgeMemberOf, 1, nil), edge(skipped+".World", skipped, graph.EdgeMemberOf, 1, nil),
		edge(fallback, iface, graph.EdgeImplements, 2, nil), edge(skipped, iface, graph.EdgeImplements, 2, nil),
		loser, winner, stub(grpcStubPlaceholder("Greeter", "World"), "World", 11), old, stationary,
		incumbent, stub(grpcStubPlaceholder("Greeter", "Hello"), "Hello", 14),
	}
	for i := range extra {
		// The old-key census exceeds 1 MiB without changing the handler bucket.
		edges = append(edges, stub("unresolved::"+strings.Repeat("a", 4096), "Hello", 20+i))
	}
	return store_pebble.ParseFacts{
		EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: rebuildGRPCFixtureFile, NodeCount: len(nodes)},
		Nodes: nodes, Edges: edges,
	}
}

func openRebuildGRPCFixture(t *testing.T, facts store_pebble.ParseFacts) (*store_pebble.BuildStore, iter.Seq2[store_pebble.ParseFacts, error]) {
	t.Helper()
	b := openRebuildGRPCBuild(t, facts)
	return b, rebuildGRPCReceipts(facts)
}

func openRebuildGRPCBuild(t *testing.T, facts ...store_pebble.ParseFacts) *store_pebble.BuildStore {
	t.Helper()
	b, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "working"), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	for _, f := range facts {
		require.NoError(t, b.AddParsedFile("", f))
	}
	return b
}

func rebuildGRPCReceipts(facts ...store_pebble.ParseFacts) iter.Seq2[store_pebble.ParseFacts, error] {
	return func(yield func(store_pebble.ParseFacts, error) bool) {
		for _, f := range facts {
			if !yield(f, nil) {
				return
			}
		}
	}
}

func rebuildGRPCNative(t *testing.T, facts store_pebble.ParseFacts) *store_sqlite.Store {
	t.Helper()
	native, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, native.Close()) })
	require.NoError(t, native.AddBatchChecked(facts.Nodes, facts.Edges))
	return native
}

func rebuildGRPCNativeCalls(t *testing.T, native *store_sqlite.Store, wantResolved int) []*graph.Edge {
	t.Helper()
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthGRPCStub})
	require.NoError(t, err)
	report := RunFrameworkSynthesizersWithSelection(native, selection)
	require.NoError(t, report.Err)
	require.Equal(t, wantResolved, report.Total)
	want := slices.Collect(native.EdgesByKind(graph.EdgeCalls))
	slices.SortFunc(want, rebuildGRPCCallLess)
	return want
}

func rebuildGRPCCallLess(a, b *graph.Edge) int {
	return cmp.Or(cmp.Compare(a.From, b.From), cmp.Compare(a.To, b.To),
		cmp.Compare(a.FilePath, b.FilePath), cmp.Compare(a.Line, b.Line))
}

func rebuildGRPCSavedCalls(t *testing.T, b *store_pebble.BuildStore) []*graph.Edge {
	t.Helper()
	var got []*graph.Edge
	require.NoError(t, b.RebuildView().VisitEdges(t.Context(), func(e *graph.Edge) error {
		if e.Kind == graph.EdgeCalls {
			got = append(got, e)
		}
		return nil
	}))
	slices.SortFunc(got, rebuildGRPCCallLess)
	return got
}

func TestRebuildGRPCNativeRows(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra int
	}{
		{"base fixture", 0},
		{"extra stub call sites", 270},
	} {
		t.Run(tc.name, func(t *testing.T) {
			extra := tc.extra
			facts := rebuildGRPCFixture(extra)
			// The two services converge at one site. Native assigns the earlier
			// receipt row the smaller edge ID, although it has the later old key.
			want := rebuildGRPCNativeCalls(t, rebuildGRPCNative(t, facts), 5+extra)
			selection, err := NewFrameworkSynthesizerSelection([]string{SynthGRPCStub})
			require.NoError(t, err)
			// Perturb arrival into the live scratch graph, not the frozen order
			// that supplies the native census ordinal.
			live := facts
			live.Edges = slices.Clone(facts.Edges)
			slices.Reverse(live.Edges)
			b := openRebuildGRPCBuild(t, live)
			receipts := rebuildGRPCReceipts(facts)
			stats, err := RunRebuildGRPC(t.Context(), b, receipts, selection)
			require.NoError(t, err)
			require.Equal(t, uint64(6+extra), stats.Stubs)
			require.Equal(t, uint64(2), stats.Registrations)
			require.Equal(t, uint64(5+extra), stats.Resolved)
			require.Equal(t, uint64(5+extra), stats.Moves)
			got := rebuildGRPCSavedCalls(t, b)
			require.Equal(t, want, got)
			at := make(map[int]*graph.Edge)
			for _, e := range got {
				at[e.Line] = e
			}
			require.Equal(t, "p/a.go::srv.Hello", at[10].To)
			require.Equal(t, "native first", at[10].Meta["unrelated"])
			require.Equal(t, "p/f.go::other.World", at[11].To)
			require.Equal(t, graph.OriginASTInferred, at[11].Origin)
			require.Equal(t, grpcStubPlaceholder("Greeter", "Gone"), at[12].To)
			require.Empty(t, at[12].Origin)
			require.NotContains(t, at[12].Meta, "grpc_resolution")
			require.Equal(t, "keep stationary", at[13].Meta["unrelated"])
			require.Equal(t, "stationary destination", at[14].Meta["unrelated"])
			again, err := RunRebuildGRPC(t.Context(), b, receipts, selection)
			require.NoError(t, err)
			require.Zero(t, again.Moves)
			require.Equal(t, want, rebuildGRPCSavedCalls(t, b))
		})
	}
}

func TestRebuildGRPCRefusesBeforeMoving(t *testing.T) {
	for _, mode := range []string{"unproved", "read failure", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			facts := rebuildGRPCFixture(0)
			b, _ := openRebuildGRPCFixture(t, facts)
			before := rebuildGRPCSavedCalls(t, b)
			selection := AllFrameworkSynthesizers()
			if mode == "disabled" {
				var err error
				selection, err = NewFrameworkSynthesizerSelection(nil)
				require.NoError(t, err)
			}
			readErr := fmt.Errorf("%w: planted gRPC receipt read failure", store_pebble.ErrRebuildView)
			stats, err := RunRebuildGRPC(t.Context(), b, func(yield func(store_pebble.ParseFacts, error) bool) {
				switch mode {
				case "read failure":
					if yield(facts, nil) {
						yield(store_pebble.ParseFacts{}, readErr)
					}
				case "unproved":
					facts.EffectiveLanguage = "python"
					yield(facts, nil)
				default:
					t.Fatal("disabled gRPC read its input")
				}
			}, selection)
			switch mode {
			case "read failure":
				require.ErrorIs(t, err, readErr)
			case "unproved":
				require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
			default:
				require.NoError(t, err)
			}
			require.Zero(t, stats)
			require.Equal(t, before, rebuildGRPCSavedCalls(t, b))
		})
	}
}

func rebuildGRPCFixtureStub(from, to, service, method string, line int) *graph.Edge {
	return &graph.Edge{From: from, To: to, Kind: graph.EdgeCalls, FilePath: rebuildGRPCFixtureFile, Line: line,
		Meta: map[string]any{"via": "grpc.stub", "grpc_service": service, "grpc_method": method}}
}

func TestRebuildGRPCMoveWindows(t *testing.T) {
	const caller = "p/client.go::call"
	const hello, world = "p/a.go::srv.Hello", "p/f.go::other.World"
	const within, across = 10, 11
	facts := rebuildGRPCFixture(0)
	facts.Edges = slices.DeleteFunc(facts.Edges, func(e *graph.Edge) bool { return e.From == caller })
	mover := func(to, method, label string, line int) *graph.Edge {
		e := rebuildGRPCFixtureStub(caller, to, "Greeter", method, line)
		e.Meta["window_witness"] = label
		return e
	}
	withinFirst := mover(grpcStubPlaceholder("Greeter", "Hello"), "Hello", "within first", within)
	withinSecond := mover(hello, "World", "within second", within)
	acrossFirst := mover(grpcStubPlaceholder("Greeter", "Hello"), "Hello", "across first", across)
	acrossSecond := mover(hello, "World", "across second", across)
	withinOther := mover("z::other", "Hello", "within loser", within)
	facts.Edges = append(facts.Edges, withinFirst, withinSecond, withinOther)
	for i := range 4996 {
		facts.Edges = append(facts.Edges, rebuildGRPCFixtureStub(caller, grpcStubPlaceholder("Greeter", "Hello"), "Greeter", "Hello", 20+i))
	}
	facts.Edges = append(facts.Edges, acrossFirst, acrossSecond)

	native := rebuildGRPCNative(t, facts)
	// Pin the two chain positions against the actual SQLite census, not the
	// fixture slice alone. Registration calls do not count toward a window.
	var order []graph.EdgeIdentity
	for e := range native.FrameworkCensusEdgesSeq(graph.EdgeCalls) {
		if e.From == caller {
			order = append(order, e.EdgeIdentity)
		}
	}
	require.Len(t, order, 5001)
	require.Equal(t, graph.EdgeIdentityFor(withinFirst), order[0])
	require.Equal(t, graph.EdgeIdentityFor(withinSecond), order[1])
	require.Equal(t, graph.EdgeIdentityFor(withinOther), order[2])
	require.Equal(t, graph.EdgeIdentityFor(acrossFirst), order[4999])
	require.Equal(t, graph.EdgeIdentityFor(acrossSecond), order[5000])
	want := rebuildGRPCNativeCalls(t, native, 5001)
	b, receipts := openRebuildGRPCFixture(t, facts)
	stats, err := RunRebuildGRPC(t.Context(), b, receipts, AllFrameworkSynthesizers())
	require.NoError(t, err)
	require.Equal(t, uint64(5001), stats.Moves)
	got := rebuildGRPCSavedCalls(t, b)
	require.Equal(t, want, got)

	// The within-window chain frees the occupied key and keeps the first
	// payload over the third. Across windows, the first mover loses to the
	// still-occupied old key; later deletion does not resurrect it.
	witnesses := map[int]map[string]any{within: {}, across: {}}
	for _, e := range got {
		if e.From == caller && witnesses[e.Line] != nil {
			witnesses[e.Line][e.To] = e.Meta["window_witness"]
		}
	}
	require.Equal(t, map[string]any{hello: "within first", world: "within second"}, witnesses[within])
	require.Equal(t, map[string]any{world: "across second"}, witnesses[across])
}

// Split the election inputs into their actual files for node/edge proof.
func rebuildGRPCFileReceipts(facts store_pebble.ParseFacts) []store_pebble.ParseFacts {
	byPath := make(map[string]*store_pebble.ParseFacts)
	file := func(path string) *store_pebble.ParseFacts {
		f := byPath[path]
		if f == nil {
			language := "go"
			if strings.HasSuffix(path, ".ts") {
				language = "typescript"
			}
			f = &store_pebble.ParseFacts{EffectiveLanguage: language, File: graph.FileMetaRow{FilePath: path}}
			byPath[path] = f
		}
		return f
	}
	for _, n := range facts.Nodes {
		f := file(n.FilePath)
		f.Nodes = append(f.Nodes, n)
		f.File.NodeCount++
	}
	for _, e := range facts.Edges {
		f := file(e.FilePath)
		f.Edges = append(f.Edges, e)
	}
	var receipts []store_pebble.ParseFacts
	for _, path := range slices.Sorted(maps.Keys(byPath)) {
		receipts = append(receipts, *byPath[path])
	}
	return receipts
}

func TestRebuildGRPCNativeElection(t *testing.T) {
	const first, second = "a::server.Hello", "z::server.Hello"
	const registered, inferred = graph.OriginASTResolved, graph.OriginASTInferred
	type candidate struct {
		id, path, repo, methodRepo string
		typeKind, methodKind       graph.NodeKind
	}
	firstType := candidate{"a::server", "a/impl.go", "local", "local", graph.KindType, graph.KindMethod}
	secondType := candidate{"z::server", "z/impl.go", "remote", "remote", graph.KindType, graph.KindMethod}
	nearFirst, nearSecond := firstType, secondType
	nearFirst.path, nearFirst.repo = "p/z.go", "z-repo"
	nearSecond.path, nearSecond.repo = "p/a.go", "a-repo"
	localSecond := secondType
	localSecond.repo, localSecond.methodRepo = "local", "local"
	otherLanguage := localSecond
	otherLanguage.path = "z/impl.ts"
	wrongType, wrongMethod := secondType, secondType
	wrongType.typeKind, wrongType.path = graph.KindInterface, "p/wrong.go"
	wrongMethod.methodKind, wrongMethod.methodRepo = graph.KindFunction, "local"

	for _, tc := range []struct {
		name         string
		candidates   []candidate
		registration string
		fallback     string
		extraMethod  bool
		want, origin string
	}{
		{"single cross-repo type and handler", []candidate{secondType}, "server", "", false, second, registered},
		{"registration prefers sole same-repo type", []candidate{firstType, secondType}, "server", "", false, first, registered},
		{"ambiguous same-repo types use interface fallback", []candidate{firstType, localSecond}, "server", "first", false, first, inferred},
		{"ambiguous registration abstains", []candidate{firstType, localSecond}, "server", "", false, "", ""},
		{"same directory uses node ID, not scoped index order", []candidate{nearSecond, nearFirst}, "server", "", false, first, registered},
		{"no matching registration prefers same-repo handler", []candidate{firstType, secondType}, "", "all", false, first, inferred},
		{"empty impl uses interface fallback", []candidate{firstType}, "empty", "all", false, first, inferred},
		{"empty source registration does not land a stub", []candidate{firstType}, "empty source", "", false, "", ""},
		{"two same-name methods on one type are ambiguous", []candidate{firstType}, "server", "", true, "", ""},
		{"only types satisfy registration", []candidate{firstType, wrongType}, "server", "", false, first, registered},
		{"only methods satisfy handler election", []candidate{firstType, wrongMethod}, "", "all", false, first, inferred},
		{"TypeScript method makes same-repo fallback ambiguous", []candidate{firstType, otherLanguage}, "", "all", false, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const caller, reg, iface = "p/stubs.go::call", "p/stubs.go::register", "p/stubs.go::GreeterServer"
			stub := rebuildGRPCFixtureStub(caller, "old::Hello", "Greeter", "Hello", 10)
			facts := store_pebble.ParseFacts{
				Nodes: []*graph.Node{
					{ID: caller, Name: "call", Kind: graph.KindFunction, FilePath: rebuildGRPCFixtureFile, RepoPrefix: "local", Language: "go"},
					{ID: reg, Name: "register", Kind: graph.KindFunction, FilePath: rebuildGRPCFixtureFile, RepoPrefix: "local", Language: "go"},
					{ID: iface, Name: "GreeterServer", Kind: graph.KindInterface, FilePath: rebuildGRPCFixtureFile, Language: "go"},
				},
				Edges: []*graph.Edge{stub},
			}
			if tc.registration != "" {
				from, impl := reg, "server"
				switch tc.registration {
				case "empty":
					impl = ""
				case "empty source":
					from = ""
					// A typed, potentially moving stub with no caller is also skipped.
					facts.Edges = append(facts.Edges, rebuildGRPCFixtureStub("", "z::old", "Greeter", "Hello", 11))
				}
				facts.Edges = append(facts.Edges, &graph.Edge{From: from, To: "unresolved::RegisterGreeterServer",
					Kind: graph.EdgeCalls, FilePath: rebuildGRPCFixtureFile, Line: 5,
					Meta: map[string]any{"grpc_register_service": "Greeter", "grpc_register_impl": impl}})
			}
			for _, c := range tc.candidates {
				language := "go"
				if strings.HasSuffix(c.path, ".ts") {
					language = "typescript"
				}
				facts.Nodes = append(facts.Nodes,
					&graph.Node{ID: c.id, Name: "server", Kind: c.typeKind, FilePath: c.path, RepoPrefix: c.repo, Language: language},
					&graph.Node{ID: c.id + ".Hello", Name: "Hello", Kind: c.methodKind, FilePath: c.path, RepoPrefix: c.methodRepo, Language: language})
				facts.Edges = append(facts.Edges, &graph.Edge{From: c.id + ".Hello", To: c.id, Kind: graph.EdgeMemberOf, FilePath: c.path, Line: 1})
				if tc.fallback == "all" || tc.fallback == "first" && c.id == firstType.id {
					facts.Edges = append(facts.Edges, &graph.Edge{From: c.id, To: iface, Kind: graph.EdgeImplements, FilePath: c.path, Line: 2})
				}
				if tc.extraMethod {
					facts.Nodes = append(facts.Nodes, &graph.Node{ID: c.id + ".Hello2", Name: "Hello", Kind: graph.KindMethod,
						FilePath: c.path, RepoPrefix: c.methodRepo, Language: language})
					facts.Edges = append(facts.Edges, &graph.Edge{From: c.id + ".Hello2", To: c.id, Kind: graph.EdgeMemberOf, FilePath: c.path, Line: 3})
				}
			}
			target, resolved := tc.want, 1
			if target == "" {
				target, resolved = grpcStubPlaceholder("Greeter", "Hello"), 0
			}
			want := rebuildGRPCNativeCalls(t, rebuildGRPCNative(t, facts), resolved)
			receipts := rebuildGRPCFileReceipts(facts)
			b := openRebuildGRPCBuild(t, receipts...)
			stats, err := RunRebuildGRPC(t.Context(), b, rebuildGRPCReceipts(receipts...), AllFrameworkSynthesizers())
			require.NoError(t, err)
			require.Equal(t, uint64(1), stats.Stubs)
			require.Equal(t, uint64(1), stats.Moves)
			got := rebuildGRPCSavedCalls(t, b)
			require.Equal(t, want, got)
			i := slices.IndexFunc(got, func(e *graph.Edge) bool { return e.From == caller && e.Line == stub.Line })
			require.NotEqual(t, -1, i)
			require.Equal(t, target, got[i].To)
			require.Equal(t, tc.origin, got[i].Origin)
		})
	}
}

func TestRebuildGRPCExactReceiptProof(t *testing.T) {
	const caller = "p/client.go::call"
	const handler, runnerUp = "p/a.go::srv.Hello", "p/b.go::srv.Hello"
	const iface = "p/i.go::GreeterServer"
	for _, mode := range []string{
		"complete proof", "wrong old key", "wrong service", "wrong method", "missing caller",
		"missing elected handler", "mixed handler evidence", "mismatched live attributes",
		"contract caller", "contract caller without recorder", "contract caller without handler",
	} {
		t.Run(mode, func(t *testing.T) {
			facts := rebuildGRPCFixture(0)
			facts.Edges = slices.DeleteFunc(facts.Edges, func(e *graph.Edge) bool { return e.From == caller })
			stub := rebuildGRPCFixtureStub(caller, grpcStubPlaceholder("Greeter", "Hello"), "Greeter", "Hello", 10)
			facts.Edges = append(facts.Edges, stub, &graph.Edge{
				From: "p/b.go::srv", To: iface, Kind: graph.EdgeImplements, FilePath: rebuildGRPCFixtureFile, Line: 3,
			})
			// Native always elects the registered method. The proved runner-up
			// belongs to the interface fallback, not the registration bucket.
			native := rebuildGRPCNative(t, facts)
			ids := map[string]string{}
			if strings.HasPrefix(mode, "contract caller") {
				ids[caller] = "settings.rb"
				require.NoError(t, native.AddBatchChecked([]*graph.Node{{
					ID: caller, Name: caller, Kind: graph.KindContract, FilePath: ids[caller], Language: "contract",
				}}, nil))
			}
			want := rebuildGRPCNativeCalls(t, native, 1)
			i := slices.IndexFunc(want, func(e *graph.Edge) bool { return e.From == caller })
			require.NotEqual(t, -1, i)
			require.Equal(t, handler, want[i].To)

			b, _ := openRebuildGRPCFixture(t, facts)
			if len(ids) != 0 {
				require.NoError(t, b.BeginRebuildContractOverlay(t.Context(), ids))
				n, err := b.RebuildView().Node(t.Context(), caller)
				require.NoError(t, err)
				require.Equal(t, graph.KindContract, n.Kind)
			}
			before := rebuildGRPCSavedCalls(t, b)
			proof := facts
			var mixed *graph.Node
			switch mode {
			case "wrong old key", "wrong service", "wrong method":
				e := *stub
				e.Meta = maps.Clone(stub.Meta)
				switch mode {
				case "wrong old key":
					e.Line++
				case "wrong service":
					e.Meta["grpc_service"] = "Alias"
				case "wrong method":
					e.Meta["grpc_method"] = "World"
				}
				proof.Edges = slices.Clone(facts.Edges)
				proof.Edges[slices.Index(proof.Edges, stub)] = &e
			case "contract caller without recorder":
				proof.Edges = slices.DeleteFunc(slices.Clone(facts.Edges), func(e *graph.Edge) bool { return e == stub })
			case "missing caller", "missing elected handler", "contract caller without handler":
				omit := caller
				if mode != "missing caller" {
					omit = handler
				}
				proof.Nodes = slices.DeleteFunc(slices.Clone(facts.Nodes), func(n *graph.Node) bool { return n.ID == omit })
			case "mixed handler evidence", "mismatched live attributes":
				proof.Nodes = slices.Clone(facts.Nodes)
				i := slices.IndexFunc(proof.Nodes, func(n *graph.Node) bool { return n.ID == handler })
				require.NotEqual(t, -1, i)
				n := *proof.Nodes[i]
				if mode == "mixed handler evidence" {
					mixed = &n
				} else {
					n.RepoPrefix = "receipt-only"
					proof.Nodes[i] = &n
				}
			}
			proof.File.NodeCount = len(proof.Nodes)
			receipts := []store_pebble.ParseFacts{proof}
			if mixed != nil {
				receipts = append(receipts, store_pebble.ParseFacts{
					EffectiveLanguage: "python", Nodes: []*graph.Node{mixed},
					File: graph.FileMetaRow{FilePath: rebuildGRPCFixtureFile, NodeCount: 1},
				})
			}
			selection := AllFrameworkSynthesizers().WithRebuildEarlyContractIDs(ids)
			stats, err := RunRebuildGRPC(t.Context(), b, rebuildGRPCReceipts(receipts...), selection)
			got := rebuildGRPCSavedCalls(t, b)
			if mode == "complete proof" || mode == "contract caller" {
				require.NoError(t, err)
				require.Equal(t, uint64(1), stats.Moves)
				require.Equal(t, want, got)
				return
			}
			require.ErrorIs(t, err, store_pebble.ErrRebuildGRPC)
			require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
			var refused *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, uint64(1), refused.Count)
			require.Equal(t, graph.EdgeIdentityFor(want[i]), refused.First)
			require.Zero(t, stats)
			require.Equal(t, before, got)
			require.False(t, slices.ContainsFunc(got, func(e *graph.Edge) bool {
				return e.From == caller && (e.To == handler || e.To == runnerUp)
			}))
		})
	}
}

func TestRebuildGRPCEarlyContractFallback(t *testing.T) {
	const impl, contract = "p/f.go::other", "env::WORLD"
	facts := rebuildGRPCFixture(0)
	facts.Nodes = slices.DeleteFunc(facts.Nodes, func(n *graph.Node) bool { return n.ID == impl })
	facts.File.NodeCount = len(facts.Nodes)
	for _, e := range facts.Edges {
		if e.Kind == graph.EdgeImplements && e.From == impl {
			e.From = contract
		}
		if e.Kind == graph.EdgeMemberOf && e.To == impl {
			e.To = contract
		}
	}
	for _, tc := range []struct {
		name string
		ids  map[string]string
		want int
	}{
		{name: "built-empty", ids: map[string]string{}, want: 4},
		{name: "private deferred-only id", ids: map[string]string{contract: "settings.rb"}, want: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			native := rebuildGRPCNative(t, facts)
			for id, file := range tc.ids {
				require.NoError(t, native.AddBatchChecked([]*graph.Node{{
					ID: id, Name: id, Kind: graph.KindContract, FilePath: file, Language: "contract",
				}}, nil))
			}
			want := rebuildGRPCNativeCalls(t, native, tc.want)
			b, receipts := openRebuildGRPCFixture(t, facts)
			require.NoError(t, b.BeginRebuildContractOverlay(t.Context(), tc.ids))
			selection := AllFrameworkSynthesizers().WithRebuildEarlyContractIDs(tc.ids)
			selection, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
			require.NoError(t, err)
			s, err := RunRebuildGRPC(t.Context(), b, receipts, selection)
			require.NoError(t, err)
			require.Equal(t, uint64(tc.want), s.Resolved)
			require.Equal(t, want, rebuildGRPCSavedCalls(t, b))
			require.NoError(t, b.EndRebuildContractOverlay(t.Context()))
			n, err := b.RebuildView().Node(t.Context(), contract)
			require.NoError(t, err)
			require.Nil(t, n)
		})
	}
}

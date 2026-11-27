package resolver

import (
	"cmp"
	"fmt"
	"iter"
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
	winner.Meta["unrelated"] = "first candidate"
	loser := stub("z::another-old-target", "Hello", 10)
	loser.Meta["unrelated"] = "second candidate"
	incumbent := edge(caller, first+".Hello", graph.EdgeCalls, 14, map[string]any{"unrelated": "stationary destination"})
	edges := []*graph.Edge{
		edge(reg, "unresolved::RegisterGreeterServer", graph.EdgeCalls, 5,
			map[string]any{"grpc_register_service": "Greeter", "grpc_register_impl": "server"}),
		edge(first+".Hello", first, graph.EdgeMemberOf, 1, nil), edge(second+".Hello", second, graph.EdgeMemberOf, 1, nil),
		edge(fallback+".World", fallback, graph.EdgeMemberOf, 1, nil), edge(skipped+".World", skipped, graph.EdgeMemberOf, 1, nil),
		edge(fallback, iface, graph.EdgeImplements, 2, nil), edge(skipped, iface, graph.EdgeImplements, 2, nil),
		winner, loser, stub(grpcStubPlaceholder("Greeter", "World"), "World", 11), old, stationary,
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
	b, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "working"), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	require.NoError(t, b.AddParsedFile("", facts))
	return b, func(yield func(store_pebble.ParseFacts, error) bool) { yield(facts, nil) }
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
	return got
}

func TestRebuildGRPCNativeRowsAndSpill(t *testing.T) {
	for _, spillAt := range []int{1, 1 << 20} {
		t.Run(fmt.Sprint(spillAt), func(t *testing.T) {
			var extra int
			if spillAt == 1<<20 {
				extra = 270
			}
			facts := rebuildGRPCFixture(extra)
			native, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, native.Close()) })
			// SQLite orders the census by edge ID. The same-site moves in
			// this arm are inserted in approved complete old-key order.
			require.NoError(t, native.AddBatchChecked(facts.Nodes, facts.Edges))
			selection, err := NewFrameworkSynthesizerSelection([]string{SynthGRPCStub})
			require.NoError(t, err)
			report := RunFrameworkSynthesizersWithSelection(native, selection)
			require.NoError(t, report.Err)
			require.Equal(t, 5+extra, report.Total)
			want := slices.Collect(native.EdgesByKind(graph.EdgeCalls))
			slices.SortFunc(want, rebuildGRPCCallLess)
			// Reverse private arrival. Election must depend on the complete
			// old key, not on ingestion or spill order.
			slices.Reverse(facts.Nodes)
			slices.Reverse(facts.Edges)
			b, receipts := openRebuildGRPCFixture(t, facts)
			stats, err := RunRebuildGRPC(t.Context(), b, receipts, selection, spillAt)
			require.NoError(t, err)
			require.Equal(t, uint64(6+extra), stats.Stubs)
			require.Equal(t, uint64(1), stats.Registrations)
			require.Equal(t, uint64(5+extra), stats.Resolved)
			require.Equal(t, uint64(5+extra), stats.Moves)
			require.Positive(t, stats.SpillBytes)
			got := rebuildGRPCSavedCalls(t, b)
			require.Equal(t, want, got)
			at := make(map[int]*graph.Edge)
			for _, e := range got {
				at[e.Line] = e
			}
			require.Equal(t, "p/a.go::srv.Hello", at[10].To)
			require.Equal(t, "first candidate", at[10].Meta["unrelated"])
			require.Equal(t, "p/f.go::other.World", at[11].To)
			require.Equal(t, graph.OriginASTInferred, at[11].Origin)
			require.Equal(t, grpcStubPlaceholder("Greeter", "Gone"), at[12].To)
			require.Empty(t, at[12].Origin)
			require.NotContains(t, at[12].Meta, "grpc_resolution")
			require.Equal(t, "keep stationary", at[13].Meta["unrelated"])
			require.Equal(t, "stationary destination", at[14].Meta["unrelated"])
			again, err := RunRebuildGRPC(t.Context(), b, receipts, selection, spillAt)
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
			}, selection, 1)
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

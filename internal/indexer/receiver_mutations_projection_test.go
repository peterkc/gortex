package indexer

import (
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	store_sqlite "github.com/zzet/gortex/internal/graph/store_sqlite"
)

type receiverMutationFallbackStore struct {
	graph.Store
}

func TestIndirectMutationProjectionMatchesFullDecode(t *testing.T) {
	s := receiverMutationSQLiteFixture(t, 0)

	want := indirectMutationEdges(receiverMutationFallbackStore{Store: s})
	got := indirectMutationEdges(s)
	sortIndirectMutationSpecs(want)
	sortIndirectMutationSpecs(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("projected result differs from full decode:\n got: %#v\nwant: %#v", got, want)
	}
	if len(got) != 3 {
		t.Fatalf("indirect mutations=%d, want 3: %#v", len(got), got)
	}
}

func BenchmarkIndirectMutationEdgesProjection(b *testing.B) {
	s := receiverMutationSQLiteFixture(b, 20000)
	b.Run("full_decode", func(b *testing.B) {
		fallback := receiverMutationFallbackStore{Store: s}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if got := indirectMutationEdges(fallback); len(got) != 3 {
				b.Fatalf("indirect mutations=%d, want 3", len(got))
			}
		}
	})
	b.Run("compact_projection", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if got := indirectMutationEdges(s); len(got) != 3 {
				b.Fatalf("indirect mutations=%d, want 3", len(got))
			}
		}
	})
}

type testFataler interface {
	Helper()
	Fatal(...any)
	Cleanup(func())
}

func receiverMutationSQLiteFixture(t testFataler, noiseCalls int) *store_sqlite.Store {
	t.Helper()
	tempDir, ok := any(t).(interface{ TempDir() string })
	if !ok {
		t.Fatal("test helper does not provide TempDir")
	}
	s, err := store_sqlite.Open(filepath.Join(tempDir.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	nodes := []*graph.Node{
		{ID: "method::set", Kind: graph.KindMethod, Name: "Set", FilePath: "type.go", Language: "go", Meta: map[string]any{"receiver": "Type"}},
		{ID: "method::wrap", Kind: graph.KindMethod, Name: "Wrap", FilePath: "type.go", Language: "go", Meta: map[string]any{"receiver": "Type"}},
		{ID: "method::outer", Kind: graph.KindMethod, Name: "Outer", FilePath: "type.go", Language: "go", Meta: map[string]any{"receiver": "Type"}},
		{ID: "method::append", Kind: graph.KindMethod, Name: "AppendItems", FilePath: "type.go", Language: "go", Meta: map[string]any{"receiver": "Type"}},
		{ID: "field::items", Kind: graph.KindField, Name: "items", FilePath: "type.go", Language: "go", Meta: map[string]any{"receiver": "Type"}},
	}
	edges := []*graph.Edge{
		{From: "method::set", To: "field::items", Kind: graph.EdgeWrites, FilePath: "type.go", Line: 3},
		{From: "method::wrap", To: "method::set", Kind: graph.EdgeCalls, FilePath: "type.go", Line: 7, Meta: map[string]any{"recv_self": true}},
		{From: "method::outer", To: "method::wrap", Kind: graph.EdgeCalls, FilePath: "type.go", Line: 11, Meta: map[string]any{"recv_self": true}},
		{From: "method::append", To: "external::Lock", Kind: graph.EdgeCalls, FilePath: "type.go", Line: 15, Meta: map[string]any{"recv_field": "items"}},
	}
	for i := 0; i < noiseCalls; i++ {
		edges = append(edges, &graph.Edge{
			From: "noise::caller", To: fmt.Sprintf("external::noise::%d", i),
			Kind: graph.EdgeCalls, FilePath: "noise.go", Line: i + 1,
			Meta: map[string]any{"return_usage": "assigned", "index": i},
		})
	}
	s.AddBatch(nodes, edges)
	return s
}

func sortIndirectMutationSpecs(specs []indirectMutSpec) {
	sort.Slice(specs, func(i, j int) bool {
		left, right := specs[i], specs[j]
		if left.from != right.from {
			return left.from < right.from
		}
		if left.to != right.to {
			return left.to < right.to
		}
		if left.file != right.file {
			return left.file < right.file
		}
		if left.line != right.line {
			return left.line < right.line
		}
		return left.via < right.via
	})
}

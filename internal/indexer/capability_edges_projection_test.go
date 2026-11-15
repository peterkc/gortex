package indexer

import (
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type capabilityEdgeSnapshot struct {
	From, To, FilePath, Origin, Access string
	Kind                               graph.EdgeKind
	Line                               int
}

func TestScopedCapabilityProjectionMatchesInMemoryAndPreservesProvenance(t *testing.T) {
	memory := graph.New()
	sqlite, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlite.Close() })

	seed := func(store graph.Store) {
		store.AddBatch([]*graph.Node{
			{ID: "a-src", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "source.go"},
			{ID: "b-src", Kind: graph.KindFunction, RepoPrefix: "b", FilePath: "other.go"},
			{ID: "field", Kind: graph.KindField, RepoPrefix: "definitions", FilePath: "field.go"},
			{ID: "not-field", Kind: graph.KindFunction, RepoPrefix: "definitions", FilePath: "fn.go"},
		}, []*graph.Edge{
			// Reverse lexical insertion order exercises SQLite id-keyset paging.
			{From: "a-src", To: "field", Kind: graph.EdgeReads, FilePath: "z.go", Line: 20},
			{From: "a-src", To: "field", Kind: graph.EdgeWrites, FilePath: "0.go", Line: 1},
			{From: "a-src", To: "field", Kind: graph.EdgeReads, FilePath: "a.go", Line: 2},
			{From: "a-src", To: "not-field", Kind: graph.EdgeReads, FilePath: "a.go", Line: 3},
			{From: "a-src", To: "cfg::env::TOKEN", Kind: graph.EdgeReadsConfig, FilePath: "z.go", Line: 4},
			{From: "a-src", To: "cfg::env::TOKEN", Kind: graph.EdgeReadsConfig, FilePath: "a.go", Line: 5},
			{From: "a-src", To: "unresolved::exec.Command", Kind: graph.EdgeCalls, FilePath: "z.go", Line: 6},
			{From: "a-src", To: "unresolved::exec.Command", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 7},
			{From: "b-src", To: "field", Kind: graph.EdgeReads, FilePath: "b.go", Line: 8},
		})
	}
	seed(memory)
	seed(sqlite)

	memoryReads, memoryExec, memoryFields, memoryErr := synthesizeCapabilityEdgesScoped(memory, map[string]bool{"a": true})
	if memoryErr != nil {
		t.Fatal(memoryErr)
	}
	sqliteReads, sqliteExec, sqliteFields, sqliteErr := synthesizeCapabilityEdgesScoped(sqlite, map[string]bool{"a": true})
	if sqliteErr != nil {
		t.Fatal(sqliteErr)
	}
	memoryCounts := [3]int{memoryReads, memoryExec, memoryFields}
	sqliteCounts := [3]int{sqliteReads, sqliteExec, sqliteFields}
	if memoryCounts != sqliteCounts {
		t.Fatalf("counts differ: memory=%v sqlite=%v", memoryCounts, sqliteCounts)
	}

	snapshot := func(store graph.Store) []capabilityEdgeSnapshot {
		var out []capabilityEdgeSnapshot
		for _, kind := range []graph.EdgeKind{
			graph.EdgeReadsEnv, graph.EdgeExecutesProcess, graph.EdgeAccessesField,
		} {
			for edge := range store.EdgesByKind(kind) {
				access, _ := edge.Meta["access"].(string)
				out = append(out, capabilityEdgeSnapshot{
					From: edge.From, To: edge.To, Kind: edge.Kind,
					FilePath: edge.FilePath, Line: edge.Line,
					Origin: edge.Origin, Access: access,
				})
			}
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Kind != out[j].Kind {
				return out[i].Kind < out[j].Kind
			}
			return out[i].To < out[j].To
		})
		return out
	}
	// Given exact scoped eviction on both backends, when a duplicate file
	// frontier repeats, then counts/provenance match and unrelated sources stay untouched.
	for _, store := range []graph.Store{memory, sqlite} {
		for range 2 {
			re, ep, fa, err := synthesizeCapabilityEdgesScoped(store, map[string]bool{"a": true}, "source.go", "", "source.go")
			if err != nil || [3]int{re, ep, fa} != memoryCounts {
				t.Fatalf("repeated scoped counts = (%d,%d,%d), %v, want %v", re, ep, fa, err, memoryCounts)
			}
			for _, edge := range store.GetOutEdges("b-src") {
				if edge.Kind != graph.EdgeReads {
					t.Fatalf("changed unrelated source: %+v", edge)
				}
			}
		}
	}
	memoryEdges, sqliteEdges := snapshot(memory), snapshot(sqlite)
	if !reflect.DeepEqual(memoryEdges, sqliteEdges) {
		t.Fatalf("derived edges differ:\nmemory: %#v\nsqlite: %#v", memoryEdges, sqliteEdges)
	}

	wantProvenance := map[graph.EdgeKind]struct {
		file, access string
		line         int
	}{
		graph.EdgeAccessesField:   {file: "a.go", line: 2, access: "read"},
		graph.EdgeReadsEnv:        {file: "a.go", line: 5},
		graph.EdgeExecutesProcess: {file: "a.go", line: 7},
	}
	if len(sqliteEdges) != len(wantProvenance) {
		t.Fatalf("derived edges = %d, want %d: %#v", len(sqliteEdges), len(wantProvenance), sqliteEdges)
	}
	for _, edge := range sqliteEdges {
		want := wantProvenance[edge.Kind]
		if edge.FilePath != want.file || edge.Line != want.line || edge.Access != want.access {
			t.Errorf("%s provenance = %s:%d access=%q, want %s:%d access=%q",
				edge.Kind, edge.FilePath, edge.Line, edge.Access, want.file, want.line, want.access)
		}
	}
}

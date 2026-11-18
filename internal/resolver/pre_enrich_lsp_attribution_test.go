package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestPreEnrichFallbackAllowsOnlyFullResolverLSPAttribution(t *testing.T) {
	build := func() (*graph.Graph, *fakeLSPHelper) {
		g := graph.New()
		g.AddBatch([]*graph.Node{
			{ID: "src/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "src/caller.ts", Language: "typescript"},
			{ID: "src/target.ts", Kind: graph.KindFile, Name: "target.ts", FilePath: "src/target.ts", Language: "typescript"},
			{ID: "src/caller.ts::callIt", Kind: graph.KindFunction, Name: "callIt", FilePath: "src/caller.ts", Language: "typescript", StartLine: 3, EndLine: 5},
			{ID: "src/target.ts::doWork", Kind: graph.KindFunction, Name: "doWork", FilePath: "src/target.ts", Language: "typescript", StartLine: 7, EndLine: 9},
		}, []*graph.Edge{
			{From: "src/caller.ts::callIt", To: "unresolved::doWork", Kind: graph.EdgeCalls, FilePath: "src/caller.ts", Line: 4},
			{From: "src/caller.ts", To: "src/target.ts", Kind: graph.EdgeImports, FilePath: "src/caller.ts", Line: 1},
		})
		return g, &fakeLSPHelper{exts: []string{".ts"}, defs: map[lspKey]lspAnswer{
			{path: "src/caller.ts", line: 4, name: "doWork"}: {defPath: "src/target.ts", defLine: 7},
		}}
	}
	original, oldLSP := build()
	old := New(&preEnrichRefusalProbeStore{Store: original})
	old.SetLSPHelper(oldLSP)
	stats := old.ResolveFilesAndIncoming([]string{"src/caller.ts"})
	if !stats.IncomingAdmissionRefused {
		t.Fatal("fixture did not refuse")
	}
	fallback := New(original)
	fallback.SetLSPHelper(oldLSP)
	fallbackStats := fallback.ResolveAll()
	if fallbackStats.PendingBefore != 0 {
		t.Fatalf("base fallback rescanned %d pending edges, want 0 after exact binding", fallbackStats.PendingBefore)
	}

	skipped, newLSP := build()
	full := New(skipped)
	full.SetLSPHelper(newLSP)
	full.ResolveAll()
	a, b := original.GetOutEdges("src/caller.ts::callIt"), skipped.GetOutEdges("src/caller.ts::callIt")
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("call edge counts: base=%d full=%d", len(a), len(b))
	}
	if oldLSP.calls != 1 || newLSP.calls != oldLSP.calls {
		t.Fatalf("Definition calls: base=%d full=%d", oldLSP.calls, newLSP.calls)
	}
	if b[0].Meta["resolution"] != "import_closure" {
		t.Fatalf("full resolver attribution = %v", b[0].Meta)
	}
	if _, present := a[0].Meta["resolution"]; present {
		t.Fatalf("base exact path unexpectedly has heuristic attribution: %v", a[0].Meta)
	}
	delete(b[0].Meta, "resolution")
	require.ElementsMatch(t, original.AllEdges(), skipped.AllEdges(), "only the heuristic resolution attribution may differ")
}

type preEnrichRefusalProbeStore struct{ graph.Store }

func (s *preEnrichRefusalProbeStore) GetInEdgesByNodeIDs(keys []string) map[string][]*graph.Edge {
	rows := s.Store.GetInEdgesByNodeIDs(keys)
	for _, key := range keys {
		if key == "unresolved::callIt" {
			row := &graph.Edge{From: "caller", To: key, Kind: graph.EdgeCalls}
			for range graph.MaxIncomingSourceCandidateRows + 1 {
				rows[key] = append(rows[key], row)
			}
		}
	}
	return rows
}

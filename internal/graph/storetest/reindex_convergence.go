package storetest

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// RunReindexConvergencePayload pins the payload and receipt rule shared by stores.
func RunReindexConvergencePayload(t *testing.T, factory Factory) {
	for _, shape := range []string{"single_stationary", "retarget_stationary", "retarget_refreshed", "refresh_stationary", "two_movers"} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reverse=%t", shape, reverse), func(t *testing.T) {
				s := factory(t)
				s.AddNode(&graph.Node{ID: "caller", FilePath: "a.go", RepoPrefix: "repo", Kind: graph.KindFunction})
				destination := graph.Edge{From: "caller", To: "unresolved::Target", Kind: graph.EdgeCalls,
					FilePath: "a.go", Line: 10, Origin: "stationary", Confidence: 0.9,
					Meta: map[string]any{"payload": "stationary"}}
				moving := destination
				moving.To, moving.Origin, moving.Confidence = "old::First", "mover", 0.2
				moving.Meta = map[string]any{"payload": "mover"}
				if shape == "refresh_stationary" {
					moving.To, moving.Line = destination.To, 20
				}
				candidate := moving
				candidate.To, candidate.Line = destination.To, destination.Line
				batch := []graph.EdgeReindex{{Edge: &candidate, OldTo: moving.To}}
				want := destination
				wantWork := false
				s.AddEdge(&moving)
				if shape == "two_movers" {
					second := moving
					second.To, second.Origin = "old::Second", "second"
					second.Meta = map[string]any{"payload": "second"}
					s.AddEdge(&second)
					next := second
					next.To = destination.To
					batch = append(batch, graph.EdgeReindex{Edge: &next, OldTo: second.To})
					want, wantWork = candidate, true
					if reverse {
						want = next
					}
				} else {
					s.AddEdge(&destination)
				}
				if shape == "retarget_refreshed" {
					refreshed := destination
					refreshed.Origin = "refreshed"
					refreshed.Meta = map[string]any{"payload": "refreshed"}
					batch = append(batch, graph.EdgeReindex{Edge: &refreshed, OldTo: destination.To,
						RefreshIdentity: true, OldFilePath: destination.FilePath, OldLine: destination.Line})
					want, wantWork = refreshed, true
				}
				if shape == "refresh_stationary" {
					batch[0].RefreshIdentity = true
					batch[0].OldFilePath, batch[0].OldLine = moving.FilePath, moving.Line
				}
				if reverse && len(batch) == 2 {
					batch[0], batch[1] = batch[1], batch[0]
				}
				receipts := s.(graph.MutationReceiptStore)
				token := receipts.BeginMutationReceipt()
				if shape == "single_stationary" {
					s.ReindexEdge(batch[0].Edge, batch[0].OldTo)
				} else {
					s.ReindexEdges(batch)
				}
				receipt := receipts.EndMutationReceipt(token)
				for name, rows := range map[string][]*graph.Edge{
					"all": s.AllEdges(), "out": s.GetOutEdges("caller"), "in": s.GetInEdges(destination.To),
				} {
					if len(rows) != 1 {
						t.Errorf("%s row count = %d, want 1", name, len(rows))
						continue
					}
					if !reflect.DeepEqual(*rows[0], want) {
						t.Errorf("%s payload = %+v, want %+v", name, *rows[0], want)
					}
				}
				if got := s.GetInEdges("old::First"); len(got) != 0 {
					t.Errorf("old incoming rows = %+v", got)
				}
				if got := s.GetInEdges("old::Second"); len(got) != 0 {
					t.Errorf("second old incoming rows = %+v", got)
				}
				if err := s.VerifyEdgeIdentities(); err != nil {
					t.Fatal(err)
				}
				var wantFiles []string
				if wantWork {
					wantFiles = []string{"a.go"}
				}
				if !reflect.DeepEqual(receipt.UnresolvedFiles, wantFiles) || !reflect.DeepEqual(receipt.ChangedFiles, wantFiles) {
					t.Errorf("receipt = %+v, want winner files %v", receipt, wantFiles)
				}
			})
		}
	}
}

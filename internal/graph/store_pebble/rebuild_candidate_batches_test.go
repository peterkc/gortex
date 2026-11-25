package store_pebble

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildCandidateBatchAdmissionBounds(t *testing.T) {
	req := LinkRequest{Kind: LinkName, Key: "A", RepoPrefix: "repo"}
	for _, tc := range []struct {
		name   string
		charge CandidateBatchCharge
		spill  int
	}{
		{"rows", CandidateBatchCharge{Rows: linkResultRows/2 + 1, Bytes: linkResultRows * 30}, 1 << 20},
		{"bytes", CandidateBatchCharge{Rows: 1, Bytes: linkResultBytes/2 + 1}, 1 << 20},
		{"final-drain", CandidateBatchCharge{Rows: goldFileCap/2 + 1, Bytes: goldFileCap * 40}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var a, b CandidateBatchLimit
			if !a.Add(req, tc.charge, tc.spill) || a.Add(req, tc.charge, tc.spill) || !b.Add(req, tc.charge, tc.spill) {
				t.Fatal("scalar-safe aggregate must split before dispatch")
			}
		})
	}
	big := LinkRequest{Kind: LinkName, Key: strings.Repeat("a", 1100<<10)}
	var a, b CandidateBatchLimit
	if !a.Add(big, CandidateBatchCharge{}, 1<<20) || a.Add(big, CandidateBatchCharge{}, 1<<20) || !b.Add(big, CandidateBatchCharge{}, 1<<20) {
		t.Fatal("aggregate wire cap")
	}
	if a.Add(LinkRequest{Kind: LinkName, Key: strings.Repeat("z", 2<<20)}, CandidateBatchCharge{}, 1<<20) {
		t.Fatal("scalar wire oversize grouped")
	}
}

func TestRebuildCandidateBatchCharge(t *testing.T) {
	b := newBuild(t, FullProfile)
	nodes := []*graph.Node{{ID: "a", Name: "A", QualName: "pkg.A", RepoPrefix: "repo", Language: "go", Meta: map[string]any{"x": "value"}}, {ID: "b", Name: "A", QualName: "pkg.A", RepoPrefix: "repo", Language: "python"}, {ID: "c", Name: "A", QualName: "pkg.A", RepoPrefix: "else", Language: "go"}}
	b.AddBatch(nodes, nil)
	pass, ctx := startLinkFixture(t, RebuildLinkOptions{Partitions: 2, SpillBytes: 1 << 20}, "ordinary", nodes)
	for _, req := range []LinkRequest{{Kind: LinkName, Key: "A", RepoPrefix: "repo", Languages: []string{"go"}}, {Kind: LinkName, Key: "A", RepoPrefix: "repo"}, {Kind: LinkName, Key: "A", AllRepos: true}, {Kind: LinkQualName, Key: "pkg.A", AllRepos: true}, {Kind: LinkName, Key: "missing"}} {
		charge, err := b.RebuildView().ChargeCandidateRequest(ctx, req)
		if err != nil || charge.Oversize {
			t.Fatalf("charge %+v: %v", charge, err)
		}
		stats, err := pass.SelectCandidatePage(ctx, b.RebuildView(), []LinkRequest{req}, 1<<20, func(int, *graph.Node) error { return nil })
		if err != nil || charge.Rows != stats.ResultRows || charge.Bytes != stats.ResultBytes {
			t.Fatalf("charge=%+v stats=%+v err=%v", charge, stats, err)
		}
	}
}

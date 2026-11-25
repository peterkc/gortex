package store_pebble

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
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
	pass, ctx := startLinkFixture(t, b.RebuildView(), nodes)
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

// This is the H97 admission oracle: checked candidate view, full decode,
// exact request filter, canonical reencode and native-order key size.
func oldRebuildCandidateCharge(ctx context.Context, v RebuildView, req LinkRequest) (CandidateBatchCharge, error) {
	var out CandidateBatchCharge
	visit := func(n *graph.Node) error {
		if !req.match(n) {
			return nil
		}
		raw, err := encodeNode(n)
		if err != nil {
			return err
		}
		out.Rows++
		out.Bytes += len(raw) + len(candidateOrderKey(req, 0, n, 0))
		return nil
	}
	var err error
	if req.Kind == LinkName {
		err = v.VisitNameCandidates(ctx, req.Key, req.Languages, visit)
	} else {
		err = v.VisitQualNameCandidates(ctx, req.Key, visit)
	}
	return out, err
}

func TestRebuildCandidateChargeCanonicalSize(t *testing.T) {
	b := newBuild(t, FullProfile)
	nodes := []*graph.Node{
		{ID: "a\x00b", Name: "N", QualName: "q.N", RepoPrefix: "r", Language: "go", Meta: map[string]any{"confidence": json.Number("0.5"), "nested": map[string]json.Number{"score": json.Number("2")}}},
		{ID: "b", Name: "N", QualName: "q.N", RepoPrefix: "r", Language: "python", Meta: map[string]any{"description": "python"}},
		{ID: "c", Name: "N", QualName: "q.N", RepoPrefix: "other", Language: "go"},
	}
	// JSON fallback bytes are not the canonical native encoding. The test
	// must exercise the distinction, not only the common round-trip case.
	raw, err := encodeNode(nodes[0])
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeNode(raw)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := encodeNode(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == len(canonical) {
		t.Fatal("fixture did not change canonical encoded length")
	}
	b.AddBatch(nodes, nil)
	ctx := context.Background()
	view := b.RebuildView()
	requests := []LinkRequest{
		{Kind: LinkName, Key: "N", RepoPrefix: "r", Languages: []string{"go"}},
		{Kind: LinkName, Key: "N", RepoPrefix: "r"},
		{Kind: LinkName, Key: "N", AllRepos: true, Languages: []string{"go", "python"}},
		{Kind: LinkName, Key: "N", AllRepos: true},
		{Kind: LinkQualName, Key: "q.N", AllRepos: true},
		{Kind: LinkName, Key: "missing"},
	}
	check := func(req LinkRequest) CandidateBatchCharge {
		t.Helper()
		old, err := oldRebuildCandidateCharge(ctx, view, req)
		if err != nil {
			t.Fatal(err)
		}
		got, err := view.ChargeCandidateRequest(ctx, req)
		if err != nil || got != old {
			t.Fatalf("request %+v: got %+v old %+v err %v", req, got, old, err)
		}
		return got
	}
	var frozen []*graph.Node
	if err := view.VisitNodes(ctx, func(n *graph.Node) error { frozen = append(frozen, n); return nil }); err != nil {
		t.Fatal(err)
	}
	pass, passCtx := startLinkFixture(t, view, frozen)
	for _, req := range requests {
		charge := check(req)
		stats, err := pass.SelectCandidatePage(passCtx, view, []LinkRequest{req}, 1<<20, func(int, *graph.Node) error { return nil })
		if err != nil || charge.Rows != stats.ResultRows || charge.Bytes != stats.ResultBytes {
			t.Fatalf("actual charge %+v vs %+v: %v", charge, stats, err)
		}
	}
	beforeName, beforeQual := check(requests[0]), check(requests[4])
	changed := *nodes[0]
	changed.Meta = map[string]any{"description": strings.Repeat("updated", 1000)}
	b.AddNode(&changed) // both candidate keys are byte-identical
	afterName, afterQual := check(requests[0]), check(requests[4])
	if beforeName.Rows != afterName.Rows || beforeQual.Rows != afterQual.Rows || afterName.Bytes <= beforeName.Bytes || afterQual.Bytes <= beforeQual.Bytes {
		t.Fatal("same-key payload update did not update both charges")
	}
	live := check(requests[3])
	stats, err := pass.SelectCandidatePage(passCtx, view, []LinkRequest{requests[3]}, 1<<20, func(int, *graph.Node) error { return nil })
	if err != nil || stats.ResultBytes != live.Bytes || stats.ResultRows != live.Rows {
		t.Fatalf("updated live charge %+v vs %+v: %v", live, stats, err)
	}
}

func TestRebuildCandidateChargeUnsizedAndIndexOnly(t *testing.T) {
	b := newBuild(t, FullProfile)
	n := &graph.Node{ID: "a", Name: "N", QualName: "q.N", RepoPrefix: "r", Language: "go"}
	other := &graph.Node{ID: "b", Name: "N", RepoPrefix: "r", Language: "python"}
	b.AddBatch([]*graph.Node{n, other}, nil)
	pass, ctx := startLinkFixture(t, b.RebuildView(), []*graph.Node{n, other})
	view := b.RebuildView()
	live := LinkRequest{Kind: LinkName, Key: "N", AllRepos: true, Languages: []string{"go"}}
	if err := b.pending.Set(rebuildCandidateKey(nameRebuildScoped, n), []byte(n.ID), pebble.NoSync); err != nil {
		t.Fatal(err)
	}
	charge, err := view.ChargeCandidateRequest(ctx, live)
	var limit CandidateBatchLimit
	if err != nil || !charge.Oversize || limit.Add(live, charge, 1<<20) {
		t.Fatalf("unsized request grouped: %+v %v", charge, err)
	}
	visits := 0
	stats, err := pass.SelectCandidatePage(ctx, view, []LinkRequest{live}, 1<<20, func(_ int, found *graph.Node) error {
		if found.ID != n.ID {
			t.Fatal(found.ID)
		}
		visits++
		return nil
	})
	if err != nil || visits != 1 || stats.ResultRows != 1 {
		t.Fatalf("original scalar request refused: %+v %v", stats, err)
	}
	// Qualified index lacks repo/language. Its filtered form remains scalar.
	filtered := LinkRequest{Kind: LinkQualName, Key: "q.N", RepoPrefix: "r", Languages: []string{"go"}}
	charge, err = view.ChargeCandidateRequest(ctx, filtered)
	if err != nil || !charge.Oversize {
		t.Fatalf("unsized qualified request: %+v %v", charge, err)
	}
	// Repair the index through ordinary write, then corrupt the node payload
	// behind it. Sizing must still read only the index; the actual live
	// selection must fail, never accept corrupt data as a checked miss.
	b.AddNode(n)
	want, err := view.ChargeCandidateRequest(ctx, live)
	if err != nil || want.Oversize {
		t.Fatal(want, err)
	}
	if err := b.pending.Set(pendingNodeKey(n.ID), []byte{0xff}, pebble.NoSync); err != nil {
		t.Fatal(err)
	}
	got, err := view.ChargeCandidateRequest(ctx, live)
	if err != nil || got != want {
		t.Fatalf("sizing read node payload: %+v want %+v err %v", got, want, err)
	}
	if _, err := pass.SelectCandidatePage(ctx, view, []LinkRequest{live}, 1<<20, func(int, *graph.Node) error { t.Fatal("corrupt node visited"); return nil }); err == nil {
		t.Fatal("actual candidate selection accepted corrupt node")
	}
}

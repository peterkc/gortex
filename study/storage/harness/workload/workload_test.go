package workload

import (
	"context"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"testing"
)

type wrongSegment struct{ adapter.Segment }

func (wrongSegment) NodeByID(context.Context, string) (dataset.Node, error) {
	return dataset.Node{ID: "wrong", Name: "wrong"}, nil
}
func TestWrongAdapterMismatch(t *testing.T) {
	q := Query{ID: "point", Repo: "r", Op: "id", Key: "right"}
	got, e := Evaluate(context.Background(), map[string]adapter.Segment{"r": wrongSegment{}}, nil, q)
	if e != nil {
		t.Fatal(e)
	}
	check := Compare(q, Answer{Node: &dataset.Node{ID: "right", Name: "right"}}, got)
	if !check.Mismatch || check.Detail == "" {
		t.Fatal("wrong adapter answer not reported", check)
	}
}
func TestRankedFailuresAndOverlap(t *testing.T) {
	q := Query{Op: "vector", K: 2, MinOverlap: .6}
	c := Compare(q, Answer{Hits: []adapter.Hit{{ID: "a"}, {ID: "b"}}}, Answer{Hits: []adapter.Hit{{ID: "a"}}})
	if !c.Mismatch || c.Overlap == nil || *c.Overlap != .5 {
		t.Fatal(c)
	}
	c = Compare(Query{Op: "symbols", K: 10}, Answer{Hits: []adapter.Hit{{ID: "a"}}}, Answer{})
	if !c.Mismatch || c.Overlap == nil || *c.Overlap != 0 {
		t.Fatal("empty ranked result passed", c)
	}
	c = Compare(Query{Op: "symbols", K: 2}, Answer{Hits: []adapter.Hit{{ID: "a"}, {ID: "b"}}}, Answer{Hits: []adapter.Hit{{ID: "a"}, {ID: "a"}}})
	if !c.Mismatch {
		t.Fatal("duplicate hits accepted")
	}
}

type wrongRanker struct{ adapter.Segment }

func (wrongRanker) SearchSymbols(context.Context, string, int) ([]adapter.Hit, error) {
	return []adapter.Hit{{ID: "a", Score: 1}, {ID: "b", Score: 0}}, nil
}
func TestRankValidationIsOutsideRawEvaluate(t *testing.T) {
	q := Query{Op: "symbols", Repo: "r", Key: "word", K: 1}
	got, err := Evaluate(context.Background(), map[string]adapter.Segment{"r": wrongRanker{}}, nil, q)
	if err != nil {
		t.Fatal("raw adapter call should not validate", err)
	}
	if err = Validate(q, got); err == nil {
		t.Fatal("oversized ranked answer not rejected outside timer")
	}
}

func TestRawEvaluateDoesNotSortEdges(t *testing.T) {
	s := rawEdges{}
	got, err := Evaluate(context.Background(), map[string]adapter.Segment{"r": s}, nil, Query{Op: "out", Repo: "r", Key: "a"})
	if err != nil || len(got.Edges) != 2 || got.Edges[0].To != "z" {
		t.Fatal("raw timed path canonicalized output", got, err)
	}
}

type rawEdges struct{ adapter.Segment }

func (rawEdges) OutEdges(context.Context, string, string) ([]dataset.Edge, error) {
	return []dataset.Edge{{From: "a", To: "z"}, {From: "a", To: "b"}}, nil
}

func TestExactRankRejectsExtraShortListHit(t *testing.T) {
	q := Query{Op: "symbols", K: 10, ExactRank: true}
	check := Compare(q, Answer{Hits: []adapter.Hit{{ID: "a", Score: 2}}}, Answer{Hits: []adapter.Hit{{ID: "a", Score: 2}, {ID: "extra", Score: 1}}})
	if !check.Mismatch || check.Overlap == nil || *check.Overlap != 1 {
		t.Fatal("exact short-list extras accepted", check)
	}
}

func TestRequiredVectorInsertCannotBeDropped(t *testing.T) {
	q := Query{Op: "vector", K: 3, MinOverlap: .5, RequiredIDs: []string{"inserted"}}
	want := Answer{Hits: []adapter.Hit{{ID: "inserted", Score: 1}, {ID: "a", Score: .9}, {ID: "b", Score: .8}}}
	got := Answer{Hits: []adapter.Hit{{ID: "a", Score: .9}, {ID: "b", Score: .8}, {ID: "c", Score: .7}}}
	if c := Compare(q, want, got); !c.Mismatch {
		t.Fatal("lost vector insertion passed", c)
	}
}
func TestExactCutoffUsesTrustedOracleScores(t *testing.T) {
	q := Query{Op: "vector", K: 2, ExactRank: true, CutoffTies: []adapter.Hit{{ID: "near", Score: .900001}}}
	want := Answer{Hits: []adapter.Hit{{ID: "a", Score: 1}, {ID: "b", Score: .9}}}
	got := Answer{Hits: []adapter.Hit{{ID: "a", Score: 1}, {ID: "near", Score: .900001}}}
	if c := Compare(q, want, got); c.Mismatch || c.Overlap == nil || *c.Overlap != .5 || !c.CutoffTie {
		t.Fatal("valid cutoff tie rejected", c)
	}
	q.CutoffTies = []adapter.Hit{{ID: "near", Score: .901}}
	if c := Compare(q, want, got); !c.Mismatch {
		t.Fatal("outside-band substitution accepted")
	}
	q.CutoffTies = nil
	if c := Compare(q, want, got); !c.Mismatch {
		t.Fatal("backend score was trusted without oracle evidence")
	}
}
func TestOracleEmptyRejectsNonemptyAtZeroFloor(t *testing.T) {
	c := Compare(Query{Op: "symbols", K: 10}, Answer{}, Answer{Hits: []adapter.Hit{{ID: "wrong", Score: 1}}})
	if !c.Mismatch {
		t.Fatal("absent term returned hits without failure")
	}
}
func TestMultiIncomingKeepsHighDegreeStrata(t *testing.T) {
	a := dataset.Dataset{Repo: "a"}
	b := dataset.Dataset{Repo: "b"}
	for target := 0; target < 8; target++ {
		for source := 0; source <= target; source++ {
			a.Edges = append(a.Edges, dataset.Edge{From: fmt.Sprintf("a%d", source), To: fmt.Sprintf("b%d", target), Kind: "calls", CrossRepo: true})
		}
	}
	qs := Generate([]dataset.Dataset{a, b}, 7)
	high := false
	for _, q := range qs {
		if q.Op == "multi_in" && q.Key == "b7" {
			high = true
		}
	}
	if !high {
		t.Fatal("cross-repository hub missing")
	}
}

package main

import (
	"context"
	"errors"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/conformance"
	"gxstudy/dataset"
	"gxstudy/runner"
	"gxstudy/workload"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blevesearch/bleve/v2"
)

type countedTextIndex = bleve.Index

type countedText struct {
	countedTextIndex
	calls int
	sizes []int
}

func (ix *countedText) SearchInContext(ctx context.Context, req *bleve.SearchRequest) (*bleve.SearchResult, error) {
	ix.calls++
	ix.sizes = append(ix.sizes, req.Size)
	return ix.countedTextIndex.SearchInContext(ctx, req)
}

type countedNames struct {
	baseResources
	requests []int
}

func (s *countedNames) NodesByName(ctx context.Context, name string, k int) ([]dataset.Node, error) {
	s.requests = append(s.requests, k)
	return s.baseResources.NodesByName(ctx, name, k)
}
func fileReplacement(d dataset.Dataset, path string) adapter.FileChange {
	c := adapter.FileChange{Path: path, Replacement: dataset.Dataset{Repo: d.Repo}}
	ids := map[string]bool{}
	for _, n := range d.Nodes {
		if n.FilePath == path {
			c.Replacement.Nodes = append(c.Replacement.Nodes, n)
			ids[n.ID] = true
		}
	}
	for _, e := range d.Edges {
		if e.FilePath == path {
			c.Replacement.Edges = append(c.Replacement.Edges, e)
		}
	}
	for _, f := range d.Files {
		if f.Path == path {
			c.Replacement.Files = append(c.Replacement.Files, f)
		}
	}
	for _, s := range d.Symbols {
		if ids[s.NodeID] {
			c.Replacement.Symbols = append(c.Replacement.Symbols, s)
		}
	}
	for _, v := range d.Content {
		if v.FilePath == path || ids[v.NodeID] {
			c.Replacement.Content = append(c.Replacement.Content, v)
		}
	}
	for _, v := range d.Vectors {
		if ids[v.NodeID] {
			c.Replacement.Vectors = append(c.Replacement.Vectors, v)
		}
	}
	return c
}
func TestCombinedDeltaViewsMatchCleanAcrossEightEdits(t *testing.T) {
	for _, format := range []string{"arrow", "parquet"} {
		t.Run(format, func(t *testing.T) {
			ctx := context.Background()
			d := conformance.Fixture()
			// Duplicate names, duplicate edges and a nested path exercise independent
			// row spaces and retained multiplicity, not only a changed marker.
			d.Nodes = append(d.Nodes, dataset.Node{ID: "alias", Name: "Alpha", FilePath: "g.go"})
			d.Edges = append(d.Edges, d.Edges[0])
			original := fileReplacement(d, "f.go")
			update := fileReplacement(d, "f.go")
			update.Replacement.Content = slices.Clone(update.Replacement.Content)
			update.Replacement.Content[0].Body += " editedmarker"
			renamed := adapter.FileChange{Path: "renamed/nested.go", Replacement: dataset.Dataset{Repo: d.Repo,
				Nodes:   []dataset.Node{{ID: "renamed", Name: "Alpha", FilePath: "renamed/nested.go"}},
				Symbols: []dataset.Symbol{{NodeID: "renamed", Tokens: "graph renamedmarker"}},
				Content: []dataset.Content{{ChunkID: "renamed#0", NodeID: "renamed", FilePath: "renamed/nested.go", Body: "graph renamedmarker"}},
				Vectors: []dataset.Vector{dataset.MakeVector("renamed", 7)}, Files: []dataset.File{{Path: "renamed/nested.go"}},
				Edges: []dataset.Edge{{From: "renamed", To: "outside", FilePath: "renamed/nested.go", Kind: "calls"}}}}
			remove := func(path string) adapter.FileChange {
				return adapter.FileChange{Path: path, Replacement: dataset.Dataset{Repo: d.Repo}}
			}
			changes := []adapter.FileChange{update, remove("f.go"), original, remove("f.go"), renamed, renamed, remove(renamed.Path), original}
			b := newBackend(format)
			root := t.TempDir()
			l, err := newLifecycle(root, b)
			if err != nil {
				t.Fatal(err)
			}
			defer l.close()
			ref, _, err := b.Build(ctx, d, filepath.Join(root, "base"))
			if err != nil {
				t.Fatal(err)
			}
			if err = l.publish(ctx, ref, nil); err != nil {
				t.Fatal(err)
			}
			for i, c := range changes {
				pinned, release, e := l.pin(ctx, d.Repo)
				if e != nil {
					t.Fatal(e)
				}
				next, _, e := b.BuildDelta(ctx, ref, c, filepath.Join(root, fmt.Sprintf("edit-%d", i+1)))
				if e != nil {
					t.Fatal(e)
				}
				if e = l.publish(ctx, next, &ref); e != nil {
					t.Fatal(e)
				}
				checkAll(t, pinned, d) // Publishing D' cannot mutate the old view or D.
				if e = release(); e != nil {
					t.Fatal(e)
				}
				d = adapter.ApplyChange(d, c)
				view, e := b.Open(ctx, next.Path, next)
				if e != nil {
					t.Fatal(e)
				}
				s := view.(*segment)
				checkAll(t, s, d)
				clean, _, e := b.Build(ctx, d, filepath.Join(root, fmt.Sprintf("clean-%d", i+1)))
				if e != nil {
					t.Fatal(e)
				}
				cleanView, e := b.Open(ctx, clean.Path, clean)
				if e != nil {
					t.Fatal(e)
				}
				for _, kind := range []string{"symbol", "content"} {
					search := s.SearchSymbols
					cleanSearch := cleanView.SearchSymbols
					indexes := &s.symbols
					if kind == "content" {
						search = s.SearchContent
						cleanSearch = cleanView.SearchContent
						indexes = &s.content
					}
					full, e := cleanSearch(ctx, "graph editedmarker renamedmarker", -1)
					if e != nil {
						t.Fatal(e)
					}
					wantIDs := hitIDs(full)
					slices.Sort(wantIDs)
					got, e := search(ctx, "graph editedmarker renamedmarker", -1)
					if e != nil {
						t.Fatal(e)
					}
					gotIDs := hitIDs(got)
					slices.Sort(gotIDs)
					if !slices.Equal(wantIDs, gotIDs) {
						t.Fatal(i+1, kind, wantIDs, gotIDs)
					}
					if i == 0 || i == 1 || i == 7 {
						baseCount := &countedText{countedTextIndex: indexes[0]}
						deltaCount := &countedText{countedTextIndex: indexes[1]}
						originalIndexes := *indexes
						*indexes = [2]bleve.Index{baseCount, deltaCount}
						hits, e := search(ctx, "graph editedmarker renamedmarker", 1)
						*indexes = originalIndexes
						if e != nil || baseCount.calls != 1 || deltaCount.calls != 1 || len(s.openedText) != 2 {
							t.Fatal("not exactly base plus D", i+1, kind, baseCount.calls, deltaCount.calls, e)
						}
						if len(hits) != min(1, len(full)) {
							t.Fatal("hidden top-k lost a visible hit", hits, full)
						}
						for _, h := range hits {
							if !slices.Contains(wantIDs, h.ID) {
								t.Fatal("hidden hit entered top-k", h)
							}
						}
						if baseCount.sizes[0] != 1+s.hiddenTextCount(kind) || deltaCount.sizes[0] != 1 {
							t.Fatal("wrong text overfetch", baseCount.sizes, deltaCount.sizes)
						}
					}
				}
				names := &countedNames{baseResources: s.external.(baseResources)}
				s.external = names
				for _, name := range []string{"Alpha", "Beta", "absent"} {
					for _, k := range []int{0, 1, 2, -1} {
						before := len(names.requests)
						got, e := s.NodesByName(ctx, name, k)
						if e != nil {
							t.Fatal(e)
						}
						want, e := cleanView.NodesByName(ctx, name, k)
						if e != nil {
							t.Fatal(e)
						}
						if !reflect.DeepEqual(got, want) && !(len(got) == 0 && len(want) == 0) {
							t.Fatal(name, k, want, got)
						}
						if k == 0 {
							if len(names.requests) != before {
								t.Fatal("zero k fetched rows")
							}
							continue
						}
						if names.requests[before] != overfetch(k, s.hiddenNames[name]) {
							t.Fatal("unbounded name fetch", name, k, names.requests[before], s.hiddenNames[name])
						}
					}
				}
				if e = errors.Join(cleanView.Close(), view.Close()); e != nil {
					t.Fatal(e)
				}
				ref = next
			}
		})
	}
}
func TestFailedPrivateDeltaIndexNeverPublishes(t *testing.T) {
	for _, format := range []string{"arrow", "parquet"} {
		t.Run(format, func(t *testing.T) {
			ctx := context.Background()
			d := conformance.Fixture()
			b := newBackend(format)
			root := t.TempDir()
			l, err := newLifecycle(root, b)
			if err != nil {
				t.Fatal(err)
			}
			defer l.close()
			ref, _, err := b.Build(ctx, d, filepath.Join(root, "base"))
			if err != nil {
				t.Fatal(err)
			}
			if err = l.publish(ctx, ref, nil); err != nil {
				t.Fatal(err)
			}
			change, err := runner.MakeChange(d)
			if err != nil {
				t.Fatal(err)
			}
			first, _, err := b.BuildDelta(ctx, ref, change, filepath.Join(root, "first"))
			if err != nil {
				t.Fatal(err)
			}
			if err = l.publish(ctx, first, &ref); err != nil {
				t.Fatal(err)
			}
			ref = first
			d = adapter.ApplyChange(d, change)
			change = adapter.FileChange{Path: "f.go", Replacement: dataset.Dataset{Repo: d.Repo}}
			errBuild := errors.New("injected D build failure")
			b.buildText = func(ctx context.Context, dir string, rows []row) error {
				if e := os.WriteFile(filepath.Join(dir, "partial-index"), []byte("private"), 0600); e != nil {
					return e
				}
				return errBuild
			}
			private := filepath.Join(root, "failed")
			next, _, err := b.BuildDelta(ctx, ref, change, private)
			if !errors.Is(err, errBuild) || next != (adapter.SegmentRef{}) {
				t.Fatal("failed build returned a publishable edit", next, err)
			}
			if _, err = os.Stat(filepath.Join(private, "seal.json")); !os.IsNotExist(err) {
				t.Fatal("failed D was sealed", err)
			}
			if err = l.publish(ctx, next, &ref); err == nil {
				t.Fatal("unsealed edit published")
			}
			old, release, err := l.pin(ctx, d.Repo)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if old.(*segment).ref != ref {
				t.Fatal("failed D build changed the published view", old.(*segment).ref, ref)
			}
			checkAll(t, old, d)
		})
	}
}
func TestReadVerdictsUseOperationLimitAndUnresolvedControl(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		base, wrapper, delta, limit float64
		status                      string
	}{
		{"floor-pass", 89000, 89000, 1050000, 1089000, "pass"},
		{"floor-miss", 89000, 89000, 1100000, 1089000, "miss"},
		{"ratio-pass", 20000000, 20000000, 21900000, 22000000, "pass"},
		{"ratio-miss", 20000000, 20000000, 22100000, 22000000, "miss"},
		{"floor-control-boundary", 89000, 1089000, 1050000, 1089000, "pass"},
		{"floor-control-unresolved", 89000, 1089001, 1050000, 1089000, "unresolved"},
		{"ratio-control-unresolved", 20000000, 22000001, 21900000, 22000000, "unresolved"},
		{"faster-control-unresolved", 20000000, 17999999, 22100000, 22000000, "unresolved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp, err := summarizeTimings(checkpoint{Size: 1}, map[string]map[string][]int64{"base": {"op": {int64(tc.base)}}, "wrapper": {"op": {int64(tc.wrapper)}}, "delta": {"op": {int64(tc.delta)}}})
			if err != nil || cp.ReadLimitNS["op"] != tc.limit || cp.ReadVerdict["op"] != tc.status {
				t.Fatal(cp, err)
			}
			r := threshold(deltaReport{Checkpoints: []checkpoint{cp}})
			if tc.status == "unresolved" && (deltaTarget("arrow", r).Status != "unresolved" || r.Threshold != nil || r.LowerBound != nil) {
				t.Fatal("noise became a verdict", r)
			}
		})
	}
}
func TestTimingSummaryRejectsMissingOperationSamples(t *testing.T) {
	for _, timings := range []map[string]map[string][]int64{
		{},
		{"base": {"op": {1}}, "wrapper": {"op": {1}}},
		{"base": {"op": {1}}, "wrapper": {"op": {0}}, "delta": {"op": {1}}},
	} {
		if _, err := summarizeTimings(checkpoint{}, timings); err == nil {
			t.Fatal("incomplete timing evidence accepted", timings)
		}
	}
}
func TestTimingOrderPairsQueriesAndBalancesPositions(t *testing.T) {
	positions := map[string][3]int{}
	for i := 0; i < 6; i++ {
		for position, form := range timingOrder(i) {
			counts := positions[form]
			counts[position]++
			positions[form] = counts
		}
	}
	for form, counts := range positions {
		if counts != ([3]int{2, 2, 2}) {
			t.Fatal(form, counts)
		}
	}
	if len(positions) != 3 {
		t.Fatal(positions)
	}
}
func TestCheck4OnlyStartsNoCheck3WorkOrResult(t *testing.T) {
	c := fixtureConfig(t)
	c.Step = "check4"
	if err := os.Mkdir(c.Out, 0700); err != nil {
		t.Fatal(err)
	}
	var calls []string
	run := func(task string, limit int64) (externalResult, error) {
		calls = append(calls, task)
		if task == "delta-arrow" || task == "delta-parquet" {
			dir := filepath.Join(c.Out, task)
			if err := os.Mkdir(dir, 0700); err != nil {
				return externalResult{}, err
			}
			if err := dataset.WriteJSON(filepath.Join(dir, "metrics.json"), passingReport()); err != nil {
				return externalResult{}, err
			}
		}
		if task == "verify-arrow" || task == "verify-parquet" {
			if err := dataset.WriteJSON(filepath.Join(c.Out, task+".json"), verifyReport{Status: "pass", Checked: 1, NativeTopKComparisons: 1, NativeTopKEqual: 1}); err != nil {
				return externalResult{}, err
			}
		}
		return completedReceipt(limit), nil
	}
	targets, err := collectTargets(c, run)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls, []string{"prepare", "references", "delta-arrow", "verify-arrow", "delta-parquet", "verify-parquet"}) {
		t.Fatal(calls)
	}
	for _, target := range targets {
		if target.Target == "check3-link-peak-and-parity" {
			t.Fatal(target)
		}
	}
}

type noBaseVectorTopK struct{ baseResources }

func (noBaseVectorTopK) VectorTopK(context.Context, []float32, int) ([]adapter.Hit, error) {
	return nil, errors.New("base vector top-k ran before bitmap filtering")
}
func TestCheck4OnlyFailureSummaryContainsNoCheck3Result(t *testing.T) {
	c := fixtureConfig(t)
	c.Step = "check4"
	c.WallSeconds = 9000
	if err := os.Mkdir(c.Out, 0700); err != nil {
		t.Fatal(err)
	}
	if err := prepare(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := dataset.WriteJSON(filepath.Join(c.Out, "config.json"), c); err != nil {
		t.Fatal(err)
	}
	if err := writePolicies(c, time.Now().Add(-8941*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := control(c); err == nil || !strings.Contains(err.Error(), "worker deadline expired") {
		t.Fatal(err)
	}
	var summary runSummary
	if err := dataset.ReadJSON(filepath.Join(c.Out, "result.json"), &summary); err != nil {
		t.Fatal(err)
	}
	if len(summary.Targets) != 3 {
		t.Fatal(summary)
	}
	for _, target := range summary.Targets {
		if strings.HasPrefix(target.Target, "check3") {
			t.Fatal(target)
		}
	}
}

func TestLexicalParityRejectsInvalidCutoffsAndFullSetDifferences(t *testing.T) {
	for _, op := range []string{"symbols", "content"} {
		t.Run(op, func(t *testing.T) {
			matches := map[string]string{"a": "r", "b": "r", "c": "r", "d": "r"}
			cutoff := workload.Query{ID: "cutoff", Op: op, Repo: "r", K: 2, ForbiddenIDs: []string{"hidden"}}
			want := workload.Expected{QueryID: cutoff.ID, Answer: workload.Answer{Hits: []adapter.Hit{{ID: "a", Score: 2}, {ID: "b", Score: 1}}}}
			// Different valid picks at a cutoff remain diagnostic, not a failure.
			valid := workload.Answer{Hits: []adapter.Hit{{ID: "d", Score: 2}, {ID: "c", Score: 1}}}
			if err := compareReference(cutoff, want, matches, valid); err != nil {
				t.Fatal("valid full cutoff refused", err)
			}
			for _, tc := range []struct {
				name string
				hits []adapter.Hit
			}{
				{"hidden", []adapter.Hit{{ID: "hidden", Score: 2}, {ID: "b", Score: 1}}},
				{"duplicate", []adapter.Hit{{ID: "b", Score: 2}, {ID: "b", Score: 1}}},
				{"non-match", []adapter.Hit{{ID: "not-a-match", Score: 2}, {ID: "b", Score: 1}}},
				{"short", []adapter.Hit{{ID: "b", Score: 1}}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if err := compareReference(cutoff, want, matches, workload.Answer{Hits: tc.hits}); !errors.Is(err, errAnswerMismatch) {
						t.Fatal("invalid cutoff accepted", tc.name, err)
					}
				})
			}
			// Even if a hidden ID leaks into prepared matches, the query's explicit
			// removed-row set must reject it independently.
			leaked := map[string]string{"hidden": "r", "b": "r", "c": "r", "d": "r"}
			if err := compareReference(cutoff, want, leaked, workload.Answer{Hits: []adapter.Hit{{ID: "hidden", Score: 2}, {ID: "b", Score: 1}}}); !errors.Is(err, errAnswerMismatch) {
				t.Fatal("hidden row accepted", err)
			}
			fullWant := workload.Expected{Answer: workload.Answer{Hits: []adapter.Hit{{ID: "a", Score: 3}, {ID: "b", Score: 2}, {ID: "c", Score: 1}}}}
			for _, k := range []int{-1, 4, 8} {
				full := workload.Query{ID: "full", Op: op, Repo: "r", K: k}
				reversed := workload.Answer{Hits: []adapter.Hit{{ID: "c", Score: 3}, {ID: "b", Score: 2}, {ID: "a", Score: 1}}}
				if err := compareReference(full, fullWant, matches, reversed); err != nil {
					t.Fatal("full set wrongly required order", k, err)
				}
				for _, hits := range [][]adapter.Hit{
					{{ID: "d", Score: 3}, {ID: "b", Score: 2}, {ID: "c", Score: 1}},
					{{ID: "a", Score: 3}, {ID: "b", Score: 2}},
					{{ID: "a", Score: 4}, {ID: "b", Score: 3}, {ID: "c", Score: 2}, {ID: "d", Score: 1}},
				} {
					if err := compareReference(full, fullWant, matches, workload.Answer{Hits: hits}); !errors.Is(err, errAnswerMismatch) {
						t.Fatal("different full set accepted", k, hits, err)
					}
				}
			}
		})
	}
}

func TestResolvedReadMissSetsThresholdDespiteUnresolvedOperation(t *testing.T) {
	prior := passingReport().Checkpoints[0]
	crossing := prior
	crossing.Size = 4
	crossing.ReadVerdict = map[string]string{"pass": "pass", "miss": "miss", "noisy": "unresolved"}
	if got := checkpointReadStatus(crossing); got != "miss" {
		t.Errorf("resolved miss hidden by unresolved operation: %s", got)
	}
	r := threshold(deltaReport{Checkpoints: []checkpoint{prior, crossing}})
	if r.CompactionBranch != "observed-read-bound-crossing" || r.Threshold == nil || *r.Threshold != prior.Size || r.LowerBound != nil {
		t.Fatal("resolved crossing lost its preceding threshold", r)
	}
	if deltaTarget("arrow", r).Status != "pass" || crossing.ReadVerdict["noisy"] != "unresolved" {
		t.Fatal("preceding passing regime or per-operation uncertainty changed", r)
	}
}

func TestUnresolvedReadDoesNotHideIndependentLimitMiss(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*checkpoint)
		status string
	}{
		{"limits-pass", func(*checkpoint) {}, "unresolved"},
		{"edit-cpu-miss", func(p *checkpoint) { p.MaxEditCPU = 1000000001 }, "miss"},
		{"visibility-miss", func(p *checkpoint) { p.Visibility.P95 = 5000000001 }, "miss"},
		{"byte-ratio-miss", func(p *checkpoint) { p.ByteRatio = 2.01 }, "miss"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := passingReport().Checkpoints[0]
			p.ReadVerdict = map[string]string{"resolved": "pass", "noisy": "unresolved"}
			tc.change(&p)
			if got := checkpointReadStatus(p); got != "unresolved" {
				t.Fatal(got)
			}
			r := threshold(deltaReport{Checkpoints: []checkpoint{p}})
			if r.Threshold != nil || r.LowerBound != nil || deltaPass(r) {
				t.Fatal("unresolved reads became a passing regime", r)
			}
			if got := deltaTarget("arrow", r).Status; got != tc.status {
				t.Fatal("unresolved read hid an independent limit result", got, tc.status)
			}
			p.Size, p.Visibility.Count = 4, 4
			r = threshold(deltaReport{Checkpoints: []checkpoint{passingReport().Checkpoints[0], p}})
			if got := deltaTarget("arrow", r).Status; got != tc.status {
				t.Fatal("later unresolved checkpoint hid an independent limit result", got, tc.status)
			}
		})
	}
}

func TestTextRebuildCPUWindowExcludesViewConstruction(t *testing.T) {
	source, err := os.ReadFile("backend.go")
	if err != nil {
		t.Fatal(err)
	}
	_, build, ok := strings.Cut(string(source), "func (b backend) BuildDelta(")
	if !ok {
		t.Fatal("BuildDelta missing")
	}
	build, _, _ = strings.Cut(build, "func finishSeal(")
	// Pin the two measured windows without running a base build on this Mac.
	// Layer/mask construction and payload writes stay only in total edit CPU.
	last := -1
	for _, boundary := range []string{
		"indexLayer(rows)", "view.indexMasks()", "p0, err := cpuNS()",
		"var deltaText []row", "p1, err := cpuNS()", "writeDelta(",
		"c0, err := cpuNS()", "b.buildText(", "c1, err := cpuNS()",
		"b.cache.TextCPU[ref.Digest] = (p1 - p0) + (c1 - c0)",
	} {
		at := strings.Index(build, boundary)
		if at <= last {
			t.Fatalf("D text CPU boundary missing or out of order: %q", boundary)
		}
		last = at
	}
}

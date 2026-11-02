package main

import (
	"context"
	"encoding/json"
	"errors"
	"gxstudy/adapter"
	"gxstudy/conformance"
	"gxstudy/dataset"
	"gxstudy/oracle"
	"gxstudy/runner"
	"gxstudy/workload"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func checkAll(t *testing.T, s adapter.Segment, d dataset.Dataset) {
	t.Helper()
	ctx := context.Background()
	o := oracle.New(d)
	queries := workload.Generate([]dataset.Dataset{d}, 20261022)
	for _, q := range queries {
		q.ExactRank = true
		// Full native membership is exact; formula BM25 rank is not the C contract.
		if workload.Lexical(q.Op) {
			q.K = -1
		}
		want, err := workload.Evaluate(ctx, map[string]adapter.Segment{d.Repo: o}, nil, q)
		if err != nil {
			t.Fatal(err)
		}
		got, err := workload.Evaluate(ctx, map[string]adapter.Segment{d.Repo: s}, nil, q)
		if err != nil {
			t.Fatal(err)
		}
		c := workload.Compare(q, want, got)
		if c.Mismatch {
			t.Fatal(q, c)
		}
	}
}
func TestFullPayloadAndLifecycleEdits(t *testing.T) {
	for _, format := range []string{"arrow", "parquet"} {
		t.Run(format, func(t *testing.T) {
			b := newBackend(format)
			conformance.CheckBackend(t, b)
			conformance.CheckCatalog(t, b, func(dir string) (adapter.Catalog, error) {
				return oracle.NewCatalogWithVerifier(dir, func(r adapter.SegmentRef) error {
					s, e := b.Open(context.Background(), r.Path, r)
					if e != nil {
						return e
					}
					return s.Close()
				})
			})
			ctx := context.Background()
			d := conformance.Fixture()
			d.Edges = append(d.Edges, d.Edges[0])
			changed, err := runner.MakeChange(d)
			if err != nil {
				t.Fatal(err)
			}
			noOp := adapter.FileChange{Path: "f.go", Replacement: dataset.Dataset{Repo: d.Repo}}
			for _, n := range d.Nodes {
				if n.FilePath == "f.go" {
					noOp.Replacement.Nodes = append(noOp.Replacement.Nodes, n)
				}
			}
			for _, e := range d.Edges {
				if e.FilePath == "f.go" {
					noOp.Replacement.Edges = append(noOp.Replacement.Edges, e)
				}
			}
			for _, f := range d.Files {
				if f.Path == "f.go" {
					noOp.Replacement.Files = append(noOp.Replacement.Files, f)
				}
			}
			noOp.Replacement.Symbols = d.Symbols
			noOp.Replacement.Content = d.Content
			noOp.Replacement.Vectors = d.Vectors
			body := noOp
			body.Replacement = noOp.Replacement
			body.Replacement.Content = slices.Clone(noOp.Replacement.Content)
			body.Replacement.Content[0].Body += " body-only"
			renamed := body
			renamed.Path = "renamed/nested.go"
			renamed.Replacement = dataset.Dataset{Repo: d.Repo, Nodes: []dataset.Node{{ID: "renamed", Name: "Alpha", QualName: "nested.Alpha", FilePath: renamed.Path, Language: "go", StartLine: 1, EndLine: 2}}, Files: []dataset.File{{Path: renamed.Path, ContentHash: "rename"}}}
			cases := []struct {
				name string
				c    adapter.FileChange
			}{{"no-op", noOp}, {"body-only", body}, {"mutation", changed}, {"delete", adapter.FileChange{Path: "f.go", Replacement: dataset.Dataset{Repo: d.Repo}}}, {"rename-add", renamed}, {"remove-renamed", adapter.FileChange{Path: renamed.Path, Replacement: dataset.Dataset{Repo: d.Repo}}}, {"re-add", noOp}}
			root := t.TempDir()
			ref, _, err := b.Build(ctx, d, filepath.Join(root, "base"))
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					next, stats, e := b.BuildDelta(ctx, ref, tc.c, filepath.Join(root, tc.name))
					if e != nil {
						t.Fatal(e)
					}
					if stats.Mode != "delta" {
						t.Fatal(stats)
					}
					d = adapter.ApplyChange(d, tc.c)
					s, e := b.Open(ctx, next.Path, next)
					if e != nil {
						t.Fatal(e)
					}
					checkAll(t, s, d)
					if e = s.Close(); e != nil {
						t.Fatal(e)
					}
					ref = next
				})
			}
			// A full rebuild after deltas must match, including retained dangling incoming.
			clean, _, err := b.Build(ctx, d, filepath.Join(root, "rebuilt"))
			if err != nil {
				t.Fatal(err)
			}
			s, err := b.Open(ctx, clean.Path, clean)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			checkAll(t, s, d)
		})
	}
}
func TestLinkLookupParityWitnessesAndReorder(t *testing.T) {
	ctx := context.Background()
	d := conformance.Fixture()
	d.Nodes[0].QualName = "p.Alpha"
	d.Nodes[1].QualName = "p.Beta"
	d.Nodes[2].QualName = "nested.Outside"
	alias := d.Nodes[0]
	alias.ID = "alias"
	d.Nodes = append(d.Nodes, alias)
	d.Edges = append(d.Edges, d.Edges[0], dataset.Edge{From: "outside", To: "alias", Kind: "calls", FilePath: "g.go"}, dataset.Edge{From: "outside", To: "unresolved:external.Name", Kind: "references", FilePath: "g.go", Line: 9, Confidence: .33, Payload: map[string]json.RawMessage{"keep": json.RawMessage(`true`)}})
	root := t.TempDir()
	facts := filepath.Join(root, "facts")
	if e := prepareLink(ctx, []dataset.Dataset{d}, facts); e != nil {
		t.Fatal(e)
	}
	var stats collisionStats
	if e := dataset.ReadJSON(filepath.Join(facts, "collisions.json"), &stats); e != nil || stats.NameScopeGroups != 1 || stats.ExtendedGroups != 1 || stats.NameScopeFacts != 3 || stats.ExtendedFacts != 3 {
		t.Fatal(stats, e)
	}
	if e := eachJSON(filepath.Join(facts, "facts.jsonl"), func(f fact) error {
		if !f.Unresolved && f.Edge.To != "" {
			t.Fatal("declared target ID leaked")
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	for _, mode := range []string{"global", "package"} {
		out := filepath.Join(root, mode)
		r, e := resolveFacts(ctx, facts, out, mode)
		if e != nil || r.Status != "pass" || r.Facts != 6 || r.Resolved != 5 || r.UnresolvedKept != 1 || r.Missing+r.Ambiguous != 0 {
			t.Fatal(r, e)
		}
		if e = compareEdgeFiles(filepath.Join(facts, "expected.jsonl"), filepath.Join(out, "edges.jsonl"), filepath.Join(root, "compare-"+mode)); e != nil {
			t.Fatal(e)
		}
	}
	slices.Reverse(d.Nodes)
	slices.Reverse(d.Edges)
	reordered := filepath.Join(root, "reordered")
	if e := prepareLink(ctx, []dataset.Dataset{d}, reordered); e != nil {
		t.Fatal(e)
	}
	out := filepath.Join(root, "reordered-output")
	if r, e := resolveFacts(ctx, reordered, out, "global"); e != nil || r.Status != "pass" {
		t.Fatal(r, e)
	}
	if e := compareEdgeFiles(filepath.Join(facts, "expected.jsonl"), filepath.Join(out, "edges.jsonl"), filepath.Join(root, "reordered-compare")); e != nil {
		t.Fatal(e)
	}
	// Invalid supplied projections still emit witnesses, never guesses. The
	// valid dangling fact above remains unchanged even in this partial result.
	var declarations []declaration
	var fs []fact
	if e := eachJSON(filepath.Join(facts, "declarations.jsonl"), func(v declaration) error { declarations = append(declarations, v); return nil }); e != nil {
		t.Fatal(e)
	}
	if e := eachJSON(filepath.Join(facts, "facts.jsonl"), func(v fact) error { fs = append(fs, v); return nil }); e != nil {
		t.Fatal(e)
	}
	dupe := declarations[0]
	dupe.ID = "deliberate-key-collision"
	declarations = append(declarations, dupe)
	fs[1].Target.Qualified = "deliberate-missing-key"
	if e := writeJSONLines(filepath.Join(facts, "declarations.jsonl"), func(enc *json.Encoder) error {
		for _, v := range declarations {
			if e := enc.Encode(v); e != nil {
				return e
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	if e := writeJSONLines(filepath.Join(facts, "facts.jsonl"), func(enc *json.Encoder) error {
		for _, v := range fs {
			if e := enc.Encode(v); e != nil {
				return e
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	result, e := resolveFacts(ctx, facts, filepath.Join(root, "partial"), "global")
	if e != nil || result.Status != "partial" || result.Missing != 1 || result.Ambiguous != 2 || result.UnresolvedKept != 1 || result.Facts != result.Resolved+result.Missing+result.Ambiguous+result.UnresolvedKept {
		t.Fatal(result, e)
	}
	// A clean preparation with the formerly dangling declaration resolves it.
	d.Nodes = append(d.Nodes, dataset.Node{ID: "unresolved:external.Name", Name: "External", FilePath: "deps/nested.go"})
	ready := filepath.Join(root, "new-definition")
	if e = prepareLink(ctx, []dataset.Dataset{d}, ready); e != nil {
		t.Fatal(e)
	}
	result, e = resolveFacts(ctx, ready, filepath.Join(root, "resolved"), "global")
	if e != nil || result.Status != "pass" || result.UnresolvedKept != 0 {
		t.Fatal(result, e)
	}
}
func TestPinsCASRetireRecoveryAndCorruptRefusal(t *testing.T) {
	for _, format := range []string{"arrow", "parquet"} {
		t.Run(format, func(t *testing.T) {
			ctx := context.Background()
			b := newBackend(format)
			root := t.TempDir()
			l, err := newLifecycle(root, b)
			if err != nil {
				t.Fatal(err)
			}
			defer l.close()
			d := conformance.Fixture()
			old, _, err := b.Build(ctx, d, filepath.Join(root, "old"))
			if err != nil {
				t.Fatal(err)
			}
			if err = l.publish(ctx, old, nil); err != nil {
				t.Fatal(err)
			}
			s, release, err := l.pin(ctx, d.Repo)
			if err != nil {
				t.Fatal(err)
			}
			c, err := runner.MakeChange(d)
			if err != nil {
				t.Fatal(err)
			}
			next, _, err := b.BuildDelta(ctx, old, c, filepath.Join(root, "next"))
			if err != nil {
				t.Fatal(err)
			}
			if err = l.publish(ctx, next, &old); err != nil {
				t.Fatal(err)
			}
			if err = l.publish(ctx, old, &old); !errors.Is(err, adapter.ErrConflict) {
				t.Fatal("stale CAS", err)
			}
			if err = l.retire(ctx, old); err == nil {
				t.Fatal("retired live ancestor")
			}
			checkAll(t, s, d)
			if err = release(); err != nil {
				t.Fatal(err)
			}
			if err = release(); err != nil {
				t.Fatal(err)
			}
			// Recovery does not adopt a sealed orphan or an unsealed interrupted build.
			orphan, _, err := b.Build(ctx, d, filepath.Join(root, "orphan"))
			if err != nil {
				t.Fatal(err)
			}
			if err = os.Mkdir(filepath.Join(root, "interrupted"), 0700); err != nil {
				t.Fatal(err)
			}
			if err = l.cat.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			v, unpin, err := l.cat.Pin(ctx, []string{d.Repo})
			if err != nil {
				t.Fatal(err)
			}
			unpin()
			if v[d.Repo] != next {
				t.Fatal("orphan adopted")
			}
			if err = l.retire(ctx, orphan); err != nil {
				t.Fatal(err)
			}
			if err = l.publish(ctx, orphan, &next); err == nil {
				t.Fatal("retirement fence ignored")
			}
			if err = os.WriteFile(filepath.Join(next.Path, "delta."+format), []byte("corrupt"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = b.Open(ctx, next.Path, next); err == nil {
				t.Fatal("corrupt payload accepted")
			}
			alias := filepath.Join(root, "alias")
			if err = os.Symlink(old.Path, alias); err != nil {
				t.Fatal(err)
			}
			aliased := old
			aliased.Path = alias
			if _, err = b.Open(ctx, alias, aliased); err == nil {
				t.Fatal("symlink accepted")
			}
		})
	}
}
func TestDeltaRejectsWrongOwnerAndMissingTombstone(t *testing.T) {
	ctx := context.Background()
	d := conformance.Fixture()
	b := newBackend("arrow")
	base, _, err := b.Build(ctx, d, filepath.Join(t.TempDir(), "base"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := b.Open(ctx, base.Path, base)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := adapter.FileChange{Path: "f.go", Replacement: dataset.Dataset{Repo: d.Repo, Nodes: []dataset.Node{{ID: "evil", FilePath: "g.go"}}}}
	if _, err = changeRows(ctx, s.(*segment), c); err == nil {
		t.Fatal("wrong owner accepted")
	}
	c, err = runner.MakeChange(d)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := changeRows(ctx, s.(*segment), c)
	if err != nil {
		t.Fatal(err)
	}
	broken := slices.DeleteFunc(slices.Clone(rows), func(r row) bool { return r.Family == "mask_file" || r.Family == "mask_node" })
	dir := filepath.Join(t.TempDir(), "broken")
	if err = os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = writeDelta(filepath.Join(dir, "delta.arrow"), "arrow", broken); err != nil {
		t.Fatal(err)
	}
	if err = buildDeltaText(ctx, dir, broken); err != nil {
		t.Fatal(err)
	}
	ref, _, err := finishSeal(dir, seal{Schema: "linkdelta-seal/v1", Repo: d.Repo, Format: "arrow", Parent: &base, Payload: "delta.arrow"}, "delta")
	if err != nil {
		t.Fatal(err)
	}
	bad, err := b.Open(ctx, ref.Path, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()
	q := workload.Query{Op: "file", Repo: d.Repo, Key: "f.go"}
	want, _ := workload.Evaluate(ctx, map[string]adapter.Segment{d.Repo: oracle.New(adapter.ApplyChange(d, c))}, nil, q)
	got, _ := workload.Evaluate(ctx, map[string]adapter.Segment{d.Repo: bad}, nil, q)
	if !workload.Compare(q, want, got).Mismatch {
		t.Fatal("missing tombstone accepted")
	}
}

func TestInterruptedPublishAndRestart(t *testing.T) {
	for _, stage := range []string{"after-write", "after-sync", "before-rename", "after-rename", "after-dirsync"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			b := newBackend("arrow")
			root := t.TempDir()
			l, err := newLifecycle(root, b)
			if err != nil {
				t.Fatal(err)
			}
			d := conformance.Fixture()
			old, _, err := b.Build(ctx, d, filepath.Join(root, "old"))
			if err != nil {
				t.Fatal(err)
			}
			if err = l.publish(ctx, old, nil); err != nil {
				t.Fatal(err)
			}
			c, err := runner.MakeChange(d)
			if err != nil {
				t.Fatal(err)
			}
			next, _, err := b.BuildDelta(ctx, old, c, filepath.Join(root, "next"))
			if err != nil {
				t.Fatal(err)
			}
			if err = dataset.WriteJSON(filepath.Join(root, "crash-next.json"), next); err != nil {
				t.Fatal(err)
			}
			exe, e := os.Executable()
			if e != nil {
				t.Fatal(e)
			}
			childCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(childCtx, exe, "-test.run=^TestPublishCrashHelper$")
			cmd.Env = append(os.Environ(), "LINKDELTA_CRASH_ROOT="+root, "LINKDELTA_CRASH_STAGE="+stage)
			output, e := cmd.CombinedOutput()
			exit, ok := e.(*exec.ExitError)
			if !ok || exit.ExitCode() != 99 {
				t.Fatalf("checkpoint child did not stop at boundary: %v %s", e, output)
			}
			if err = l.close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := newLifecycle(root, b)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.close()
			if err = reopened.cat.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			view, release, err := reopened.cat.Pin(ctx, []string{d.Repo})
			if err != nil {
				t.Fatal(err)
			}
			release()
			want := old
			if stage == "after-rename" || stage == "after-dirsync" {
				want = next
			}
			if view[d.Repo] != want {
				t.Fatal("interrupted publish exposed wrong route")
			}
		})
	}
	d := conformance.Fixture()
	d.Nodes = append(d.Nodes, d.Nodes[0])
	out := filepath.Join(t.TempDir(), "must-not-seal")
	if _, _, err := (newBackend("arrow")).Build(context.Background(), d, out); err == nil {
		t.Fatal("duplicate node ID accepted")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("invalid build created output", err)
	}
}

func TestPinPreventsUnreferencedReclaim(t *testing.T) {
	ctx := context.Background()
	b := newBackend("arrow")
	root := t.TempDir()
	l, err := newLifecycle(root, b)
	if err != nil {
		t.Fatal(err)
	}
	defer l.close()
	d := conformance.Fixture()
	old, _, err := b.Build(ctx, d, filepath.Join(root, "old"))
	if err != nil {
		t.Fatal(err)
	}
	if err = l.publish(ctx, old, nil); err != nil {
		t.Fatal(err)
	}
	s, release, err := l.pin(ctx, d.Repo)
	if err != nil {
		t.Fatal(err)
	}
	prior := old
	for _, name := range []string{"next", "current"} {
		next, _, e := b.Build(ctx, d, filepath.Join(root, name))
		if e != nil {
			t.Fatal(e)
		}
		if e = l.publish(ctx, next, &prior); e != nil {
			t.Fatal(e)
		}
		prior = next
	}
	if err = l.retire(ctx, old); err == nil {
		t.Fatal("unpinned roots check hid missing pin protection")
	}
	checkAll(t, s, d)
	if err = release(); err != nil {
		t.Fatal(err)
	}
	if err = l.retire(ctx, old); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Open(ctx, old.Path, old); err == nil {
		t.Fatal("retired version reopened")
	}
}

func TestPublishCrashHelper(t *testing.T) {
	root := os.Getenv("LINKDELTA_CRASH_ROOT")
	if root == "" {
		return
	}
	stage := os.Getenv("LINKDELTA_CRASH_STAGE")
	b := newBackend("arrow")
	l, err := newLifecycle(root, b)
	if err != nil {
		t.Fatal(err)
	}
	defer l.close()
	var next adapter.SegmentRef
	if err = dataset.ReadJSON(filepath.Join(root, "crash-next.json"), &next); err != nil {
		t.Fatal(err)
	}
	v, release, err := l.cat.Pin(context.Background(), []string{next.Repo})
	if err != nil {
		t.Fatal(err)
	}
	release()
	old := v[next.Repo]
	adapter.Checkpoint = func(s string) {
		if s == stage {
			os.Exit(99)
		}
	}
	if err = l.publish(context.Background(), next, &old); err != nil {
		t.Fatal(err)
	}
	t.Fatal("checkpoint was unreachable")
}

// Given a fresh owner, adoption hashes complete bytes once. When the same
// immutable ref is opened/published again, seal/size checks remain reachable.
func TestAdoptionHashesOnceAndRejectsFreshCorruption(t *testing.T) {
	ctx := context.Background()
	b := newBackend("arrow")
	root := t.TempDir()
	ref, _, e := b.Build(ctx, conformance.Fixture(), filepath.Join(root, "base"))
	if e != nil {
		t.Fatal(e)
	}
	adopter := newBackend("arrow")
	for i := 0; i < 3; i++ {
		s, e := adopter.Open(ctx, ref.Path, ref)
		if e != nil {
			t.Fatal(e)
		}
		if e = s.Close(); e != nil {
			t.Fatal(e)
		}
		if _, e = ancestors(adopter, ref); e != nil {
			t.Fatal(e)
		}
	}
	if adopter.cache.HashPasses != 1 || len(adopter.cache.owners) != 1 {
		t.Fatal("repeated whole-base adoption", adopter.cache.HashPasses)
	}
	payload := filepath.Join(ref.Path, "base-ref.json")
	raw, e := os.ReadFile(payload)
	if e != nil {
		t.Fatal(e)
	}
	raw[0] ^= 1
	if e = os.WriteFile(payload, raw, 0600); e != nil {
		t.Fatal(e)
	}
	fresh := newBackend("arrow")
	if _, e = fresh.Open(ctx, ref.Path, ref); e == nil {
		t.Fatal("same-size corruption passed fresh adoption")
	}
}
func TestVectorMergedTopKWithMaskedWinnersAndTies(t *testing.T) {
	for _, format := range []string{"arrow", "parquet"} {
		t.Run(format, func(t *testing.T) {
			ctx := context.Background()
			d := conformance.Fixture()
			v := make([]float32, dataset.Dimensions)
			v[0] = 1
			d.Vectors = []dataset.Vector{{NodeID: "a", Values: v}, {NodeID: "b", Values: v}, {NodeID: "outside", Values: v}}
			c := adapter.FileChange{Path: "f.go", Replacement: dataset.Dataset{Repo: d.Repo, Nodes: []dataset.Node{d.Nodes[1]}, Files: []dataset.File{d.Files[0]}, Vectors: []dataset.Vector{{NodeID: "b", Values: v}}}}
			b := newBackend(format)
			root := t.TempDir()
			old, _, e := b.Build(ctx, d, filepath.Join(root, "base"))
			if e != nil {
				t.Fatal(e)
			}
			next, _, e := b.BuildDelta(ctx, old, c, filepath.Join(root, "delta"))
			if e != nil {
				t.Fatal(e)
			}
			s, e := b.Open(ctx, next.Path, next)
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			oracle := oracle.New(adapter.ApplyChange(d, c))
			for _, k := range []int{0, 1, 2, -1} {
				want, e := oracle.VectorTopK(ctx, v, k)
				if e != nil {
					t.Fatal(e)
				}
				got, e := s.VectorTopK(ctx, v, k)
				if e != nil || !slices.Equal(want, got) {
					t.Fatal(k, want, got, e)
				}
			}
			if _, e = s.VectorTopK(ctx, []float32{1}, 1); e == nil {
				t.Fatal("invalid dimensions accepted")
			}
			if _, e = s.VectorTopK(ctx, make([]float32, dataset.Dimensions), 1); e == nil {
				t.Fatal("zero vector accepted")
			}
		})
	}
}

// Each family must independently exclude superseded native documents. Removing
// either mask below must fail this counterfactual without relying on graph reads.
func TestNativeFamilyMaskCounterfactuals(t *testing.T) {
	for _, format := range []string{"arrow", "parquet"} {
		for _, family := range []string{"mask_node", "mask_chunk"} {
			t.Run(format+"/"+family, func(t *testing.T) {
				ctx := context.Background()
				d := conformance.Fixture()
				b := newBackend(format)
				root := t.TempDir()
				base, _, e := b.Build(ctx, d, filepath.Join(root, "base"))
				if e != nil {
					t.Fatal(e)
				}
				deleted := adapter.FileChange{Path: "f.go", Replacement: dataset.Dataset{Repo: d.Repo}}
				next, _, e := b.BuildDelta(ctx, base, deleted, filepath.Join(root, "valid"))
				if e != nil {
					t.Fatal(e)
				}
				rows, e := readDelta(filepath.Join(next.Path, "delta."+format), format)
				if e != nil {
					t.Fatal(e)
				}
				rows = slices.DeleteFunc(rows, func(r row) bool { return r.Family == family })
				dir := filepath.Join(root, "broken")
				if e = os.Mkdir(dir, 0700); e != nil {
					t.Fatal(e)
				}
				if e = writeDelta(filepath.Join(dir, "delta."+format), format, rows); e != nil {
					t.Fatal(e)
				}
				if e = buildDeltaText(ctx, dir, rows); e != nil {
					t.Fatal(e)
				}
				ref, _, e := finishSeal(dir, seal{Schema: "linkdelta-seal/v1", Repo: d.Repo, Format: format, Parent: &base, Payload: "delta." + format}, "delta")
				if e != nil {
					t.Fatal(e)
				}
				s, e := b.Open(ctx, ref.Path, ref)
				if e != nil {
					t.Fatal(e)
				}
				defer s.Close()
				var hits []adapter.Hit
				if family == "mask_node" {
					hits, e = s.SearchSymbols(ctx, "graph", -1)
				} else {
					hits, e = s.SearchContent(ctx, "graph", -1)
				}
				if e != nil || len(hits) == 0 {
					t.Fatal("counterfactual did not expose unmasked old documents", hits, e)
				}
			})
		}
	}
}

// The measured overlay may call indexed C operations, but never scan or encode
// whole C row families to reconstruct text masks or vector answers.
type noRowScans struct{ baseResources }

func forbiddenRows[T any]() iter.Seq2[T, error] {
	return func(yield func(T, error) bool) { var zero T; yield(zero, errors.New("unexpected full C row scan")) }
}
func (noRowScans) NodeRows(context.Context) iter.Seq2[dataset.Node, error] {
	return forbiddenRows[dataset.Node]()
}
func (noRowScans) EdgeRows(context.Context) iter.Seq2[dataset.Edge, error] {
	return forbiddenRows[dataset.Edge]()
}
func (noRowScans) FileRows(context.Context) iter.Seq2[dataset.File, error] {
	return forbiddenRows[dataset.File]()
}
func (noRowScans) SymbolRows(context.Context) iter.Seq2[dataset.Symbol, error] {
	return forbiddenRows[dataset.Symbol]()
}
func (noRowScans) ContentRows(context.Context) iter.Seq2[dataset.Content, error] {
	return forbiddenRows[dataset.Content]()
}
func (noRowScans) VectorRows(context.Context) iter.Seq2[dataset.Vector, error] {
	return forbiddenRows[dataset.Vector]()
}
func TestOverlayUsesIndexedCWithoutWholeFamilyScans(t *testing.T) {
	for _, format := range []string{"arrow", "parquet"} {
		t.Run(format, func(t *testing.T) {
			ctx := context.Background()
			d := conformance.Fixture()
			c, e := runner.MakeChange(d)
			if e != nil {
				t.Fatal(e)
			}
			b := newBackend(format)
			root := t.TempDir()
			ref, _, e := b.Build(ctx, d, filepath.Join(root, "base"))
			if e != nil {
				t.Fatal(e)
			}
			next, _, e := b.BuildDelta(ctx, ref, c, filepath.Join(root, "delta"))
			if e != nil {
				t.Fatal(e)
			}
			opened, e := b.Open(ctx, next.Path, next)
			if e != nil {
				t.Fatal(e)
			}
			defer opened.Close()
			s := opened.(*segment)
			s.external = noRowScans{s.external.(baseResources)}
			if e = s.configureText(ctx); e != nil {
				t.Fatal(e)
			}
			if _, e = changeRows(ctx, s, c); e != nil {
				t.Fatal(e)
			}
			checkAll(t, s, adapter.ApplyChange(d, c))
		})
	}
}

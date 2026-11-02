package main

import (
	"context"
	"errors"
	"gxstudy/adapter"
	"gxstudy/conformance"
	"gxstudy/dataset"
	"gxstudy/oracle"
	"gxstudy/runner"
	"gxstudy/workload"
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
			b := backend{format: format}
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
			}{{"no-op", noOp}, {"body-only", body}, {"mutation", changed}, {"delete", {Path: "f.go", Replacement: dataset.Dataset{Repo: d.Repo}}}, {"rename-add", renamed}, {"remove-renamed", {Path: renamed.Path, Replacement: dataset.Dataset{Repo: d.Repo}}}, {"re-add", noOp}}
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
	d.Edges = append(d.Edges, d.Edges[0])
	root := t.TempDir()
	facts := filepath.Join(root, "facts")
	if err := prepareLink(ctx, []dataset.Dataset{d}, facts); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"global", "package"} {
		out := filepath.Join(root, mode)
		r, err := resolveFacts(ctx, facts, out, mode)
		if err != nil || r.Status != "pass" || r.Resolved != 4 {
			t.Fatal(r, err)
		}
		if err = compareEdgeFiles(filepath.Join(facts, "expected.jsonl"), filepath.Join(out, "edges.jsonl"), filepath.Join(root, "compare-"+mode)); err != nil {
			t.Fatal(err)
		}
		var got []dataset.Edge
		if err = eachJSON(filepath.Join(out, "edges.jsonl"), func(e dataset.Edge) error { got = append(got, e); return nil }); err != nil {
			t.Fatal(err)
		}
		q := workload.Query{Op: "in"}
		if workload.Compare(q, workload.Answer{Edges: d.Edges}, workload.Answer{Edges: got}).Mismatch {
			t.Fatal("link parity")
		}
		if !workload.Compare(q, workload.Answer{Edges: d.Edges}, workload.Answer{Edges: got[1:]}).Mismatch {
			t.Fatal("dropped duplicate accepted")
		}
	}
	slices.Reverse(d.Nodes)
	slices.Reverse(d.Edges)
	reordered := filepath.Join(root, "reordered")
	if err := prepareLink(ctx, []dataset.Dataset{d}, reordered); err != nil {
		t.Fatal(err)
	}
	reorderedOut := filepath.Join(root, "reordered-output")
	if r, e := resolveFacts(ctx, reordered, reorderedOut, "global"); e != nil || r.Status != "pass" {
		t.Fatal(r, e)
	}
	if e := compareEdgeFiles(filepath.Join(facts, "expected.jsonl"), filepath.Join(reorderedOut, "edges.jsonl"), filepath.Join(root, "reordered-compare")); e != nil {
		t.Fatal(e)
	}
	d.Nodes = append(d.Nodes, dataset.Node{ID: "alias", QualName: "p.Alpha", Name: "Alias", FilePath: "f.go"})
	d.Edges = append(d.Edges, dataset.Edge{From: "b", To: "missing", Kind: "calls", FilePath: "nested/g.go"})
	facts2 := filepath.Join(root, "ambiguous")
	if err := prepareLink(ctx, []dataset.Dataset{d}, facts2); err != nil {
		t.Fatal(err)
	}
	r, err := resolveFacts(ctx, facts2, filepath.Join(root, "witnesses"), "global")
	if err != nil || r.Status != "partial" || r.Ambiguous != 2 || r.Missing != 1 {
		t.Fatal(r, err)
	}
	// A new definition satisfies the prior missing lookup on a clean preparation;
	// changing its ID exercises dependency replacement, not reuse of stale IDs.
	d.Nodes = append(d.Nodes, dataset.Node{ID: "missing", Name: "New", QualName: "dependency.New", FilePath: "deps/nested.go"})
	d.Nodes = slices.DeleteFunc(d.Nodes, func(n dataset.Node) bool { return n.ID == "alias" })
	facts3 := filepath.Join(root, "new-definition")
	if err = prepareLink(ctx, []dataset.Dataset{d}, facts3); err != nil {
		t.Fatal(err)
	}
	r, err = resolveFacts(ctx, facts3, filepath.Join(root, "resolved"), "global")
	if err != nil || r.Status != "pass" {
		t.Fatal(r, err)
	}
}
func TestPinsCASRetireRecoveryAndCorruptRefusal(t *testing.T) {
	for _, format := range []string{"arrow", "parquet"} {
		t.Run(format, func(t *testing.T) {
			ctx := context.Background()
			b := backend{format: format}
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
	b := backend{format: "arrow"}
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
			b := backend{format: "arrow"}
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
	if _, _, err := (backend{format: "arrow"}).Build(context.Background(), d, out); err == nil {
		t.Fatal("duplicate node ID accepted")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("invalid build created output", err)
	}
}

func TestPinPreventsUnreferencedReclaim(t *testing.T) {
	ctx := context.Background()
	b := backend{format: "arrow"}
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
	b := backend{format: "arrow"}
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

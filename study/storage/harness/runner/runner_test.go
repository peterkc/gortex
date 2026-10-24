package runner

import (
	"context"
	"errors"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/conformance"
	"gxstudy/dataset"
	"gxstudy/oracle"
	"gxstudy/workload"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func TestCrashCheckerRejectsMixedSet(t *testing.T) {
	a := adapter.SegmentRef{Repo: "a", Version: "old", Digest: "sealed"}
	b := adapter.SegmentRef{Repo: "b", Version: "old", Digest: "sealed"}
	na := a
	na.Version = "new"
	nb := b
	nb.Version = "new"
	old := adapter.View{"a": a, "b": b}
	next := adapter.View{"a": na, "b": nb}
	validate := func(adapter.SegmentRef) error { return nil }
	if e := CheckView(adapter.View{"a": na, "b": b}, old, next, validate); e == nil {
		t.Fatal("mixed linked set accepted")
	}
	if e := CheckView(old, old, next, validate); e != nil {
		t.Fatal(e)
	}
	if e := CheckView(next, old, next, validate); e != nil {
		t.Fatal(e)
	}
	if e := CheckView(next, old, next, func(adapter.SegmentRef) error { return fmt.Errorf("incomplete") }); e == nil {
		t.Fatal("incomplete sealed pointer accepted")
	}
}
func TestSummary(t *testing.T) {
	r := Summarize([]int64{5, 1, 3, 2, 4})
	if r.P50 != 3 || r.P95 != 5 || r.P99 != 5 || r.Mean != 3 || r.Count != 5 {
		t.Fatal(r)
	}
}

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "__catalog_publish" {
		factory := adapter.CatalogFactory(oracle.NewCatalog)
		if os.Getenv("GXSTUDY_BROKEN_CATALOG") == "1" {
			factory = newBrokenCatalog
		}
		if err := publishChild(factory, os.Args[2:]); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}
func TestSIGKILLCheckpointProtocol(t *testing.T) {
	ctx := context.Background()
	b := oracle.Backend{}
	root := t.TempDir()
	d := dataset.Dataset{Repo: "r", Nodes: []dataset.Node{{ID: "node", Kind: "function", Name: "old", FilePath: "f.go"}}}
	old, _, err := b.Build(ctx, d, filepath.Join(root, "old"))
	if err != nil {
		t.Fatal(err)
	}
	d.Nodes = []dataset.Node{{ID: "node", Kind: "function", Name: "new", FilePath: "f.go"}}
	next, _, err := b.Build(ctx, d, filepath.Join(root, "new"))
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"before-write", "after-write", "after-sync", "before-rename", "after-rename", "after-dirsync"} {
		dir := filepath.Join(root, stage)
		cat, err := oracle.NewCatalog(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err = cat.Publish(ctx, "r", old, nil); err != nil {
			t.Fatal(err)
		}
		cat.Close()
		killed, err := killPublish(ctx, publishRequest{Dir: dir, Set: adapter.View{"r": next}, Old: adapter.View{"r": old}, Single: true}, filepath.Join(root, stage+".json"), stage)
		if err != nil || !killed {
			t.Fatalf("%s: killed=%v err=%v", stage, killed, err)
		}
	}
}

func TestDeltaGraphMutationHandComputed(t *testing.T) {
	base := conformance.Fixture()
	c, err := MakeChange(base)
	if err != nil {
		t.Fatal(err)
	}
	changed := adapter.ApplyChange(base, c)
	s := oracle.New(changed)
	ctx := context.Background()
	if _, err = s.NodeByID(ctx, "b"); !errors.Is(err, adapter.ErrNotFound) {
		t.Fatal("removed b remains", err)
	}
	a, err := s.NodeByID(ctx, "a")
	if err != nil || a.Name != "Alpha__renamed" {
		t.Fatal(a, err)
	}
	added, err := s.NodeByID(ctx, "b__added")
	if err != nil || added.Name != "studyAddedFunction" {
		t.Fatal(added, err)
	}
	es, err := s.OutEdges(ctx, "a", "any")
	if err != nil || len(es) != 1 || es[0].To != "b__added" {
		t.Fatal("edge not changed", es, err)
	}
	es, err = s.OutEdges(ctx, "b", "any")
	if err != nil || len(es) != 0 {
		t.Fatal("removed outgoing edges remain", es, err)
	}
	es, err = s.InEdges(ctx, "b", "calls")
	if err != nil || len(es) != 1 || es[0].From != "outside" {
		t.Fatal("external incoming edge must survive dangling", es, err)
	}
	h, err := s.SearchSymbols(ctx, "deltaprobe", 10)
	if err != nil || len(h) != 2 {
		t.Fatal(h, err)
	}
	if len(changed.Vectors) != 2 {
		t.Fatal("remove/add vector count", len(changed.Vectors))
	}
	for _, v := range changed.Vectors {
		if v.NodeID == "b" {
			t.Fatal("deleted vector retained")
		}
	}
}
func TestCommitSideIsStrict(t *testing.T) {
	a := adapter.SegmentRef{Repo: "r", Version: "old", Digest: "x"}
	b := a
	b.Version = "new"
	old := adapter.View{"r": a}
	next := adapter.View{"r": b}
	ok := func(adapter.SegmentRef) error { return nil }
	if CheckViewAllowed(old, old, next, "new", ok) == nil {
		t.Fatal("postcommit rollback accepted")
	}
	if CheckViewAllowed(next, old, next, "old", ok) == nil {
		t.Fatal("precommit update accepted")
	}
}
func TestOracleConformance(t *testing.T) { conformance.CheckBackend(t, oracle.Backend{}) }

type brokenCatalog struct{ dir string }

func newBrokenCatalog(dir string) (adapter.Catalog, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	return &brokenCatalog{dir}, nil
}
func (c *brokenCatalog) CommitPoint() string { return "incorrect per-repo writes" }
func (c *brokenCatalog) load() adapter.View {
	v := adapter.View{}
	_ = dataset.ReadJSON(filepath.Join(c.dir, "view.json"), &v)
	return v
}
func (c *brokenCatalog) Publish(ctx context.Context, repo string, ref adapter.SegmentRef, old *adapter.SegmentRef) error {
	return c.PublishSet(ctx, adapter.View{repo: ref}, nil)
}
func (c *brokenCatalog) PublishSet(ctx context.Context, set, old adapter.View) error {
	v := c.load()
	var repos []string
	for repo := range set {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	for _, repo := range repos {
		v[repo] = set[repo]
		if err := dataset.WriteJSON(filepath.Join(c.dir, "view.json"), v); err != nil {
			return err
		}
		if len(old) > 0 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	return nil
}
func (c *brokenCatalog) Pin(ctx context.Context, repos []string) (adapter.View, func(), error) {
	all := c.load()
	v := adapter.View{}
	for _, r := range repos {
		v[r] = all[r]
	}
	return v, func() {}, nil
}
func (c *brokenCatalog) Previous(context.Context, string) (adapter.SegmentRef, error) {
	return adapter.SegmentRef{}, adapter.ErrNotFound
}
func (c *brokenCatalog) Recover(context.Context) error { return nil }
func (c *brokenCatalog) Close() error                  { return nil }
func TestExternalKillRejectsNonAtomicSet(t *testing.T) {
	t.Setenv("GXSTUDY_BROKEN_CATALOG", "1")
	ctx := context.Background()
	dir := t.TempDir()
	a := adapter.SegmentRef{Repo: "a", Version: "old", Digest: "sealed"}
	b := adapter.SegmentRef{Repo: "b", Version: "old", Digest: "sealed"}
	na := a
	na.Version = "new"
	nb := b
	nb.Version = "new"
	old, next := adapter.View{"a": a, "b": b}, adapter.View{"a": na, "b": nb}
	cat, err := newBrokenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = cat.PublishSet(ctx, old, nil); err != nil {
		t.Fatal(err)
	}
	killed, err := killPublish(ctx, publishRequest{Dir: dir, Set: next, Old: old, External: true, DelayNS: int64(10 * time.Millisecond)}, filepath.Join(dir, "request.json"), "external")
	if err != nil || !killed {
		t.Fatal(killed, err)
	}
	got, done, err := cat.Pin(ctx, []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	done()
	if CheckViewAllowed(got, old, next, "either", func(adapter.SegmentRef) error { return nil }) == nil {
		t.Fatal("non-atomic set escaped external SIGKILL checker", got)
	}
}

func TestUnsupportedDoesNotAbortOtherMetrics(t *testing.T) {
	ctx := context.Background()
	seg := oracle.New(conformance.Fixture())
	qs := []workload.Query{{ID: "skip", Op: "content", Repo: "fixture", Key: "graph", K: 10}, {ID: "point", Op: "id", Repo: "fixture", Key: "a"}}
	node := conformance.Fixture().Nodes[0]
	want := []workload.Expected{{QueryID: "skip"}, {QueryID: "point", Answer: workload.Answer{Node: &node}}}
	ns, checks, err := measuredQueries(ctx, map[string]adapter.Segment{"fixture": seg}, nil, qs, want, adapter.Capabilities{Unsupported: map[string]string{"content": "not provided"}})
	if err != nil || len(ns["id"]) != 1 || len(ns["content"]) != 0 || checks[0].Status != "unsupported" || checks[1].Mismatch {
		t.Fatal(ns, checks, err)
	}
}

func TestOracleCatalogConformance(t *testing.T) {
	conformance.CheckCatalog(t, oracle.Backend{}, oracle.NewCatalog)
}

type hooksBackend struct {
	oracle.Backend
	resets, releases, resources int
}

func (b *hooksBackend) Reset(context.Context) error                       { b.resets++; return nil }
func (b *hooksBackend) Release(context.Context, adapter.SegmentRef) error { b.releases++; return nil }
func (b *hooksBackend) ResourceUsage(context.Context) (adapter.ResourceUsage, error) {
	b.resources++
	return adapter.ResourceUsage{RSSBytes: 123, DiskBytes: 456}, nil
}
func TestServerHooksAreCalledOutsideRunPhases(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	data := filepath.Join(dir, "tier")
	d := conformance.Fixture()
	if _, err := dataset.Write(filepath.Join(data, d.Repo), d); err != nil {
		t.Fatal(err)
	}
	qs := workload.Generate([]dataset.Dataset{d}, 7)
	s := map[string]adapter.Segment{d.Repo: oracle.New(d)}
	var expected []workload.Expected
	for _, q := range qs {
		a, err := workload.Evaluate(ctx, s, nil, q)
		if err != nil {
			t.Fatal(err)
		}
		expected = append(expected, workload.Expected{QueryID: q.ID, Answer: a})
	}
	if err := dataset.WriteJSON(filepath.Join(data, "queries.json"), qs); err != nil {
		t.Fatal(err)
	}
	if err := dataset.WriteJSON(filepath.Join(data, "expected.json"), expected); err != nil {
		t.Fatal(err)
	}
	if err := dataset.TierManifest(data, []string{d.Repo}, dataset.Manifest{Version: dataset.GeneratorVersion}); err != nil {
		t.Fatal(err)
	}
	b := &hooksBackend{}
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0755); err != nil {
		t.Fatal(err)
	}
	if err := runChild(b, oracle.NewCatalog, runRequest{Data: data, Out: out, Run: 1, Trials: 1}); err != nil {
		t.Fatal(err)
	}
	var report Result
	if err := dataset.ReadJSON(filepath.Join(out, "oracle-tier-01.json"), &report); err != nil {
		t.Fatal(err)
	}
	if report.Concurrent.AfterPublish.Count < 8*MinimumNewSamplesPerReader {
		t.Fatal("after-commit distribution lacks checked minimum")
	}
	if len(report.Concurrent.NewSamplesPerReader) != 8 {
		t.Fatal("missing new-version reader coverage")
	}
	for _, count := range report.Concurrent.NewSamplesPerReader {
		if count < MinimumNewSamplesPerReader {
			t.Fatal("new-version sample minimum not enforced")
		}
	}
	for _, stage := range stages {
		if report.Concurrent.CommitRaceWindows[stage] < MinimumCommitRaceSamples {
			t.Fatal("checkpoint sample minimum not enforced", stage)
		}
	}
	if b.resets != 2 || b.releases != 5 || b.resources < 8 {
		t.Fatal("server lifecycle not fully observed", b.resets, b.releases, b.resources)
	}
}

func TestPublicationStartDoesNotRequireVectorQuery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	base := conformance.Fixture()
	b := &trackedBackend{Backend: oracle.Backend{}}
	dir := t.TempDir()
	old, _, err := b.Build(ctx, base, filepath.Join(dir, "old"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := MakeChange(base)
	if err != nil {
		t.Fatal(err)
	}
	changed := adapter.ApplyChange(base, c)
	qs := []workload.Query{{ID: "point", Op: "id", Repo: base.Repo, Key: "a"}}
	n := base.Nodes[0]
	want := []workload.Expected{{QueryID: "point", Answer: workload.Answer{Node: &n}}}
	_, err = concurrent(ctx, b, oracle.NewCatalog, map[string]adapter.Segment{base.Repo: oracle.New(base)}, []dataset.Dataset{base}, old, changed, qs, want, filepath.Join(dir, "concurrent"))
	if err != nil {
		t.Fatal(err)
	}
}

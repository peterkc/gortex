package store_pebble

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/vfs"
	"github.com/cockroachdb/pebble/v2/vfs/errorfs"
	"github.com/zzet/gortex/internal/graph"
)

// One row frame per delivered candidate: request position and ordinal,
// exact native tuple digest, and structural digest of the complete decoded
// payload (including Meta types and nested values). Lengths and SHA-256s
// keep long-ID and row-limit fixtures bounded without losing any position.
type v3GoldenCase struct {
	Name     string
	Requests []byte // ordered SHA-256 per complete LinkRequest, including misses
	Trace    []byte // ordered fixed-size row frames
	Rows     int
	Bytes    int
	Charge   *CandidateBatchCharge `json:",omitempty"`
	Error    string
}

type v3GoldenExpectation struct {
	SHA256      string
	Requests    int
	TraceRows   int
	Rows, Bytes int
	Charge      *CandidateBatchCharge `json:",omitempty"`
	Error       string
}

type v3FixtureSuite struct {
	want map[string]v3GoldenExpectation
}

// The expected fingerprint commits the complete canonical H98 case, not a
// sample of its rows. The capture command is temporary and cannot update
// these expectations; no product test path writes a fixture.
func (s *v3FixtureSuite) expect(t *testing.T, c v3GoldenCase) {
	t.Helper()
	want, ok := s.want[c.Name]
	if !ok {
		t.Fatalf("unexpected or duplicate H98 golden case %q", c.Name)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprintf("%x", sha256.Sum256(raw))
	if got != want.SHA256 {
		t.Fatalf("H98 golden %s: got %s want %s (rows=%d bytes=%d error=%q)", c.Name, got, want.SHA256, c.Rows, c.Bytes, c.Error)
	}
	if len(c.Requests) != 32*want.Requests || len(c.Trace) != 72*want.TraceRows || c.Rows != want.Rows || c.Bytes != want.Bytes || c.Error != want.Error || !reflect.DeepEqual(c.Charge, want.Charge) {
		t.Fatalf("H98 golden %s: plain expectation disagrees with verified digest %s", c.Name, got)
	}
	delete(s.want, c.Name)
}

func runV3Fixture(t *testing.T, run func(*v3FixtureSuite, *testing.T)) {
	t.Helper()
	raw, err := os.ReadFile("testdata/rebuild_candidate_h98_goldens.json")
	if err != nil {
		t.Fatal(err)
	}
	var all map[string]v3GoldenExpectation
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatal(err)
	}
	if len(all) != 32 {
		t.Fatalf("H98 golden case count=%d, want 32", len(all))
	}
	s := &v3FixtureSuite{want: make(map[string]v3GoldenExpectation)}
	for name, digest := range all {
		if name == t.Name() || strings.HasPrefix(name, t.Name()+"/") {
			s.want[name] = digest
		}
	}
	if len(s.want) == 0 {
		t.Fatal("no H98 expectations for fixture")
	}
	run(s, t)
	if !t.Failed() && len(s.want) != 0 {
		t.Fatalf("unvisited H98 golden cases: %v", s.want)
	}
}

// The H98 subprocess includes its random private partition path and child
// process-exit detail in the wrapping error. Compare the owning stage and
// original logical error, not these deliberately absent v3 IPC details.
func v3ErrorClass(err error) string {
	if err == nil {
		return ""
	}
	if !errors.Is(err, ErrRebuildLink) {
		return err.Error()
	}
	for _, text := range []string{"candidate result exceeds link record or page bound", "merged candidate page exceeds row or byte bound", "candidate order exceeds link record or page bound", "pebble order exceeds 1024 sorted runs", "link request page exceeds wire byte bound", "candidate record exceeds 16777216 bytes", "fixture reducer refusal"} {
		if strings.Contains(err.Error(), text) {
			return text
		}
	}
	return err.Error()
}
func v3FixturePass(t *testing.T, ctx context.Context, budget int64, view RebuildView, nodes []*graph.Node) (*RebuildLink, error) {
	t.Helper()
	opts := RebuildLinkOptions{ScratchParent: t.TempDir(), SpillBytes: 1 << 20}
	if err := os.WriteFile(filepath.Join(opts.ScratchParent, "keep"), []byte("owner data"), 0600); err != nil {
		t.Fatal(err)
	}
	source := func(yield func(*graph.Node) error) error {
		for _, n := range nodes {
			if err := yield(n); err != nil {
				return err
			}
		}
		return nil
	}
	if budget == rebuildCandidateCacheBytes {
		return StartRebuildLink(ctx, opts, view, source)
	}
	return startRebuildLink(ctx, opts, view, budget, source)
}

func v3TraceCall(t *testing.T, ctx context.Context, p *RebuildLink, view RebuildView, reqs []LinkRequest, spill int, refuse bool) (trace []byte, seen []string, stats CandidatePageStats, err error) {
	t.Helper()
	last, ordinal := -1, uint32(0)
	stats, err = p.SelectCandidatePage(ctx, view, reqs, spill, func(pos int, n *graph.Node) error {
		if pos != last {
			last, ordinal = pos, 0
		}
		digest, err := v3Fingerprint(n)
		if err != nil {
			return err
		}
		key := sha256.Sum256(candidateOrderKey(reqs[pos], pos, n, uint64(ordinal)))
		trace = binary.BigEndian.AppendUint32(trace, uint32(pos))
		trace = binary.BigEndian.AppendUint32(trace, ordinal)
		trace = append(trace, key[:]...)
		trace = append(trace, digest[:]...)
		ordinal++
		seen = append(seen, n.ID)
		if refuse {
			return errors.New("fixture reducer refusal")
		}
		return nil
	})
	return
}

func v3RequestDigests(t *testing.T, reqs []LinkRequest) []byte {
	t.Helper()
	var out []byte
	for _, req := range reqs {
		raw, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(raw)
		out = append(out, digest[:]...)
	}
	return out
}

func (s *v3FixtureSuite) checkCall(t *testing.T, ctx context.Context, p *RebuildLink, view RebuildView, call int, reqs []LinkRequest, spill int, refuse bool, wantErr string) ([]string, CandidatePageStats) {
	t.Helper()
	trace, ids, stats, err := v3TraceCall(t, ctx, p, view, reqs, spill, refuse)
	c := v3GoldenCase{Name: fmt.Sprintf("%s/call=%d", t.Name(), call), Requests: v3RequestDigests(t, reqs), Trace: trace, Rows: stats.ResultRows, Bytes: stats.ResultBytes, Error: v3ErrorClass(err)}
	s.expect(t, c)
	if v3ErrorClass(err) != wantErr {
		t.Fatalf("candidate error=%v want %q", err, wantErr)
	}
	if wantErr != "" && wantErr != "fixture reducer refusal" && len(trace) != 0 {
		t.Fatal("visitor ran before staged limit checks")
	}
	return ids, stats
}

func (s *v3FixtureSuite) charge(t *testing.T, ctx context.Context, p *RebuildLink, view RebuildView, name string, req LinkRequest) CandidateBatchCharge {
	t.Helper()
	charge, err := p.ChargeCandidateRequest(ctx, view, req)
	if err != nil {
		t.Fatal(err)
	}
	s.expect(t, v3GoldenCase{Name: t.Name() + "/charge=" + name, Requests: v3RequestDigests(t, []LinkRequest{req}), Charge: &charge})
	return charge
}

func (s *v3FixtureSuite) pass(t *testing.T, view RebuildView, nodes []*graph.Node, budget int64) (context.Context, *RebuildLink) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	t.Cleanup(cancel)
	p, err := v3FixturePass(t, ctx, budget, view, nodes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return ctx, p
}

func v3RetainedCharge(c *v3Cache) int64 {
	return v3Alloc(int64(unsafe.Sizeof(*c))) + c.indexBytes + c.gens[0].charge + c.gens[1].charge
}

// Given two repositories, mixed/empty languages and duplicate occurrences,
// native order is independent of source order and of language request order.
// A checked miss has no visits. A live wildcard must reorder z/go vs a/python.
func (s *v3FixtureSuite) RebuildV3ExactOrderAndLive(t *testing.T) {
	a := &graph.Node{ID: "z", Name: "N", QualName: "q.N", RepoPrefix: "r", Language: "go"}
	b := &graph.Node{ID: "a", Name: "N", QualName: "q.N", RepoPrefix: "r", Language: "python"}
	c := &graph.Node{ID: "0", Name: "N", QualName: "q.N", RepoPrefix: "s", Language: ""}
	d := &graph.Node{ID: "e", Name: "N", QualName: "q.N", RepoPrefix: "r", Language: ""}
	dup := *a
	dup.Meta = map[string]any{"occurrence": 2}
	nodes := []*graph.Node{a, c, b, d, &dup}
	build := newBuild(t, FullProfile)
	build.AddBatch([]*graph.Node{a, b, c, d}, nil)
	view := build.RebuildView()
	ctx, v := s.pass(t, view, nodes, 512<<20)
	requests := []LinkRequest{
		{Kind: LinkQualName, Key: "q.N", AllRepos: true},
		{Kind: LinkQualName, Key: "q.N", RepoPrefix: "r", Languages: []string{"go"}},
		{Kind: LinkName, Key: "N", RepoPrefix: "r", Languages: []string{"python", "go", "", "go"}},
		{Kind: LinkName, Key: "N", RepoPrefix: "r"},
		{Kind: LinkName, Key: "missing", RepoPrefix: "r"},
		{Kind: LinkName, Key: "N", AllRepos: true},
		{Kind: LinkName, Key: "N", AllRepos: true, Languages: []string{"python", "go", ""}},
	}
	// The synthetic duplicate source is not one-to-one with the working
	// index; use a separate stationary fixture for admission-cache checks.
	// One multi-position fixture also proves positions and duplicate ordinals.
	ids, _ := s.checkCall(t, ctx, v, view, 0, requests, 1<<20, false, "")
	want := []string{"0", "a", "e", "z", "z", "z", "z", "e", "z", "z", "a", "a", "e", "z", "z", "a", "e", "z", "0", "e", "z", "a", "0"}
	if !slices.Equal(ids, want) {
		t.Fatalf("native order %v want %v", ids, want)
	}
	// This write must be visible on the next live page, but not in frozen Q.
	if len(v.v3.summaries) == 0 {
		t.Fatal("missing page-local summary cache")
	}
	v.ResetCandidatePage()
	if len(v.v3.summaries) != 0 {
		t.Fatal("summary cache survived page")
	}
	changed := *b
	changed.Name = "Changed"
	build.AddNode(&changed)
	// H98 admission still reads the current scratch index after a node write.
	// The immutable candidate payload stream stays frozen on both routes.
	changedReq := LinkRequest{Kind: LinkName, Key: "N", RepoPrefix: "r", Languages: []string{"python"}}
	oldCharge := s.charge(t, ctx, v, view, "after-write", changedReq)
	if oldCharge.Rows != 0 {
		t.Fatalf("fresh admission H98=%+v", oldCharge)
	}

	ids, _ = s.checkCall(t, ctx, v, view, 1, []LinkRequest{{Kind: LinkName, Key: "N", AllRepos: true}, {Kind: LinkName, Key: "Changed", AllRepos: true}, requests[0]}, 1<<20, false, "")
	if want := []string{"e", "z", "0", "a", "0", "a", "e", "z", "z"}; !slices.Equal(ids, want) {
		t.Fatalf("fresh live %v want %v", ids, want)
	}
	live := LinkRequest{Kind: LinkName, Key: "Changed", AllRepos: true}
	s.charge(t, ctx, v, view, "live-before", live)
	s.checkCall(t, ctx, v, view, 2, []LinkRequest{live, live}, 1<<20, false, "")
	changed.Meta = map[string]any{"now": strings.Repeat("fresh", 64)}
	build.AddNode(&changed)
	s.charge(t, ctx, v, view, "live-after", live)
	s.checkCall(t, ctx, v, view, 3, []LinkRequest{live, live}, 1<<20, false, "")
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
}

// Given JSON-extension metadata whose second encoding differs and a valid
// scratch index with an ID-only unsized entry, both remain scalar-admissible.
func (s *v3FixtureSuite) RebuildV3UnstableUnsized(t *testing.T) {
	n := &graph.Node{ID: "a", Name: "N", QualName: "q.N", RepoPrefix: "r", Meta: map[string]any{"score": json.Number("7")}}
	raw, err := encodeNode(n)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeNode(raw)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := encodeNode(decoded)
	if err != nil || bytes.Equal(raw, canonical) {
		t.Fatalf("not an unstable fixture: %v", err)
	}
	build := newBuild(t, FullProfile)
	build.AddNode(n)
	view := build.RebuildView()
	if err := build.pending.Set(rebuildCandidateKey(nameRebuildScoped, n), []byte(n.ID), pebble.NoSync); err != nil {
		t.Fatal(err)
	}
	ctx, v := s.pass(t, view, []*graph.Node{n}, 512<<20)
	req := LinkRequest{Kind: LinkName, Key: "N", RepoPrefix: "r"}
	hc := s.charge(t, ctx, v, view, "unsized", req)
	if !hc.Oversize {
		t.Fatalf("H98=%+v", hc)
	}
	s.checkCall(t, ctx, v, view, 0, []LinkRequest{req, {Kind: LinkQualName, Key: "q.N", AllRepos: true}}, 1<<20, false, "")
}

// A deliberately tiny retained budget forces eviction and re-read between the
// stage-check walk and the delivery walk inside one complete scalar call.
func (s *v3FixtureSuite) RebuildV3EvictionWithinCall(t *testing.T) {
	nodes := make([]*graph.Node, 0, 240)
	for i := range 240 {
		nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("%04d", i), Name: "N", RepoPrefix: "r", Meta: map[string]any{"value": strings.Repeat("p", 2048)}})
	}
	ctx, v := s.pass(t, RebuildView{}, nodes, 512<<10)
	s.checkCall(t, ctx, v, RebuildView{}, 0, []LinkRequest{{Kind: LinkName, Key: "N", RepoPrefix: "r"}}, 1<<20, false, "")
	c := v.v3.cache
	sum, err := v.v3.requestSummary(ctx, LinkRequest{Kind: LinkName, Key: "N", RepoPrefix: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if v3RetainedCharge(c) > 512<<10 || c.slot(uint64(sum.first)) != nil || c.gens[0].head == nil && c.gens[1].head == nil {
		t.Fatalf("first piece should be evicted with other retained rows: charge=%d", v3RetainedCharge(c))
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
}

// Construct a canonical payload with the requested order-record byte size.
func v3SizedNode(t *testing.T, id, name string, want int) *graph.Node {
	t.Helper()
	n := &graph.Node{ID: id, Name: name, RepoPrefix: "r", Meta: map[string]any{"p": ""}}
	req := LinkRequest{Kind: LinkName, Key: name, RepoPrefix: "r"}
	payload := 0
	for range 6 {
		n.Meta["p"] = strings.Repeat("x", payload)
		raw, err := encodeNode(n)
		if err != nil {
			t.Fatal(err)
		}
		delta := want - len(raw) - len(candidateOrderKey(req, 0, n, 0))
		if delta == 0 {
			return n
		}
		payload += delta
		if payload < 0 {
			t.Fatal("negative fixture padding")
		}
	}
	t.Fatal("failed to construct exact record bytes")
	return nil
}
func (s *v3FixtureSuite) RebuildV3LogicalRecordAndByteLimits(t *testing.T) {
	t.Run("record-build-over", func(t *testing.T) {
		n := &graph.Node{ID: "a", Name: "N", Meta: map[string]any{"p": strings.Repeat("x", linkRecordBytes)}}
		ctx := t.Context()
		budget := int64(rebuildCandidateCacheBytes)
		p, err := v3FixturePass(t, ctx, budget, RebuildView{}, []*graph.Node{n})
		if p != nil {
			_ = p.Close()
		}
		s.expect(t, v3GoldenCase{Name: t.Name(), Error: v3ErrorClass(err)})
		if v3ErrorClass(err) != "candidate record exceeds 16777216 bytes" {
			t.Fatalf("budget=%d err=%v", budget, err)
		}
	})
	t.Run("long-id-at-limit", func(t *testing.T) {
		n := v3SizedNode(t, strings.Repeat("x", (linkRecordBytes/2)-512), "N", linkRecordBytes)
		ctx, v := s.pass(t, RebuildView{}, []*graph.Node{n}, 512<<20)
		_, st := s.checkCall(t, ctx, v, RebuildView{}, 0, []LinkRequest{{Kind: LinkName, Key: "N", RepoPrefix: "r"}}, 1<<20, false, "")
		if st.ResultBytes != linkRecordBytes {
			t.Fatal(st)
		}
	})
	t.Run("long-id-over-limit", func(t *testing.T) {
		n := v3SizedNode(t, strings.Repeat("x", (linkRecordBytes/2)-512), "N", linkRecordBytes+1)
		ctx, v := s.pass(t, RebuildView{}, []*graph.Node{n}, 512<<20)
		s.checkCall(t, ctx, v, RebuildView{}, 0, []LinkRequest{{Kind: LinkName, Key: "N", RepoPrefix: "r"}}, 1<<20, false, "candidate order exceeds link record or page bound")
	})
	for _, over := range []bool{false, true} {
		t.Run(fmt.Sprintf("bytes-over=%t", over), func(t *testing.T) {
			n := v3SizedNode(t, "a", "N", linkResultBytes/16)
			other := &graph.Node{ID: "b", Name: "Y", RepoPrefix: "r"}
			ctx, v := s.pass(t, RebuildView{}, []*graph.Node{n, other}, 512<<20)
			req := LinkRequest{Kind: LinkName, Key: "N", RepoPrefix: "r"}
			reqs := make([]LinkRequest, 16)
			for i := range reqs {
				reqs[i] = req
			}
			want := ""
			if over {
				reqs = append(reqs, LinkRequest{Kind: LinkName, Key: "Y", RepoPrefix: "r"})
				want = "candidate order exceeds link record or page bound"
			}
			_, st := s.checkCall(t, ctx, v, RebuildView{}, 0, reqs, 1<<20, false, want)
			if !over && st.ResultBytes != linkResultBytes {
				t.Fatal(st)
			}
		})
	}
}

func (s *v3FixtureSuite) RebuildV3LogicalRowAndRunLimits(t *testing.T) {
	for _, over := range []bool{false, true} {
		t.Run(fmt.Sprintf("rows-over=%t", over), func(t *testing.T) {
			rows := 64
			if over {
				rows++
			}
			nodes := make([]*graph.Node, 0, rows)
			for i := range rows {
				nodes = append(nodes, &graph.Node{ID: fmt.Sprint(i), Name: "N", RepoPrefix: "r"})
			}
			reqs := make([]LinkRequest, LinkPageRequests)
			for i := range reqs {
				reqs[i] = LinkRequest{Kind: LinkName, Key: "N", RepoPrefix: "r"}
			}
			ctx, v := s.pass(t, RebuildView{}, nodes, 512<<20)
			want := ""
			if over {
				want = "candidate result exceeds link record or page bound"
			}
			_, st := s.checkCall(t, ctx, v, RebuildView{}, 0, reqs, 1<<20, false, want)
			if !over && st.ResultRows != linkResultRows {
				t.Fatal(st)
			}
		})
	}
	for _, over := range []bool{false, true} {
		t.Run(fmt.Sprintf("runs-over=%t", over), func(t *testing.T) {
			rows := goldFileCap
			if over {
				rows++
			}
			nodes := make([]*graph.Node, 0, rows)
			for i := range rows {
				nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("%04d", rows-i), Name: "N", RepoPrefix: "r"})
			}
			ctx, v := s.pass(t, RebuildView{}, nodes, 512<<20)
			want := ""
			if over {
				want = "pebble order exceeds 1024 sorted runs"
			}
			s.checkCall(t, ctx, v, RebuildView{}, 0, []LinkRequest{{Kind: LinkName, Key: "N", RepoPrefix: "r"}}, 1, over, want)
		})
	}
}

// H96 type-batch aggregate wire refusal must precede a scalar-admissible
// request or reducer failure; each admitted singleton remains a checked miss.
func (s *v3FixtureSuite) RebuildV3WireAndReducerPrecedence(t *testing.T) {
	ctx, v := s.pass(t, RebuildView{}, []*graph.Node{{ID: "a", Name: "N"}}, 512<<20)
	req := LinkRequest{Kind: LinkName, Key: strings.Repeat("q", linkFrameBytes/2)}
	s.checkCall(t, ctx, v, RebuildView{}, 0, []LinkRequest{req}, 1<<20, false, "")
	s.checkCall(t, ctx, v, RebuildView{}, 1, []LinkRequest{req, req}, 1<<20, true, "link request page exceeds wire byte bound")
	ctx, v = s.pass(t, RebuildView{}, []*graph.Node{{ID: "a", Name: "N"}}, 512<<20)
	// The fixture call ordinal spans passes, so the capture key stays unique.
	s.checkCall(t, ctx, v, RebuildView{}, 2, []LinkRequest{{Kind: LinkName, Key: "N"}}, 1<<20, true, "fixture reducer refusal")
}

func (s *v3FixtureSuite) RebuildV3ExceptionalMapStream(t *testing.T) {
	m := map[string]any{}
	for i := range 897 {
		m[fmt.Sprint(i)] = true
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	deep := json.RawMessage(strings.Repeat("[", 1001) + "null" + strings.Repeat("]", 1001))
	nodes := []*graph.Node{{ID: "a", Name: "N", Meta: map[string]any{"native": m}}, {ID: "b", Name: "N", Meta: map[string]any{"extension": json.RawMessage(raw)}}, {ID: "c", Name: "N"}, {ID: "d", Name: "N", Meta: map[string]any{"deep": deep}}}
	ctx, v := s.pass(t, RebuildView{}, nodes, 512<<20)
	s.checkCall(t, ctx, v, RebuildView{}, 0, []LinkRequest{{Kind: LinkName, Key: "N"}}, 1<<20, false, "")
	c := v.v3.cache
	for _, g := range c.gens {
		for e := g.head; e != nil; e = e.next {
			if e.unbounded {
				t.Fatal("exceptional piece retained")
			}
		}
	}
}

// The dense file index cannot fit in this budget. More distinct pieces than
// slots must stream without refusing or changing a single delivery.
func (s *v3FixtureSuite) RebuildV3BoundedIndex(t *testing.T) {
	const budget = 4 << 10
	var nodes []*graph.Node
	var reqs []LinkRequest
	for i := range 256 {
		name := fmt.Sprintf("N%04d", i)
		nodes = append(nodes, &graph.Node{ID: name, Name: name, RepoPrefix: "r"})
		reqs = append(reqs, LinkRequest{Kind: LinkName, Key: name, RepoPrefix: "r"})
	}
	ctx, v := s.pass(t, RebuildView{}, nodes, budget)
	s.checkCall(t, ctx, v, RebuildView{}, 0, reqs, 1<<20, false, "")
	s.checkCall(t, ctx, v, RebuildView{}, 1, reqs, 1<<20, false, "")
	c := v.v3.cache
	if c.pieceCount <= uint64(len(c.index)) || len(c.index) == 0 || v3RetainedCharge(c) > budget {
		t.Fatalf("slots=%d pieces=%d charge=%d", len(c.index), c.pieceCount, v3RetainedCharge(c))
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
}

// An ordinary stationary index can answer from its S summary. A same-key
// payload update changes the working admission size, not the frozen rows.
func (s *v3FixtureSuite) RebuildV3AdmissionEpoch(t *testing.T) {
	build := newBuild(t, FullProfile)
	n := &graph.Node{ID: "a", Name: "N", QualName: "q.N", RepoPrefix: "r", Language: "go", Meta: map[string]any{"payload": "x"}}
	build.AddNode(n)
	view := build.RebuildView()
	ctx, v := s.pass(t, view, []*graph.Node{n}, rebuildCandidateCacheBytes)
	req := LinkRequest{Kind: LinkName, Key: "N", RepoPrefix: "r", Languages: []string{"go"}}
	before := s.charge(t, ctx, v, view, "sized", req)
	if before.Oversize || before.Rows != 1 {
		t.Fatal(before)
	}
	if len(v.v3.summaries) != 1 {
		t.Fatal("sized admission did not use S summary")
	}
	s.charge(t, ctx, v, view, "repeat", req)
	changed := *n
	changed.Meta = map[string]any{"payload": strings.Repeat("y", 1024)}
	build.AddNode(&changed)
	after := s.charge(t, ctx, v, view, "node-write", req)
	if after.Bytes <= before.Bytes || after.Rows != 1 {
		t.Fatalf("before=%+v after=%+v", before, after)
	}
	s.checkCall(t, ctx, v, view, 0, []LinkRequest{req, {Kind: LinkName, Key: "N", AllRepos: true}}, 1<<20, false, "")
}

func TestRebuildV3ExactOrderAndLive(t *testing.T) {
	runV3Fixture(t, (*v3FixtureSuite).RebuildV3ExactOrderAndLive)
}
func TestRebuildV3UnstableUnsized(t *testing.T) {
	runV3Fixture(t, (*v3FixtureSuite).RebuildV3UnstableUnsized)
}
func TestRebuildV3EvictionWithinCall(t *testing.T) {
	runV3Fixture(t, (*v3FixtureSuite).RebuildV3EvictionWithinCall)
}
func TestRebuildV3LogicalRecordAndByteLimits(t *testing.T) {
	runV3Fixture(t, (*v3FixtureSuite).RebuildV3LogicalRecordAndByteLimits)
}
func TestRebuildV3LogicalRowAndRunLimits(t *testing.T) {
	runV3Fixture(t, (*v3FixtureSuite).RebuildV3LogicalRowAndRunLimits)
}
func TestRebuildV3WireAndReducerPrecedence(t *testing.T) {
	runV3Fixture(t, (*v3FixtureSuite).RebuildV3WireAndReducerPrecedence)
}
func TestRebuildV3ExceptionalMapStream(t *testing.T) {
	runV3Fixture(t, (*v3FixtureSuite).RebuildV3ExceptionalMapStream)
}
func TestRebuildV3BoundedIndex(t *testing.T) {
	runV3Fixture(t, (*v3FixtureSuite).RebuildV3BoundedIndex)
}
func TestRebuildV3AdmissionEpoch(t *testing.T) {
	runV3Fixture(t, (*v3FixtureSuite).RebuildV3AdmissionEpoch)
}

func (s *v3FixtureSuite) RebuildV3ReadFailure(t *testing.T) {
	for _, mode := range []string{"integrity", "cancel", "live-index"} {
		t.Run(mode, func(t *testing.T) {
			build := newBuild(t, FullProfile)
			nodes := []*graph.Node{{ID: "a", Name: "A"}, {ID: "b", Name: "B"}, {ID: "c", Name: "B"}}
			build.AddBatch(nodes, nil)
			view := build.RebuildView()
			if mode == "live-index" {
				key := keyStrings([]byte{familyName, nameRebuildScoped}, "A", "", "", "z")
				if err := build.pending.Set(key, []byte("z"), pebble.NoSync); err != nil {
					t.Fatal(err)
				}
			}
			reqs := []LinkRequest{{Kind: LinkName, Key: "B"}, {Kind: LinkName, Key: "A", AllRepos: true}}
			budget := int64(rebuildCandidateCacheBytes)
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			p, err := v3FixturePass(t, ctx, budget, view, nodes)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			if mode == "integrity" {
				f, err := os.OpenFile(filepath.Join(p.dir, "v3.sst"), os.O_WRONLY, 0)
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				_, err = f.WriteAt(bytes.Repeat([]byte{0xff}, 8), 0)
				if err = errors.Join(err, f.Close()); err != nil {
					cancel()
					t.Fatal(err)
				}
			}
			page, cancelPage := context.WithCancel(ctx)
			visits := 0
			var trace []byte
			stats, err := p.SelectCandidatePage(page, view, reqs, 1, func(pos int, n *graph.Node) error {
				visits++
				digest, e := v3Fingerprint(n)
				if e != nil {
					return e
				}
				key := sha256.Sum256(candidateOrderKey(reqs[pos], pos, n, 0))
				trace = binary.BigEndian.AppendUint32(trace, uint32(pos))
				trace = binary.BigEndian.AppendUint32(trace, 0)
				trace = append(trace, key[:]...)
				trace = append(trace, digest[:]...)
				if mode == "cancel" {
					cancelPage()
				}
				return nil
			})
			cancelPage()
			if !errors.Is(err, ErrRebuildLink) || stats != (CandidatePageStats{}) {
				t.Fatalf("mode=%s stats=%+v err=%v", mode, stats, err)
			}
			switch mode {
			case "integrity":
				if visits != 0 {
					t.Fatalf("integrity visits=%d err=%v", visits, err)
				}
			case "cancel":
				if visits != 1 || !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel visits=%d err=%v", visits, err)
				}
			case "live-index":
				if visits != 0 || !errors.Is(err, ErrRebuildView) || !strings.Contains(err.Error(), "stale working candidate index entry") {
					t.Fatalf("live index visits=%d err=%v", visits, err)
				}
			}
			s.expect(t, v3GoldenCase{Name: t.Name(), Requests: v3RequestDigests(t, reqs), Trace: trace, Error: mode})
			entries, e := os.ReadDir(filepath.Dir(p.dir))
			if e != nil || len(entries) != 1 || entries[0].Name() != "keep" {
				t.Fatalf("scratch after failure %v %v", entries, e)
			}
			if _, e := p.SelectCandidatePage(ctx, view, reqs, 1, func(int, *graph.Node) error { t.Fatal("retry visited"); return nil }); !errors.Is(e, ErrRebuildLink) {
				t.Fatalf("failed pass admitted retry: %v", e)
			}
			cancel()
		})
	}
}

func TestRebuildV3ReadFailure(t *testing.T) { runV3Fixture(t, (*v3FixtureSuite).RebuildV3ReadFailure) }

func TestRebuildV3BorrowedPieceReleased(t *testing.T) {
	ctx := t.Context()
	n := &graph.Node{ID: "a", Name: "N", Meta: map[string]any{"p": strings.Repeat("x", v3PieceBytes)}}
	v, err := v3FixturePass(t, ctx, rebuildCandidateCacheBytes, RebuildView{}, []*graph.Node{n})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = v.Close() }()
	req := LinkRequest{Kind: LinkName, Key: "N"}
	ids := []string{}
	_, err = v.SelectCandidatePage(ctx, RebuildView{}, []LinkRequest{req}, 1<<20, func(_ int, n *graph.Node) error {
		ids = append(ids, n.ID)
		return nil
	})
	if err != nil || !slices.Equal(ids, []string{"a"}) {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
	if v.v3.cache.borrowed != nil {
		t.Fatal("oversized borrowed piece survived candidate call")
	}
	failure := errors.New("fixture visitor refusal")
	err = v.v3.walk(ctx, req, func(r v3CachedRow) error {
		r.node.Name = "mutated"
		return failure
	})
	if !errors.Is(err, failure) || !strings.Contains(err.Error(), "mutated") || v.v3.cache.borrowed != nil {
		t.Fatalf("borrowed error release: %v", err)
	}
}

// A filtered summary cannot bypass an earlier malformed index row or omit a
// valid index-only charge for a stale row absent from the frozen node source.
func TestRebuildV3ChargeIndexErrors(t *testing.T) {
	for _, mode := range []string{"other-language", "dangling", "qualified-dangling"} {
		t.Run(mode, func(t *testing.T) {
			build := newBuild(t, FullProfile)
			n := &graph.Node{ID: "a", Name: "N", QualName: "q.N", RepoPrefix: "r", Language: "go"}
			build.AddNode(n)
			view := build.RebuildView()
			bad := &graph.Node{ID: "z", Name: "N", QualName: "q.N", RepoPrefix: "r", Language: "go"}
			column := nameRebuildScoped
			req := LinkRequest{Kind: LinkName, Key: "N", RepoPrefix: "r", Languages: []string{"go"}}
			value := binary.BigEndian.AppendUint64([]byte(bad.ID), 100)
			if mode == "other-language" {
				bad.Language = "python"
				value = []byte("wrong-id")
			} else if mode == "qualified-dangling" {
				column = nameQualified
				req = LinkRequest{Kind: LinkQualName, Key: "q.N", AllRepos: true}
			}
			if err := build.pending.Set(rebuildCandidateKey(column, bad), value, pebble.NoSync); err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			v, err := v3FixturePass(t, ctx, rebuildCandidateCacheBytes, view, []*graph.Node{n})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = v.Close() }()
			want, wantErr := view.ChargeCandidateRequest(ctx, req)
			got, err := v.ChargeCandidateRequest(ctx, view, req)
			if mode == "other-language" {
				if !errors.Is(wantErr, ErrRebuildView) || !errors.Is(wantErr, errInvalidRecord) || !errors.Is(err, ErrRebuildView) || !errors.Is(err, errInvalidRecord) || err.Error() != wantErr.Error() || got != want {
					t.Fatalf("filtered malformed index: got %+v %v want %+v %v", got, err, want, wantErr)
				}
			} else if wantErr != nil || want.Rows != 2 || got != want || err != nil {
				t.Fatalf("dangling index charge: got %+v %v want %+v %v", got, err, want, wantErr)
			}
			// A separately scoped miss remains an ordinary checked miss.
			miss := LinkRequest{Kind: LinkName, Key: "N", RepoPrefix: "other", Languages: []string{"go"}}
			if got, err := v.ChargeCandidateRequest(ctx, view, miss); got != (CandidateBatchCharge{}) || err != nil {
				t.Fatalf("unrelated scope: %+v %v", got, err)
			}
		})
	}
}

func TestRebuildV3BuildIndexReadError(t *testing.T) {
	failure := errors.New("fixture index read failure")
	fault := &errorfs.Toggle{Injector: errorfs.InjectorFunc(func(op errorfs.Op) error {
		if strings.HasSuffix(op.Path, ".sst") && (op.Kind == errorfs.OpOpen || op.Kind == errorfs.OpFileReadAt || op.Kind == errorfs.OpFileRead) {
			return failure
		}
		return nil
	})}
	opts := pendingOptions(nil)
	opts.FS = errorfs.Wrap(vfs.NewMem(), fault)
	opts.CacheSize = 1
	db, err := pebble.Open("pending", opts)
	if err != nil {
		t.Fatal(err)
	}
	n := &graph.Node{ID: "a", Name: "N", RepoPrefix: "r", Language: "go"}
	key := rebuildCandidateKey(nameRebuildScoped, n)
	if err := db.Set(key, binary.BigEndian.AppendUint64([]byte(n.ID), 100), pebble.NoSync); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Flush(); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	opts.ErrorIfExists = false
	db, err = pebble.Open("pending", opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	view := (&BuildStore{pending: db}).RebuildView()
	parent := t.TempDir()
	p, err := StartRebuildLink(t.Context(), RebuildLinkOptions{ScratchParent: parent, SpillBytes: 1 << 20}, view, func(yield func(*graph.Node) error) error {
		fault.On()
		defer fault.Off()
		return yield(n)
	})
	if p != nil {
		_ = p.Close()
	}
	if !errors.Is(err, ErrRebuildLink) || !errors.Is(err, failure) {
		t.Fatalf("candidate build hid scratch read error: %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed build left scratch: %v %v", entries, err)
	}
}

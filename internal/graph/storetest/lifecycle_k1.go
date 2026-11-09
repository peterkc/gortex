package storetest

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// K1 Primary cold chain (design-c.md 1.8). Given empty derived storage and F,
// when B, G, S, P, O and G complete, then both directions, lookups, ranking
// and corpora, routes and provenance match F at E0. After the E1 update an
// old pin still sees E0 and a new pin sees E1. A second checkout stays
// independent. No private version is directly servable.

// testK1ColdChain publishes every checkout of F at E0 and checks each served
// version against F.
func testK1ColdChain(t *testing.T, backend Backend) {
	ctx, f, inst := newK1Chain(t, backend)
	for _, c := range f.Checkouts {
		version := publishVersion(t, ctx, inst, f, c.ID, E0)
		view := openView(t, ctx, inst, c.ID)
		if view.Version() != version {
			t.Fatalf("%s: a new pin opened %q, want the published version %q", c.ID, view.Version(), version)
		}
		assertServesF(t, ctx, view, f, c.ID, E0)
	}
}

// testK1UpdatePins publishes E1 for R1 and S1 while pins on their E0 versions
// are open: the old pins keep answering E0, new pins answer E1.
func testK1UpdatePins(t *testing.T, backend Backend) {
	ctx, f, inst := newK1Chain(t, backend)
	old := map[string]View{}
	for _, c := range []string{CheckoutR1, CheckoutS1} {
		publishVersion(t, ctx, inst, f, c, E0)
		old[c] = openView(t, ctx, inst, c)
	}
	for _, c := range []string{CheckoutS1, CheckoutR1} {
		e1 := publishVersion(t, ctx, inst, f, c, E1)
		if e1 == old[c].Version() {
			t.Fatalf("%s: E1 published as %q, the version E0 already holds", c, e1)
		}
	}
	for _, c := range []string{CheckoutR1, CheckoutS1} {
		assertServesF(t, ctx, old[c], f, c, E0)
		assertServesF(t, ctx, openView(t, ctx, inst, c), f, c, E1)
	}
}

// testK1SecondCheckout updates R1 to E1 and checks that R2, the second
// checkout of R, keeps its own E0 version and answers.
func testK1SecondCheckout(t *testing.T, backend Backend) {
	ctx, f, inst := newK1Chain(t, backend)
	r1E0 := publishVersion(t, ctx, inst, f, CheckoutR1, E0)
	r2E0 := publishVersion(t, ctx, inst, f, CheckoutR2, E0)
	if r1E0 == r2E0 {
		t.Fatalf("R1 and R2 share version %q; each checkout needs its own", r1E0)
	}
	before, err := inst.Current(ctx, CheckoutR2)
	if err != nil {
		t.Fatalf("current publication of R2: %v", err)
	}
	r1E1 := publishVersion(t, ctx, inst, f, CheckoutR1, E1)
	after, err := inst.Current(ctx, CheckoutR2)
	if err != nil {
		t.Fatalf("current publication of R2 after the R1 update: %v", err)
	}
	if after != before {
		t.Fatalf("publishing R1's E1 (%q) moved R2's publication from %+v to %+v", r1E1, before, after)
	}
	assertServesF(t, ctx, openView(t, ctx, inst, CheckoutR2), f, CheckoutR2, E0)
	assertServesF(t, ctx, openView(t, ctx, inst, CheckoutR1), f, CheckoutR1, E1)
}

// testK1PrivateNotServed builds E1 for R1 up to and through the seal without
// publishing it: every pin opened meanwhile still serves E0.
func testK1PrivateNotServed(t *testing.T, backend Backend) {
	ctx, f, inst := newK1Chain(t, backend)
	e0 := publishVersion(t, ctx, inst, f, CheckoutR1, E0)
	build, err := inst.Begin(ctx, BuildRequest{Checkout: CheckoutR1, Inputs: f.Inputs(CheckoutR1, E1), Operation: "build-private-E1"})
	if err != nil {
		t.Fatalf("begin E1: %v", err)
	}
	if build.Version() == e0 {
		t.Fatalf("the E1 build reused the published E0 version %q", e0)
	}
	checkStillE0 := func(stage string) {
		t.Helper()
		view := openView(t, ctx, inst, CheckoutR1)
		if view.Version() != e0 {
			t.Fatalf("%s: a new pin opened %q, want the published E0 %q", stage, view.Version(), e0)
		}
		assertServesF(t, ctx, view, f, CheckoutR1, E0)
	}
	checkStillE0("after begin")
	if err := build.Write(ctx, f.Payload("r", E1)); err != nil {
		t.Fatalf("write E1: %v", err)
	}
	checkStillE0("after the private write")
	if err := build.Seal(ctx); err != nil {
		t.Fatalf("seal E1: %v", err)
	}
	checkStillE0("after the seal")
}

// newK1Chain opens an instance over empty storage and tracks F's checkouts.
func newK1Chain(t *testing.T, backend Backend) (context.Context, *FixtureF, Instance) {
	t.Helper()
	ctx := caseContext(t)
	f := NewFixtureF(t)
	inst := newChainIn(t, ctx, backend, t.TempDir(), nil, f, CheckoutR1, CheckoutR2, CheckoutS1)
	for _, c := range f.Checkouts {
		current, err := inst.Current(ctx, c.ID)
		if err != nil {
			t.Fatalf("current publication of %s: %v", c.ID, err)
		}
		if current.Version != "" {
			t.Fatalf("%s serves %q before any build", c.ID, current.Version)
		}
	}
	return ctx, f, inst
}

// publishVersion runs B, G, S and P for one checkout at epoch and returns the
// version the checkout now serves.
func publishVersion(t *testing.T, ctx context.Context, inst Instance, f *FixtureF, checkout string, epoch Epoch) Version {
	t.Helper()
	current, err := inst.Current(ctx, checkout)
	if err != nil {
		t.Fatalf("%s: current publication: %v", checkout, err)
	}
	build := sealedBuild(t, ctx, inst, checkout, f.Inputs(checkout, epoch),
		fmt.Sprintf("build-%s-E%d", checkout, epoch), f.Payload(f.RepositoryOf(checkout).Prefix, epoch))
	if err := build.Publish(ctx, current, fmt.Sprintf("publish-%s-E%d", checkout, epoch)); err != nil {
		t.Fatalf("%s E%d: publish over %+v: %v", checkout, epoch, current, err)
	}
	after, err := inst.Current(ctx, checkout)
	if err != nil {
		t.Fatalf("%s E%d: current publication after publish: %v", checkout, epoch, err)
	}
	if after.Version != build.Version() {
		t.Fatalf("%s E%d: the checkout serves %q after publishing %q", checkout, epoch, after.Version, build.Version())
	}
	return build.Version()
}

func openView(t *testing.T, ctx context.Context, inst Instance, checkout string) View {
	t.Helper()
	view, err := inst.Open(ctx, checkout)
	if err != nil {
		t.Fatalf("%s: open: %v", checkout, err)
	}
	t.Cleanup(view.Release)
	return view
}

// assertServesF checks that view serves exactly F's version of checkout at
// epoch: provenance, every node and edge with all fields, both edge
// directions, name lookups, file metadata, and the symbol and vector corpora.
func assertServesF(t *testing.T, ctx context.Context, view View, f *FixtureF, checkout string, epoch Epoch) {
	t.Helper()
	prefix := f.RepositoryOf(checkout).Prefix
	label := fmt.Sprintf("%s at E%d (version %q)", checkout, epoch, view.Version())
	provenance, err := view.Provenance(ctx)
	if err != nil {
		t.Fatalf("%s: provenance: %v", label, err)
	}
	if wantProv := (Provenance{Inputs: f.Inputs(checkout, epoch), Producers: f.Producers()}); !reflect.DeepEqual(provenance, wantProv) {
		t.Fatalf("%s: provenance = %+v, want %+v", label, provenance, wantProv)
	}
	assertServesPayload(t, label, view.Reader(), prefix, f.Payload(prefix, epoch), true)
}

// assertServesPayload checks that reader serves exactly want: every node and
// edge with all fields, both edge directions and name lookups, and, when
// corpora is set, file metadata and the symbol and vector corpora.
func assertServesPayload(t *testing.T, label string, reader graph.Reader, prefix string, want Payload, corpora bool) {
	t.Helper()
	// Nodes: the complete set, each with every field.
	assertNodes(t, label+": AllNodes", reader.AllNodes(), want.Nodes)
	for _, n := range want.Nodes {
		assertNodes(t, label+": GetNode("+n.ID+")", []*graph.Node{reader.GetNode(n.ID)}, []*graph.Node{n})
	}

	// Edges: the complete multiset, then both directions of every endpoint.
	assertEdges(t, label+": AllEdges", reader.AllEdges(), want.Edges)
	endpoints := map[string]bool{}
	for _, e := range want.Edges {
		endpoints[e.From], endpoints[e.To] = true, true
	}
	for _, n := range want.Nodes {
		endpoints[n.ID] = true
	}
	for id := range endpoints {
		assertEdges(t, label+": GetOutEdges("+id+")", reader.GetOutEdges(id),
			filterEdges(want.Edges, func(e *graph.Edge) bool { return e.From == id }))
		assertEdges(t, label+": GetInEdges("+id+")", reader.GetInEdges(id),
			filterEdges(want.Edges, func(e *graph.Edge) bool { return e.To == id }))
	}

	// Lookups: same-named types resolve only inside this repository, and the
	// failed name lookup stays failed.
	names := []string{"Client", "Run", "Twice", "Serve", "Missing"}
	for _, n := range want.Nodes {
		if !slices.Contains(names, n.Name) {
			names = append(names, n.Name)
		}
	}
	for _, name := range names {
		assertNodes(t, label+": FindNodesByName("+name+")", reader.FindNodesByName(name),
			filterNodes(want.Nodes, func(n *graph.Node) bool { return n.Name == name }))
	}
	assertNodes(t, label+": GetNodeByQualName(api.Client)",
		[]*graph.Node{reader.GetNodeByQualName("api.Client")},
		filterNodes(want.Nodes, func(n *graph.Node) bool { return n.QualName == "api.Client" }))
	if !corpora {
		return
	}

	// File metadata, a required capability.
	files, ok := reader.(graph.FileMetaReader)
	if !ok {
		t.Fatalf("%s: the reader lacks the required graph.FileMetaReader", label)
	}
	gotFiles, err := files.FileMetasForRepo(prefix)
	if err != nil {
		t.Fatalf("%s: FileMetasForRepo: %v", label, err)
	}
	slices.SortFunc(gotFiles, func(a, b graph.FileMetaRow) int { return cmp.Compare(a.FilePath, b.FilePath) })
	wantFiles := slices.Clone(want.Files)
	slices.SortFunc(wantFiles, func(a, b graph.FileMetaRow) int { return cmp.Compare(a.FilePath, b.FilePath) })
	if !reflect.DeepEqual(gotFiles, wantFiles) {
		t.Fatalf("%s: file metadata = %+v, want %+v", label, gotFiles, wantFiles)
	}

	assertSymbolCorpus(t, label, reader, want)
	assertVectorCorpus(t, label, reader, want)
}

// assertSymbolCorpus searches each symbol's token and the failed lookup's.
// Each token belongs to exactly the nodes that carry it in this version, so
// the hits are those nodes, in non-increasing score order.
func assertSymbolCorpus(t *testing.T, label string, reader graph.Reader, want Payload) {
	t.Helper()
	search, ok := reader.(graph.SymbolSearcher)
	if !ok {
		t.Fatalf("%s: the reader lacks the required graph.SymbolSearcher", label)
	}
	tokens := []string{"client", "run", "twice", "serve", "missing"}
	for _, s := range want.Symbols {
		if !slices.Contains(tokens, s.Tokens) {
			tokens = append(tokens, s.Tokens)
		}
	}
	for _, token := range tokens {
		hits, err := search.SearchSymbols(token, 10)
		if err != nil {
			t.Fatalf("%s: SearchSymbols(%q): %v", label, token, err)
		}
		var got, wantIDs []string
		for i, h := range hits {
			got = append(got, h.NodeID)
			if i > 0 && h.Score > hits[i-1].Score {
				t.Fatalf("%s: SearchSymbols(%q) ranks %v out of score order", label, token, hits)
			}
		}
		for _, s := range want.Symbols {
			if s.Tokens == token {
				wantIDs = append(wantIDs, s.NodeID)
			}
		}
		slices.Sort(got)
		slices.Sort(wantIDs)
		if !slices.Equal(got, wantIDs) {
			t.Fatalf("%s: SearchSymbols(%q) = %v, want %v", label, token, got, wantIDs)
		}
	}
}

// assertVectorCorpus queries with each stored vector: every vector of the
// version comes back once, ascending by cosine distance, with the distance F's
// vectors give, and the query's own node first at distance zero.
func assertVectorCorpus(t *testing.T, label string, reader graph.Reader, want Payload) {
	t.Helper()
	vectors, ok := reader.(graph.VectorSearcher)
	if !ok {
		t.Fatalf("%s: the reader lacks the required graph.VectorSearcher", label)
	}
	for _, query := range want.Vectors {
		hits, err := vectors.SimilarTo(query.Vec, len(want.Vectors)+1)
		if err != nil {
			t.Fatalf("%s: SimilarTo(%s): %v", label, query.NodeID, err)
		}
		if len(hits) != len(want.Vectors) || hits[0].NodeID != query.NodeID {
			t.Fatalf("%s: SimilarTo(%s) = %+v, want all %d vectors with the query's node first",
				label, query.NodeID, hits, len(want.Vectors))
		}
		seen := map[string]bool{}
		for i, h := range hits {
			stored := slices.IndexFunc(want.Vectors, func(v graph.VectorItem) bool { return v.NodeID == h.NodeID })
			if stored < 0 || seen[h.NodeID] || h.ParentID != "" {
				t.Fatalf("%s: SimilarTo(%s) returned %+v, not one of the version's vectors once", label, query.NodeID, h)
			}
			seen[h.NodeID] = true
			if d := cosineDistance(query.Vec, want.Vectors[stored].Vec); math.Abs(h.Distance-d) > 1e-6 {
				t.Fatalf("%s: SimilarTo(%s) puts %s at %v, want cosine distance %v", label, query.NodeID, h.NodeID, h.Distance, d)
			}
			if i > 0 && h.Distance < hits[i-1].Distance {
				t.Fatalf("%s: SimilarTo(%s) is not ascending: %+v", label, query.NodeID, hits)
			}
		}
	}
}

func cosineDistance(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	return 1 - dot/(math.Sqrt(na)*math.Sqrt(nb))
}

func filterNodes(nodes []*graph.Node, keep func(*graph.Node) bool) []*graph.Node {
	var out []*graph.Node
	for _, n := range nodes {
		if keep(n) {
			out = append(out, n)
		}
	}
	return out
}

func filterEdges(edges []*graph.Edge, keep func(*graph.Edge) bool) []*graph.Edge {
	var out []*graph.Edge
	for _, e := range edges {
		if keep(e) {
			out = append(out, e)
		}
	}
	return out
}

// assertNodes compares two node multisets field by field, ignoring order.
func assertNodes(t *testing.T, what string, got, want []*graph.Node) {
	t.Helper()
	key := func(n *graph.Node) string {
		if n == nil {
			return "<nil>"
		}
		return fmt.Sprintf("%#v", *n)
	}
	assertMultiset(t, what, got, want, key)
}

// assertEdges compares two edge multisets field by field, ignoring order.
func assertEdges(t *testing.T, what string, got, want []*graph.Edge) {
	t.Helper()
	key := func(e *graph.Edge) string {
		if e == nil {
			return "<nil>"
		}
		return fmt.Sprintf("%#v", *e)
	}
	assertMultiset(t, what, got, want, key)
}

func assertMultiset[T any](t *testing.T, what string, got, want []T, key func(T) string) {
	t.Helper()
	g, w := make([]string, 0, len(got)), make([]string, 0, len(want))
	for _, v := range got {
		g = append(g, key(v))
	}
	for _, v := range want {
		w = append(w, key(v))
	}
	slices.Sort(g)
	slices.Sort(w)
	if !slices.Equal(g, w) {
		t.Fatalf("%s:\n got  %s\n want %s", what, strings.Join(g, "\n      "), strings.Join(w, "\n      "))
	}
}

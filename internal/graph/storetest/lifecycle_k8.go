package storetest

import (
	"fmt"
	"slices"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// K8 Unsupported capability (design-c.md 1.8 and 1.7). Given one missing
// required search or receipt capability, when opening that profile, then
// startup or build refuses. Given an optional acceleration missing, then its
// declared reference path or visible unavailable result is asserted. No
// passing skip is accepted as parity.
//
// The suite classifies the capabilities below for the default profile. A
// required one must be present on every backend in the list; an optional one
// names its reference path, which the case runs. "Startup refuses" needs a
// product check that opens a profile and refuses a missing capability; no
// backend has one yet, so that half is a reported gap, not a case.

// capabilitySide is where a capability is asserted: on private work (the
// build's store, G on private work) or on a pinned view's reader.
type capabilitySide string

const (
	onStore  capabilitySide = "store"
	onReader capabilitySide = "reader"
)

// profileCapability is one capability's classification for the default
// profile.
type profileCapability struct {
	name string
	side capabilitySide
	has  func(any) bool
	// required capabilities must be present. requiredWith makes one required
	// only on a backend that has the named capability.
	required     bool
	requiredWith string
	// reason is why it is required, or the reference path that makes it
	// optional.
	reason string
}

func capability[T any](v any) bool { _, ok := v.(T); return ok }

// defaultProfile is the suite's classification.
var defaultProfile = []profileCapability{
	{name: "graph.FileMetaReader", side: onReader, has: capability[graph.FileMetaReader], required: true,
		reason: "file receipts; a missing required persistence fails (1.7)"},
	{name: "graph.SymbolSearcher", side: onReader, has: capability[graph.SymbolSearcher], required: true,
		reason: "symbol search; no reference path answers it"},
	{name: "graph.ContentSearcher", side: onReader, has: capability[graph.ContentSearcher], required: true,
		reason: "content search; no reference path answers it"},
	{name: "graph.VectorSearcher", side: onReader, has: capability[graph.VectorSearcher], required: true,
		reason: "vector search; an absent model is a visible unavailability, not a missing capability"},
	{name: "graph.CloneCorpusWriter", side: onStore, has: capability[graph.CloneCorpusWriter],
		reason: "optional: without the persisted corpus, indexer.finaliseCloneCorpusCtx finalises from a node scan"},
	{name: "graph.CloneCorpusPager", side: onStore, has: capability[graph.CloneCorpusPager],
		reason: "optional: the same node-scan finalisation as graph.CloneCorpusWriter"},
	{name: "graph.CloneCorpusRepoReplacer", side: onStore, has: capability[graph.CloneCorpusRepoReplacer],
		requiredWith: "graph.CloneCorpusWriter",
		reason: "required with a persisted clone corpus: ResetCloneCorpus clears stale rows and the initialization " +
			"marker before a full shadow drain, and indexer.resetShadowCloneCorpus skips the reset silently " +
			"without it, so stale rows would survive; there is no reference path"},
	{name: "graph.CloneCorpusInitialization", side: onStore, has: capability[graph.CloneCorpusInitialization],
		reason: "optional: without it indexer.finaliseCloneCorpusCtx keeps the compatibility node scan, " +
			"the same result at the cost of one scan of the repository's nodes"},
	{name: "graph.CallAncestorReader", side: onReader, has: capability[graph.CallAncestorReader],
		reason: "optional: graph.CallAncestorsContext falls back to graph.CallAncestorsOf"},
}

// testK8RequiredCapabilities publishes E0 and checks every capability the
// default profile requires on the build's store and the pinned reader.
func testK8RequiredCapabilities(t *testing.T, backend Backend) {
	ctx, f, inst := newK1Chain(t, backend)
	base := current(t, ctx, inst, CheckoutR1)
	build := sealedBuild(t, ctx, inst, CheckoutR1, f.Inputs(CheckoutR1, E0), "k8-build", f.Payload("r", E0))
	if err := build.Publish(ctx, base, "k8-publish"); err != nil {
		t.Fatalf("publish E0: %v", err)
	}
	view := openView(t, ctx, inst, CheckoutR1)
	if missing := missingRequired(build.Store(), view.Reader()); len(missing) > 0 {
		t.Fatalf("%s lacks capabilities the default profile requires: %v", backend.Name, missing)
	}
}

// missingRequired lists each required capability absent from store or
// reader, with its reason.
func missingRequired(store graph.Store, reader graph.Reader) []string {
	present := map[string]bool{}
	for _, c := range defaultProfile {
		var target any = reader
		if c.side == onStore {
			target = store
		}
		present[c.name] = c.has(target)
	}
	var missing []string
	for _, c := range defaultProfile {
		if (c.required || present[c.requiredWith]) && !present[c.name] {
			missing = append(missing, fmt.Sprintf("%s on the %s (%s)", c.name, c.side, c.reason))
		}
	}
	return missing
}

// testK8OptionalAccelerationMatchesReference runs graph.CallAncestorsContext
// on the pinned reader, which takes its accelerator when it has one, and on
// the same reader with every optional capability hidden, which takes the
// reference path. Both must give F's closure of calls and references.
func testK8OptionalAccelerationMatchesReference(t *testing.T, backend Backend) {
	ctx, f, inst := newK1Chain(t, backend)
	publishVersion(t, ctx, inst, f, CheckoutR1, E0)
	view := openView(t, ctx, inst, CheckoutR1)
	want := f.Payload("r", E0)
	for _, candidate := range []string{"r/app/run.go::Run", "r/api/client.go::Client", "s/api/client.go::Serve"} {
		expected := callAncestorsOfPayload(want, candidate)
		for _, path := range []struct {
			name   string
			reader graph.Reader
		}{{"backend reader", view.Reader()}, {"reference path", referenceReader{view.Reader()}}} {
			got, err := graph.CallAncestorsContext(ctx, path.reader, []string{candidate})
			if err != nil {
				t.Fatalf("%s: CallAncestorsContext(%s): %v", path.name, candidate, err)
			}
			assertMultiset(t, fmt.Sprintf("%s: call ancestors of %s", path.name, candidate), got, expected,
				func(r graph.CallAncestorRow) string { return fmt.Sprintf("%#v", r) })
		}
	}
}

// referenceReader exposes only graph.Reader, so every optional accelerator
// the backend's reader has is hidden.
type referenceReader struct{ graph.Reader }

// callAncestorsOfPayload is the inverse closure of calls and references
// edges from candidate over want, one row per edge, as 1.8's F defines it.
func callAncestorsOfPayload(want Payload, candidate string) []graph.CallAncestorRow {
	nodes := map[string]*graph.Node{}
	for _, n := range want.Nodes {
		nodes[n.ID] = n
	}
	seen := map[string]bool{candidate: true}
	frontier := []string{candidate}
	var out []graph.CallAncestorRow
	for len(frontier) > 0 {
		var next []string
		for _, e := range want.Edges {
			if (e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeReferences) || !slices.Contains(frontier, e.To) {
				continue
			}
			row := graph.CallAncestorRow{From: e.From, To: e.To}
			if n := nodes[e.From]; n != nil {
				row.FromKind, row.FromFile = n.Kind, n.FilePath
			}
			out = append(out, row)
			if !seen[e.From] {
				seen[e.From] = true
				next = append(next, e.From)
			}
		}
		frontier = next
	}
	return out
}

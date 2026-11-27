package store_pebble

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestDefaultOffGroupsAreVisible(t *testing.T) {
	b := newBuild(t, DefaultProfile)
	b.AddNode(&graph.Node{ID: "target", Kind: graph.KindFunction, RepoPrefix: "repo"})
	b.AddNode(&graph.Node{ID: "param", Kind: graph.KindParam, RepoPrefix: "repo"})
	b.AddEdge(&graph.Edge{From: "param", To: "target", Kind: graph.EdgeContains})
	if err := b.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		read func(*Snapshot) error
	}{
		{"nodes", func(s *Snapshot) error { _ = slices.Collect(s.NodesByKind(graph.KindParam)); return s.ReadError() }},
		{"edges", func(s *Snapshot) error { _ = slices.Collect(s.EdgesByKind(graph.EdgeContains)); return s.ReadError() }},
		{"counts", func(s *Snapshot) error {
			_ = s.InEdgeCountsByKind([]graph.EdgeKind{graph.EdgeContains})
			return s.ReadError()
		}},
		{"value-references", func(s *Snapshot) error { _ = slices.Collect(s.ValueRefPlaceholderEdges()); return s.ReadError() }},
		{"existence", func(s *Snapshot) error {
			_, err := s.FindExistingEdgeEndpoints(t.Context(), []graph.TypedEdgeEndpoint{{From: "param", To: "target", Kind: graph.EdgeContains}}, 1)
			return err
		}},
		{"identity", func(s *Snapshot) error {
			_, err := s.FindIncomingEdgeIdentitiesBounded(t.Context(), []string{"target"}, []graph.EdgeKind{graph.EdgeContains}, 1)
			return err
		}},
		{"dead-code", func(s *Snapshot) error {
			_, err := s.DeadCodeCandidatesInReposFiltered([]graph.NodeKind{graph.KindFunction}, nil, []string{"repo"}, graph.OutlineEdgeFilter{})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.read(snapshot(t, b)); !errors.Is(err, ErrGroupOff) {
				t.Fatalf("read error = %v, want ErrGroupOff", err)
			}
		})
	}
	s := snapshot(t, b)
	if got := slices.Collect(s.NodesByKind(graph.KindFunction)); len(got) != 1 || got[0].ID != "target" || s.ReadError() != nil {
		t.Fatalf("kept group read = %v, %v", got, s.ReadError())
	}
}

func TestProfileFTSProjectsOffGroupDocuments(t *testing.T) {
	for _, profile := range []Profile{DefaultProfile, FullProfile} {
		t.Run(string(profile), func(t *testing.T) {
			b := newBuild(t, profile)
			b.AddNode(&graph.Node{ID: "param", Kind: graph.KindParam, Name: "binding", FilePath: "one.go"})
			if err := b.UpsertSymbolFTS("param", "needle"); err != nil {
				t.Fatal(err)
			}
			if err := b.Publish(t.Context()); err != nil {
				t.Fatal(err)
			}
			s := snapshot(t, b)
			hits, err := s.SearchSymbols("needle", 1)
			if profile == DefaultProfile {
				if len(hits) != 0 || err != nil {
					t.Fatalf("default FTS = %v, %v; want no document", hits, err)
				}
				if bundles, err := s.SearchSymbolBundles("needle", 1); len(bundles) != 0 || err != nil {
					t.Fatalf("default bundles = %v, %v; want no document", bundles, err)
				}
			} else if err != nil || len(hits) != 1 || hits[0].NodeID != "param" {
				t.Fatalf("full FTS = %v, %v; want param", hits, err)
			}
			if got, err := s.SearchSymbols("unmatched", 1); len(got) != 0 || err != nil {
				t.Fatalf("unmatched query = %v, %v", got, err)
			}
		})
	}
}

func TestRepoIncomingRequiresVisibleSource(t *testing.T) {
	for _, profile := range []Profile{DefaultProfile, FullProfile} {
		t.Run(string(profile), func(t *testing.T) {
			b := newBuild(t, profile)
			b.AddNode(&graph.Node{ID: "target", Kind: graph.KindFunction, RepoPrefix: "repo"})
			b.AddNode(&graph.Node{ID: "unowned", Kind: graph.KindFunction})
			b.AddNode(&graph.Node{ID: "closure", Kind: graph.KindClosure, RepoPrefix: "repo"})
			for _, from := range []string{"missing", "unowned", "closure"} {
				b.AddEdge(&graph.Edge{From: from, To: "target", Kind: graph.EdgeCalls})
			}
			if err := b.Publish(t.Context()); err != nil {
				t.Fatal(err)
			}
			s := snapshot(t, b)
			out, complete, err := s.GetInEdgesByNodeIDsFromRepos([]string{"target"}, []string{""})
			if got := out["target"]; err != nil || !complete || len(got) != 1 || got[0].From != "unowned" {
				t.Errorf("empty-repo sources = %v, complete = %t, error = %v; want only unowned", got, complete, err)
			}
			out, complete, err = s.GetInEdgesByNodeIDsFromRepos([]string{"target"}, []string{"repo"})
			if profile == DefaultProfile {
				if len(out["target"]) != 0 || !complete || err != nil {
					t.Fatalf("default repo sources = %v, complete = %t, error = %v; want projected empty result", out, complete, err)
				}
			} else if got := out["target"]; err != nil || !complete || len(got) != 1 || got[0].From != "closure" {
				t.Fatalf("full repo sources = %v, complete = %t, error = %v; want closure", got, complete, err)
			}
		})
	}
}

func TestDetachedEdgeProvenanceClearsTier(t *testing.T) {
	b := newBuild(t, FullProfile)
	b.AddEdge(&graph.Edge{From: "from", To: "to", Kind: graph.EdgeCalls, Origin: "old", Tier: "old"})
	e := &graph.Edge{From: "from", To: "to", Kind: graph.EdgeCalls}
	if !b.SetEdgeProvenance(e, "new") || e.Origin != "new" || e.Tier != "" {
		t.Fatalf("detached update = %+v", e)
	}
	if err := b.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, b).GetOutEdges("from"); len(got) != 1 || got[0].Origin != "new" || got[0].Tier != "" {
		t.Fatalf("published update = %+v", got)
	}
}

func TestBulkFTSEmptyAndUnownedScope(t *testing.T) {
	b := newBuild(t, FullProfile)
	for _, n := range []*graph.Node{
		{ID: "unowned", Kind: graph.KindFunction},
		{ID: "owned", Kind: graph.KindFunction, RepoPrefix: "repo"},
	} {
		b.AddNode(n)
		if err := b.UpsertSymbolFTS(n.ID, "original"); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.BulkUpsertSymbolFTS("repo", nil); err != nil {
		t.Fatal(err)
	}
	if err := b.BulkUpsertSymbolFTS("", []graph.SymbolFTSItem{{NodeID: "unowned", Tokens: "replacement"}}); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	s := snapshot(t, b)
	for q, want := range map[string]string{"original": "owned", "replacement": "unowned"} {
		hits, err := s.SearchSymbols(q, 2)
		if err != nil || len(hits) != 1 || hits[0].NodeID != want {
			t.Fatalf("%q: hits = %v, error = %v; want %q", q, hits, err, want)
		}
	}
}

func TestBlockMaxSkipUsesStrictConservativeBound(t *testing.T) {
	const freq, length, avgdl = 3, 3, 3.0
	factor := searchFactor(freq, length, avgdl)
	rounded := float64(float32(factor))
	if rounded >= factor {
		t.Fatal("fixture must round down without upward correction")
	}
	scratch, err := searchDocBounds(searchDoc{Length: length, NodePresent: true, Terms: []searchTerm{{"x", freq}}}, avgdl, func(string) (uint32, error) { return 1, nil })
	if err != nil {
		t.Fatal(err)
	}
	maxima := make(map[uint32]float32)
	present, err := mergeSearchDocBounds(scratch, maxima)
	if err != nil || !present || len(maxima) != 1 {
		t.Fatalf("bounds = %v, node present = %t, error = %v", maxima, present, err)
	}
	block := searchPackBlock([]searchPosting{{Rowid: 1, NodePresent: present}}, maxima)
	bound := float64(math.Nextafter32(float32(factor), float32(math.Inf(1))))
	for _, tc := range []struct {
		threshold float64
		canSkip   bool
		wantSkip  bool
	}{
		{factor, true, false},
		{bound, true, false},
		{math.Nextafter(bound, math.Inf(1)), true, true},
		{math.Nextafter(bound, math.Inf(1)), false, false},
	} {
		visits := 0
		skipped, err := searchReadBlock(block, searchBlockQuery{Phrases: []string{"x"}, PrefixIDs: []uint32{1}, IDF: []float64{1}, Threshold: tc.threshold, CanSkip: tc.canSkip}, func(p searchPosting) error {
			visits++
			if p.Rowid != 1 {
				t.Errorf("posting = %+v", p)
			}
			return nil
		})
		if err != nil || skipped != tc.wantSkip || skipped && visits != 0 || !skipped && visits != 1 {
			t.Fatalf("case %+v: skipped=%t, visits=%d, error=%v", tc, skipped, visits, err)
		}
	}
}

func TestOpenBuildRefusesExistingPendingDirectory(t *testing.T) {
	dir := t.TempDir()
	pending := filepath.Join(dir, "pending")
	if err := os.Mkdir(pending, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(pending, "keep")
	if err := os.WriteFile(marker, []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := OpenBuild(dir, FullProfile); err == nil {
		_ = b.Close()
		t.Error("opened a pre-existing pending directory")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "retained" {
		t.Fatalf("pending marker = %q, %v", data, err)
	}
}

func TestMetadataNativeDepthAndJSONLeaf(t *testing.T) {
	var deep any = "leaf"
	for range 70 {
		deep = []any{deep}
	}
	want := map[string]any{"deep": deep, "maps": []map[string]any{{"key": int64(42)}}}
	encoded, ok := encodeMetaFast(want)
	if !ok || !slices.Contains(encoded, byte(metaTagJSON)) {
		t.Fatal("expected a JSON leaf below the native nesting limit")
	}
	if got, err := decodeMetaFast(encoded); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("metadata round trip differs: %v", err)
	}
	broken := appendMetaString([]byte{metaFlatMagic0, metaFlatVersion, 1}, "nested")
	for range 70 {
		broken = append(broken, metaTagAnySlice, 1)
	}
	broken = append(broken, metaTagNil)
	if _, err := decodeMetaFast(broken); err == nil || !strings.Contains(err.Error(), "nesting") {
		t.Fatalf("malformed native nesting error = %v", err)
	}
	if _, err := decodeMetaFast(encoded[:len(encoded)-1]); err == nil {
		t.Fatal("truncated metadata accepted")
	}
}

func TestExactUnicodeAndEmptyNodeFilters(t *testing.T) {
	b := newBuild(t, FullProfile)
	b.AddNode(&graph.Node{ID: "one", Name: "café", Kind: graph.KindFunction})
	b.AddNode(&graph.Node{ID: "two", Name: "café", Kind: graph.KindFunction, RepoPrefix: "repo", Language: "go"})
	if err := b.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	s := snapshot(t, b)
	if hits, err := s.SearchSymbols("café", 2); err != nil || !slices.Equal(hits, []graph.SymbolHit{{NodeID: "one", Score: 100}, {NodeID: "two", Score: 100}}) {
		t.Errorf("Unicode exact hits = %v, %v", hits, err)
	}
	if _, err := s.SearchSymbols("café au lait", 2); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("non-ASCII FTS error = %v, want unsupported", err)
	}
	if nodes := s.FindNodesByNameInRepo("café", ""); len(nodes) != 1 || nodes[0].ID != "one" {
		t.Errorf("unowned name search = %v, want one", nodes)
	}
	if all, scoped := s.GetNodesByLanguage(""), s.GetRepoNodesByLanguage("", ""); all != nil || scoped != nil {
		t.Errorf("empty language: global = %v, repo = %v; want nil", all, scoped)
	}
}

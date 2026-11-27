package resolver

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
)

// TestSvelteKitLoadPairing proves the +page ↔ +page.server load pairing: a
// SvelteKit route's page component reaches the server `load` function in the
// same route directory through a tier-tagged synthesized edge, so a trace from
// the rendered page lands on its server data source — a cross-file link a
// per-file scanner never makes. A page directory with no server module pairs
// nothing.
func TestSvelteKitLoadPairing(t *testing.T) {
	g := graph.New()

	// Real page component node via the Svelte extractor (route /blog).
	pageRes, err := languages.NewSvelteExtractor().Extract("src/routes/blog/+page.svelte", []byte("<h1>Blog</h1>\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range pageRes.Nodes {
		g.AddNode(n)
	}
	// The server load module for the same route.
	g.AddNode(&graph.Node{
		ID: "src/routes/blog/+page.server.ts", Kind: graph.KindFile,
		Name: "+page.server.ts", FilePath: "src/routes/blog/+page.server.ts",
	})
	g.AddNode(&graph.Node{
		ID: "src/routes/blog/+page.server.ts::load", Kind: graph.KindFunction,
		Name: "load", FilePath: "src/routes/blog/+page.server.ts", StartLine: 1,
	})

	// An unrelated route with only a page — must pair nothing.
	aboutRes, _ := languages.NewSvelteExtractor().Extract("src/routes/about/+page.svelte", []byte("<h1>About</h1>\n"))
	for _, n := range aboutRes.Nodes {
		g.AddNode(n)
	}

	n := ResolveSvelteKitLoad(g)
	if n != 1 {
		t.Fatalf("synthesized %d load pairs, want 1", n)
	}

	var paired *graph.Edge
	for _, e := range g.AllEdges() {
		if e.Meta != nil && e.Meta["via"] == sveltekitLoadVia {
			paired = e
		}
	}
	if paired == nil {
		t.Fatal("no sveltekit_load edge synthesized")
	}
	if paired.From != "src/routes/blog/+page.svelte::+page" {
		t.Errorf("load edge From=%q, want the blog page component", paired.From)
	}
	if paired.To != "src/routes/blog/+page.server.ts::load" {
		t.Errorf("load edge To=%q, want the server load function", paired.To)
	}
	if paired.Meta[MetaSynthesizedBy] != SynthSvelteKitLoad {
		t.Errorf("synthesized_by=%v, want %q", paired.Meta[MetaSynthesizedBy], SynthSvelteKitLoad)
	}
	if paired.Origin != graph.OriginASTInferred {
		t.Errorf("origin=%v, want OriginASTInferred (tier-tagged)", paired.Origin)
	}
}

func TestSvelteKitLoadDoesNotFallBackFromElectedLoad(t *testing.T) {
	const route = "src/routes/blog/"
	page := &graph.Node{ID: route + "+page.svelte::+page", Kind: graph.KindType, FilePath: route + "+page.svelte",
		WorkspaceID: "blog", Meta: map[string]any{"component": true}}
	server := &graph.Node{ID: route + "+layout.server.js", Kind: graph.KindFile, FilePath: route + "+layout.server.js", WorkspaceID: "blog"}
	load := &graph.Node{ID: server.ID + "::load", Kind: graph.KindFunction, Name: "load", FilePath: server.FilePath, WorkspaceID: "other"}
	g := graph.New()
	g.AddNode(page)
	g.AddNode(server)
	g.AddNode(load)
	if got := ResolveSvelteKitLoad(g); got != 0 {
		t.Fatalf("mismatched explicit load produced %d pairs, want 0", got)
	}
	g = graph.New()
	g.AddNode(page)
	g.AddNode(server)
	if got := ResolveSvelteKitLoad(g); got != 1 {
		t.Fatalf("server file with no explicit load produced %d pairs, want 1", got)
	}
	if edges := g.AllEdges(); len(edges) != 1 || edges[0].To != server.ID {
		t.Fatalf("unexpected file fallback: %v", edges)
	}
}

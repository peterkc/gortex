package resolver

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

type rebuildMemberTierWant struct {
	to, receiver, origin string
	confidence           float64
	dispatch, resolution string
}

// Exercise the actual extraction shapes behind the promoted Go method and
// typed interface calls. Interface locality must change even a preexisting
// origin, while retaining confidence and the other extraction payload.
func TestRebuildMainMemberExtractedGoTiers(t *testing.T) {
	const file = "repo/client/client.go"
	facts, err := languages.NewGoExtractor().Extract(file, []byte(`package client
type Reader interface { ReadName() string }
type Alpha struct{}
type Beta struct{}
func (Alpha) ReadName() string { return "alpha" }
func (Beta) ReadName() string { return "beta" }
func Dispatch(r Reader) string { return r.ReadName() }

type Base struct{}
func (Base) Promoted() string { return "base" }
type Outer struct { Base }
func (o Outer) Forward() string { return o.Promoted() }
`))
	require.NoError(t, err)
	t.Cleanup(facts.ReleaseTree)
	want := map[string]rebuildMemberTierWant{
		file + "::Dispatch":      {to: file + "::Alpha.ReadName", receiver: "Reader", origin: graph.OriginASTInferred, confidence: 0.42, dispatch: "interface"},
		file + "::Outer.Forward": {to: file + "::Base.Promoted", receiver: "Outer", origin: graph.OriginASTInferred, confidence: 0.7},
	}
	var edges []*graph.Edge
	for _, n := range facts.Nodes {
		n.RepoPrefix = "repo"
	}
	for _, e := range facts.Edges {
		if e.Kind != graph.EdgeCalls || !strings.HasPrefix(e.To, "unresolved::*.") {
			continue
		}
		if e.From == file+"::Dispatch" {
			// Deliberately exercise the interface annotation's overwrite and
			// confidence preservation, not merely filling empty provenance.
			e.Origin, e.Confidence = graph.OriginASTResolved, 0.42
		}
		setEdgeTerminal(e, terminalReasonNoDefinition)
		edges = append(edges, e)
	}
	require.Len(t, edges, 2)
	checkRebuildMainMemberTiers(t, facts.Nodes, edges, want)
}

// Named imports, an injected field, an untyped this-call and a static module
// factory are actual TS extraction shapes. The first raw receiver name has
// two exact matches; only the later-sorting one is directly imported. An
// import of the other match by that imported module is NOT a direct import.
func TestRebuildMainMemberExtractedWebTiers(t *testing.T) {
	const (
		caller   = "repo/web/client/auth.ts"
		factory  = "repo/web/client/app.ts"
		near     = "repo/web/zzusers/index.ts"
		far      = "repo/web/aausers/index.ts"
		jsCaller = "repo/web/jsclient/client.js"
		jsNear   = "repo/web/jszz/index.js"
		jsFar    = "repo/web/jsaa/index.js"
	)
	files := []struct{ path, source string }{
		{far, `export class UsersService {
  findOne() { return "far"; }
}
`},
		{near, `import "../aausers";
export class UsersService {
  findOne() { return "near"; }
}
export class CacheModule {
  static forRoot() { return 60; }
}
`},
		{caller, `import { UsersService } from "../zzusers";
export class Auth {
  private users = inject(UsersService);
  choose() { return this.users.findOne(); }
  render() { return this.displayList(); }
  displayList() { return "local"; }
}
`},
		{factory, `import { CacheModule } from "../zzusers";
export const cache = CacheModule.forRoot();
`},
		{jsFar, `export class Far {
  drawPicture() { return "far"; }
}
`},
		{jsNear, `export class Near {
  drawPicture() { return "near"; }
}
`},
		{jsCaller, `import "../jszz";
export function Draw(view) {
  return view.drawPicture();
}
`},
	}
	want := map[string]rebuildMemberTierWant{
		caller + "::Auth.choose": {to: near + "::UsersService.findOne", receiver: "UsersService", origin: graph.OriginTextMatched},
		caller + "::Auth.render": {to: caller + "::Auth.displayList", confidence: 0.9},
		factory:                  {to: near + "::CacheModule.forRoot", origin: graph.OriginTextMatched},
		jsCaller + "::Draw":      {to: jsNear + "::Near.drawPicture", origin: graph.OriginTextMatched},
	}
	var nodes []*graph.Node
	var edges []*graph.Edge
	var rawCalls int
	for _, f := range files {
		var facts *parser.ExtractionResult
		var err error
		if strings.HasSuffix(f.path, ".js") {
			facts, err = languages.NewJavaScriptExtractor().Extract(f.path, []byte(f.source))
		} else {
			facts, err = languages.NewTypeScriptExtractor().Extract(f.path, []byte(f.source))
		}
		require.NoError(t, err)
		t.Cleanup(facts.ReleaseTree)
		for _, n := range facts.Nodes {
			n.RepoPrefix = "repo"
			nodes = append(nodes, n)
		}
		for _, e := range facts.Edges {
			switch {
			case e.Kind == graph.EdgeImports:
				edges = append(edges, e)
			case e.Kind == graph.EdgeCalls && strings.HasPrefix(e.To, "unresolved::*."):
				setEdgeTerminal(e, terminalReasonNoDefinition)
				edges = append(edges, e)
				rawCalls++
			}
		}
	}
	require.Equal(t, len(want), rawCalls)
	checkRebuildMainMemberTiers(t, nodes, edges, want)
}

// Source-shaped rows isolate the ordered tiers. Caller receiver selection
// must prefer the local match over an imported earlier match, then choose
// the first matching receiver anywhere in the retained candidate group.
// Exact receiver selection must still use the raw group. A reachability
// filter miss restores all candidates; an untyped multi-method miss does not
// guess. Include a deferred-language DI provider to prove the provider census
// is not narrowed by the caller or candidate language.
func TestRebuildMainMemberOrderedTiers(t *testing.T) {
	const (
		client = "repo/client/client.go"
		web    = "repo/web/client.ts"
		other  = "repo/aa/other.go"
		local  = "repo/client/local.go"
		remote = "repo/zz/remote.go"
		aaJS   = "repo/aaweb/index.ts"
		zzJS   = "repo/zzweb/index.ts"
	)
	node := func(id, name, file, lang, receiver string, kind graph.NodeKind) *graph.Node {
		return &graph.Node{ID: id, Name: name, FilePath: file, Language: lang,
			RepoPrefix: "repo", Kind: kind, Meta: map[string]any{"receiver": receiver}}
	}
	const method, fn = graph.KindMethod, graph.KindFunction
	nodes := []*graph.Node{
		node(client, client, client, "go", "", graph.KindFile),
		node(other, other, other, "go", "", graph.KindFile),
		node(remote, remote, remote, "go", "", graph.KindFile),
		node(web, web, web, "typescript", "", graph.KindFile),
		node(zzJS, zzJS, zzJS, "typescript", "", graph.KindFile),
		node("goCallerLocal", "ForwardLocal", client, "go", "Owner", method),
		node("goCallerAny", "ForwardAny", client, "go", "Owner", method),
		node("goDI", "Bound", client, "go", "", fn),
		node("a-go-unreachable-di", "Emit", "repo/beyond/di.go", "go", "Provider", method),
		node("repo/php/module.php::Module", "Module", "repo/php/module.php", "php", "", graph.KindType),
		node("goDIDistractor", "Emit", local, "go", "Other", method),
		node("goDIWinner", "Emit", remote, "go", "Provider", method),
		node("a-local-tier", "LocalPick", other, "go", "Owner", method),
		node("z-local-tier", "LocalPick", local, "go", "Owner", method),
		node("a-any-unreachable", "AnyPick", "repo/beyond/owner.go", "go", "Owner", method),
		node("a-any-distractor", "AnyPick", local, "go", "Other", method),
		node("b-any-winner", "AnyPick", remote, "go", "Owner", method),
		node("z-any-winner", "AnyPick", other, "go", "Owner", method),
		node("tsCallerExact", "Exact", web, "typescript", "", fn),
		node("tsCallerLocalExact", "LocalExact", web, "typescript", "", fn),
		node("a-remote-exact", "ExactLocal", aaJS, "typescript", "Hint", method),
		node("z-local-exact", "ExactLocal", "repo/web/local.ts", "typescript", "Hint", method),
		node("a-raw-exact", "ExactPick", aaJS, "typescript", "Hint", method),
		node("z-reachable-other", "ExactPick", zzJS, "typescript", "Other", method),
		node("tsCallerNoReach", "Empty", "repo/unrelated/empty.ts", "typescript", "", fn),
		node("tsCallerAmbiguous", "Ambiguous", "repo/unrelated/empty.ts", "typescript", "", fn),
		node("a-empty-tier", "EmptyPick", aaJS, "typescript", "Other", method),
		node("z-empty-tier", "EmptyPick", zzJS, "typescript", "Other", method),
	}
	cases := []struct {
		from, file, name, receiver, origin string
		want                               rebuildMemberTierWant
	}{
		{"goCallerLocal", client, "LocalPick", "", graph.OriginTextMatched, rebuildMemberTierWant{to: "z-local-tier", origin: graph.OriginTextMatched, confidence: 0.9}},
		{"goCallerAny", client, "AnyPick", "", graph.OriginASTInferred, rebuildMemberTierWant{to: "b-any-winner", origin: graph.OriginASTInferred, confidence: 0.8}},
		{"goDI", client, "Emit", "Abstract", graph.OriginTextMatched, rebuildMemberTierWant{to: "goDIWinner", receiver: "Abstract", origin: graph.OriginTextMatched, confidence: 0.9, resolution: "useClass_binding"}},
		{"tsCallerExact", web, "ExactPick", "Hint", graph.OriginTextMatched, rebuildMemberTierWant{to: "a-raw-exact", receiver: "Hint", origin: graph.OriginASTResolved, confidence: 0.85}},
		{"tsCallerLocalExact", web, "ExactLocal", "Hint", graph.OriginTextMatched, rebuildMemberTierWant{to: "z-local-exact", receiver: "Hint", origin: graph.OriginASTResolved, confidence: 0.95}},
		{"tsCallerNoReach", "repo/unrelated/empty.ts", "EmptyPick", "Hint", graph.OriginASTResolved, rebuildMemberTierWant{to: "a-empty-tier", receiver: "Hint", origin: graph.OriginASTResolved, confidence: 0.42}},
		{"tsCallerAmbiguous", "repo/unrelated/empty.ts", "EmptyPick", "", graph.OriginASTResolved, rebuildMemberTierWant{to: "unresolved::*.EmptyPick", origin: graph.OriginASTResolved, confidence: 0.42}},
	}
	want := make(map[string]rebuildMemberTierWant, len(cases))
	edges := []*graph.Edge{
		{From: client, To: other, Kind: graph.EdgeImports, FilePath: client, Line: 1},
		{From: client, To: remote, Kind: graph.EdgeImports, FilePath: client, Line: 2},
		{From: web, To: zzJS, Kind: graph.EdgeImports, FilePath: web, Line: 1},
		{From: "repo/php/module.php::Module", To: "unresolved::Provider", Kind: graph.EdgeProvides,
			FilePath: "repo/php/module.php", Line: 10, Meta: map[string]any{graph.MetaDIProvidesFor: "Abstract", graph.MetaDIBinding: graph.DIBindingUseClass}},
	}
	for i, tc := range cases {
		e := &graph.Edge{From: tc.from, To: "unresolved::*." + tc.name, Kind: graph.EdgeCalls,
			FilePath: tc.file, Line: i + 10, Origin: tc.origin, Confidence: 0.42,
			CrossRepo: true, Meta: map[string]any{"receiver_type": tc.receiver, "payload": map[string]any{"kept": true}}}
		setEdgeTerminal(e, terminalReasonNoDefinition)
		edges = append(edges, e)
		want[tc.from] = tc.want
	}
	checkRebuildMainMemberTiers(t, nodes, edges, want)
}

// Compare the main page, not the later weak-cross-package guard or tails.
// The native graph sees the same raw input and retains the unselected rows.
func checkRebuildMainMemberTiers(t *testing.T, nodes []*graph.Node, edges []*graph.Edge, want map[string]rebuildMemberTierWant) {
	t.Helper()
	var pending, selected []*graph.Edge
	for _, e := range edges {
		if graph.IsUnresolvedTarget(e.To) {
			pending = append(pending, e)
		}
		if e.Kind == graph.EdgeCalls {
			tc, ok := want[e.From]
			require.True(t, ok, "unexpected call from %s", e.From)
			require.True(t, graph.IsUnresolvedTarget(e.To))
			require.Equal(t, tc.receiver, edgeReceiverType(e), e.From)
			selected = append(selected, e)
		}
	}
	require.Len(t, selected, len(want))
	rebuildFnPointerSortRows(pending)
	rebuildFnPointerSortRows(selected)

	g := graph.New()
	for _, n := range nodes {
		copy := *n
		copy.Meta = cloneFrameworkMeta(n.Meta)
		g.AddNode(&copy)
	}
	for _, e := range edges {
		g.AddEdge(cloneFrameworkEdge(e))
	}
	native := New(g)
	native.bulkMode = true
	require.NoError(t, native.warmLookupCache(selected))
	defer native.clearLookupCache()
	require.True(t, native.buildReachabilityIndexForPending(selected, native.nodeByID))
	defer native.clearReachabilityIndex()
	native.buildProvidesForIndex()
	defer native.clearProvidesForIndex()
	var nativeMoves []graph.EdgeReindex
	seen := make(map[string]bool, len(want))
	for _, original := range selected {
		require.False(t, seen[original.From], "duplicate caller %s", original.From)
		seen[original.From] = true
		e := cloneFrameworkEdge(original)
		oldTo, changed := native.resolveEdge(e, &ResolveStats{})
		tc := want[e.From]
		require.Equal(t, tc.to != original.To, changed, e.From)
		if changed {
			clearEdgeTerminal(e)
			nativeMoves = append(nativeMoves, graph.EdgeReindex{Edge: e, OldTo: oldTo, OldKind: original.Kind})
		}
		expected := cloneFrameworkEdge(original)
		expected.To, expected.Origin, expected.Confidence = tc.to, tc.origin, tc.confidence
		if tc.dispatch != "" {
			expected.Meta["dispatch"] = tc.dispatch
		}
		if tc.resolution != "" {
			expected.Meta["resolution"] = tc.resolution
		}
		if changed {
			clearEdgeTerminal(expected)
		}
		require.Equal(t, expected, e, original.From)
	}
	require.NotEmpty(t, nativeMoves)
	g.ReindexEdges(nativeMoves)
	nativeRows := g.AllEdges()
	rebuildFnPointerSortRows(nativeRows)
	require.Len(t, nativeRows, len(edges))

	ctx, build, opts := frontierFixture(t, 0)
	opts.SpillBytes = 1
	build.AddBatch(nodes, edges)
	require.ElementsMatch(t, edges, rebuildFnPointerSaved(t, build))
	require.NoError(t, runRebuildLinkFrontier(ctx, build, opts, func(page []*graph.Edge) ([]*graph.Edge, error) {
		require.Equal(t, pending, page)
		var chosen []*graph.Edge
		for _, e := range page {
			if _, ok := want[e.From]; ok && e.Kind == graph.EdgeCalls {
				chosen = append(chosen, e)
			}
		}
		require.Equal(t, selected, chosen)
		return chosen, nil
	}, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
		if err := prepareRebuildProvides(ctx, f, page); err != nil {
			return err
		}
		moves, err := reduceRebuildMainPage(ctx, f, page)
		if err != nil {
			return err
		}
		require.Equal(t, nativeMoves, moves)
		require.Equal(t, selected, page, "tentative reduction must not mutate the borrowed page")
		return build.ApplyLinkReindexes(ctx, moves)
	}))
	require.Equal(t, nativeRows, rebuildFnPointerSaved(t, build))
	checkFrontierScratch(t, opts.ScratchParent)
}

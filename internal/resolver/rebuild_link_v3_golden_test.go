package resolver

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

type frontierGoldenCase struct {
	Name string
	Data json.RawMessage
}

type frontierGoldenPage struct {
	Rows       int
	Calls      uint64
	Moves      []graph.EdgeReindex
	Requests   []store_pebble.LinkRequest `json:",omitempty"`
	Candidates []frontierGoldenCandidate  `json:",omitempty"`
}
type frontierGoldenCandidate struct {
	Position int
	Node     *graph.Node
}

type frontierGoldenResult struct {
	Pages []frontierGoldenPage
	Saved []*graph.Edge
	Error string
}

// This helper captures only the H98 arm when capture is true. For focused CI
// it executes the same fixture on both concrete readers and compares the
// complete ordered serialized observations (not just counts).
func compareFrontierGolden(t *testing.T, capture bool, name string, run func(*testing.T, bool) any) frontierGoldenCase {
	t.Helper()
	old, err := json.Marshal(run(t, true))
	require.NoError(t, err)
	if !capture {
		product, err := json.Marshal(run(t, false))
		require.NoError(t, err)
		if !bytes.Equal(old, product) {
			t.Fatalf("%s differs: H98 sha=%x product sha=%x", name, sha256.Sum256(old), sha256.Sum256(product))
		}
	}
	return frontierGoldenCase{Name: name, Data: old}
}

func frontierGoldenMixed(t *testing.T, legacy bool) any {
	ctx, b, opts := frontierFixture(t, 1, 0)
	opts.LegacyCandidates = legacy
	const file = "src/use.go"
	node := func(id, name, lang, repo, location string, kind graph.NodeKind) *graph.Node {
		return &graph.Node{ID: id, Name: name, Language: lang, RepoPrefix: repo, FilePath: location, Kind: kind}
	}
	edge := func(from, target string, kind graph.EdgeKind, line int) *graph.Edge {
		return &graph.Edge{From: from, To: "unresolved::" + target, Kind: kind, FilePath: file, Line: line, Origin: graph.OriginASTInferred, Confidence: .42, Meta: map[string]any{"payload": "kept"}}
	}

	caller := node("a", "caller", "go", "repo", file, graph.KindFunction)
	later := node("z", "later", "go", "repo", file, graph.KindFunction)
	memberA := node("aa-method", "Work", "go", "repo", file, graph.KindMethod)
	memberA.Meta = map[string]any{"receiver": "A"}
	memberB := node("zz-method", "Work", "go", "repo", file, graph.KindMethod)
	memberB.Meta = map[string]any{"receiver": "B"}
	iface := node("iface", "Reader", "go", "repo", file, graph.KindInterface)
	imported := node("pkg", "Package", "rust", "repo", "pkg.rs", graph.KindPackage)
	imported.QualName = "example.test/pkg"
	nodes := []*graph.Node{caller, later, memberA, memberB, iface, imported, node("a-fn", "First", "go", "repo", file, graph.KindFunction), node("b-fn", "Second", "go", "repo", file, graph.KindFunction), node("wrong", "First", "python", "repo", file, graph.KindFunction), node("old-live", "Live", "go", "repo", "pkg/old.go", graph.KindFunction)}
	var input []*graph.Edge
	for i, target := range []string{"First", "First", "Second", "Missing", "*.Work", "*.Work", "extern::pkg::Live", "extern::pkg::Live", "import::example.test/pkg", "import::example.test/pkg", "ctx.Done()"} {
		kind := graph.EdgeCalls
		if strings.HasPrefix(target, "import::") {
			kind = graph.EdgeImports
		}
		if target == "ctx.Done()" {
			kind = graph.EdgeRecvs
		}
		e := edge("a", target, kind, i+1)
		if target == "*.Work" {
			e.Meta["receiver_type"] = "Reader"
		}
		setEdgeTerminal(e, terminalReasonNoDefinition)
		input = append(input, e)
	}
	for i := len(input); i < resolvePendingPageRows; i++ {
		input = append(input, edge("middle", "grpc::Ignored::Run", graph.EdgeCalls, i+1))
	}
	input = append(input, edge("z", "extern::pkg::Live", graph.EdgeCalls, resolvePendingPageRows+1), edge("z", "extern::pkg::Live", graph.EdgeCalls, resolvePendingPageRows+2))
	b.AddBatch(nodes, input)

	var out frontierGoldenResult
	err := runRebuildLinkFrontier(ctx, b, opts, nil, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
		f.imports.checkGoExtern = func(string) error { return nil }
		f.imports.goOwnership = func(GoImportCandidate) (GoPackageOwnershipResult, error) { return GoPackageOwnershipExact, nil }
		f.providesFor = map[string]map[string]struct{}{}
		if len(out.Pages) == 1 {
			b.AddNode(node("old-live", "Other", "go", "repo", "pkg/old.go", graph.KindFunction))
			b.AddNode(node("new-live", "Live", "go", "repo", "pkg/new.go", graph.KindFunction))
		}
		moves, err := reduceRebuildMainPage(ctx, f, page)
		if err != nil {
			return err
		}
		out.Pages = append(out.Pages, frontierGoldenPage{Rows: len(page), Calls: f.link.RequestProgress().CandidateCalls, Moves: moves})
		return b.ApplyLinkReindexes(ctx, moves)
	})
	require.NoError(t, err)
	require.Len(t, out.Pages, 2)
	require.Len(t, out.Pages[0].Moves, 9)
	require.Len(t, out.Pages[1].Moves, 2)
	for _, move := range out.Pages[1].Moves {
		require.Equal(t, "new-live", move.Edge.To)
	}
	out.Saved = rebuildFnPointerSaved(t, b)
	checkFrontierScratch(t, opts.ScratchParent)
	return out
}

func frontierGoldenTypes(t *testing.T, legacy bool) any {
	ctx, b, opts := frontierFixture(t, 1, 0)
	opts.LegacyCandidates = legacy
	const goFile, phpFile, csFile = "src/use.go", "src/use.php", "src/use.cs"
	const vueFile, fallbackFile, panel = "ui/View.vue", "ui/Fallback.vue", "ui/Panel.vue"
	nodes := []*graph.Node{
		typeTestNode("go", "Caller", graph.KindFunction, "go", goFile),
		typeTestNode("php", "Caller", graph.KindFunction, "php", phpFile),
		typeTestNode("cs", "Caller", graph.KindFunction, "csharp", csFile),
		typeTestNode("go-A", "A", graph.KindType, "go", "defs/a.go"),
		typeTestNode("wrong-A", "A", graph.KindType, "rust", goFile),
		typeTestNode("go-B", "B", graph.KindFunction, "go", goFile),
		typeTestNode("web-B", "B", graph.KindType, "typescript", "ui/b.ts"),
		testNamespacedType("php-User", "User", "php", "Model"),
		testNamespacedType("cs-other", "User", "csharp", "Other"),
		testNamespacedType("cs-model", "User", "csharp", "Model"),
		typeTestNode(vueFile+"::Source", "Source", graph.KindFunction, "vue", vueFile),
		typeTestNode(fallbackFile+"::Source", "Source", graph.KindFunction, "vue", fallbackFile),
		typeTestNode(panel, "Panel.vue", graph.KindFile, "vue", panel),
		typeTestNode(panel+"::A", "A", graph.KindFunction, "vue", panel),
	}
	// A and B route to different worker slots. Duplicate A positions
	// remain distinct; a type branch never falls back to a function.
	edges := []*graph.Edge{
		typeTestEdge("go", goFile, "*.A", graph.EdgeExtends, ""),
		typeTestEdge("go", goFile, "A", graph.EdgeReferences, ""),
		typeTestEdge("go", goFile, "B", graph.EdgeInstantiates, ""),
		typeTestEdge("go", goFile, "B", graph.EdgeReturns, ""),
		typeTestEdge("go", goFile, "Missing", graph.EdgeTypedAs, ""),
		typeTestEdge("php", phpFile, "Nickname", graph.EdgeTypedAs, `Model\User`),
		typeTestEdge("cs", csFile, "User", graph.EdgeReferences, ""),
		typeTestEdge("cs", csFile, "User", graph.EdgeImplements, "Model.User"),
		rebuildVueTestRef(vueFile, "A", "A", 9),
		rebuildVueTestRef(fallbackFile, "B", "B", 10),
		typeTestEdge("go", goFile, "grpc::Skip", graph.EdgeReferences, ""),
		typeTestEdge("go", goFile, "pyrel::Skip", graph.EdgeTypedAs, ""),
		typeTestEdge("go", goFile, "A", graph.EdgeTests, ""),
	}
	for i, e := range edges {
		e.Line = i + 1
	}
	b.AddBatch(nodes, append(edges, rebuildVueTestImport(vueFile, panel, "A", 1)))

	before := rebuildFnPointerSaved(t, b)
	type typeOutcome struct {
		Old     graph.EdgeIdentity
		Row     graph.Edge
		Request store_pebble.LinkRequest
		Refusal string
	}
	var typeRows []typeOutcome
	var refusals []string
	var out frontierGoldenResult
	require.NoError(t, runRebuildLinkFrontier(ctx, b, opts, nil, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
		queries, err := resolveRebuildTypePage(ctx, f, page)
		if err != nil {
			return err
		}
		for i, e := range page {
			if queries[i].request.Key == "" {
				continue
			}
			copy := *e
			var refusal string
			if err := queries[i].finish(&copy); err != nil {
				refusal = err.Error()
				refusals = append(refusals, refusal)
			}
			typeRows = append(typeRows, typeOutcome{graph.EdgeIdentityFor(e), copy, queries[i].request, refusal})
		}
		require.EqualValues(t, 1, f.link.RequestProgress().CandidateCalls)
		return nil
	}))
	require.Len(t, typeRows, 8)
	require.Len(t, refusals, 2)
	err := runRebuildLinkFrontier(ctx, b, opts, nil, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
		moves, err := reduceRebuildMainPage(ctx, f, page)
		require.Empty(t, moves)
		require.Error(t, err)
		require.Equal(t, refusals[0], err.Error())
		return err
	})
	require.ErrorIs(t, err, store_pebble.ErrRebuildLink)
	out.Error = refusals[0]
	require.Equal(t, before, rebuildFnPointerSaved(t, b))
	checkFrontierScratch(t, opts.ScratchParent)
	require.NoError(t, runRebuildLinkFrontier(ctx, b, opts, func(page []*graph.Edge) ([]*graph.Edge, error) {
		var kept []*graph.Edge
		for _, e := range page {
			if e.Line != 6 && e.Line != 7 {
				kept = append(kept, e)
			}
		}
		return kept, nil
	}, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
		moves, err := reduceRebuildMainPage(ctx, f, page)
		if err != nil {
			return err
		}
		require.Len(t, moves, 6)
		calls := f.link.RequestProgress().CandidateCalls
		require.EqualValues(t, 2, calls, "conditional Vue query")
		out.Pages = append(out.Pages, frontierGoldenPage{Rows: len(page), Calls: calls, Moves: moves})
		return b.ApplyLinkReindexes(ctx, moves)
	}))
	out.Saved = rebuildFnPointerSaved(t, b)
	checkFrontierScratch(t, opts.ScratchParent)
	return struct {
		Rows   []typeOutcome
		Result frontierGoldenResult
	}{typeRows, out}
}

func frontierGoldenAggregate(t *testing.T, legacy bool, kind string) any {
	ctx, b, opts := frontierFixture(t, 1, 0)
	opts.LegacyCandidates = legacy
	var key string
	count, wantCalls := 2, 2
	if kind == "result" {
		key = "Huge"
		count, wantCalls = 26, 9
		b.AddNode(&graph.Node{ID: "huge", Name: key, Kind: graph.KindFunction, Language: "go", FilePath: "a.go", Meta: map[string]any{"payload": strings.Repeat("x", 5<<20)}})
	} else {
		key = strings.Repeat("a", 1100<<10)
	}
	b.AddNode(&graph.Node{ID: "caller", Name: "caller", Language: "go", Kind: graph.KindFunction, FilePath: "a.go"})
	for i := range count {
		kindEdge := graph.EdgeCalls
		target := key
		if kind == "type-wire" {
			kindEdge = graph.EdgeReferences
			if i == 1 {
				target = strings.Repeat("z", 1100<<10)
			}
			e := typeTestEdge("caller", "a.go", target, kindEdge, "")
			e.Line = i + 1
			b.AddEdge(e)
			continue
		}
		b.AddEdge(&graph.Edge{From: "caller", To: "unresolved::" + target, Kind: kindEdge, FilePath: "a.go", Line: i + 1})
	}
	before := rebuildFnPointerSaved(t, b)
	var out frontierGoldenResult
	err := runRebuildLinkFrontier(ctx, b, opts, nil, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
		moves, err := reduceRebuildMainPage(ctx, f, page)
		calls := f.link.RequestProgress().CandidateCalls
		out.Pages = append(out.Pages, frontierGoldenPage{Rows: len(page), Calls: calls, Moves: moves})
		if kind == "type-wire" {
			require.ErrorContains(t, err, "wire byte bound")
			require.Empty(t, moves)
			require.EqualValues(t, 1, calls)
			out.Error = "wire byte bound"
			return err
		}
		if err != nil {
			return err
		}
		require.EqualValues(t, wantCalls, calls)
		if kind == "result" {
			require.Len(t, moves, count)
		} else {
			require.Empty(t, moves)
		}
		return b.ApplyLinkReindexes(ctx, moves)
	})
	if kind == "type-wire" {
		require.ErrorIs(t, err, store_pebble.ErrRebuildLink)
		require.Equal(t, before, rebuildFnPointerSaved(t, b))
	} else {
		require.NoError(t, err)
	}
	// Large wire inputs are deterministically generated by the fixture;
	// record the complete saved rows, compressed on disk by the capture test.
	out.Saved = rebuildFnPointerSaved(t, b)
	checkFrontierScratch(t, opts.ScratchParent)
	return out
}

// T1's actual reader trace fixture (not its disposable timing/checkpoint
// report). Original page boundaries, checked misses and duplicate payloads
// remain observable without a permanent trace hook in production.
func frontierGoldenTrace(t *testing.T, legacy bool) any {
	ctx, b, opts := frontierFixture(t, 1, resolvePendingPageRows+1)
	opts.LegacyCandidates = legacy
	b.AddNode(&graph.Node{ID: "z", Name: "Run", RepoPrefix: "repo"})
	requests := []store_pebble.LinkRequest{{Kind: store_pebble.LinkName, Key: "Run", RepoPrefix: "repo"}, {Kind: store_pebble.LinkName, Key: "Missing", RepoPrefix: "repo"}, {Kind: store_pebble.LinkName, Key: "Run", AllRepos: true}}
	var pages []frontierGoldenPage
	require.NoError(t, runRebuildLinkFrontier(ctx, b, opts, nil, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
		p := frontierGoldenPage{Rows: len(page), Requests: requests}
		var ids []string
		_, err := f.candidates(ctx, requests, func(pos int, n *graph.Node) error {
			require.NotEqual(t, 1, pos)
			ids = append(ids, n.ID)
			// The fixture owns output copies; cache pointers remain read-only.
			copy := *n
			p.Candidates = append(p.Candidates, frontierGoldenCandidate{Position: pos, Node: &copy})
			return nil
		})
		require.Equal(t, []string{"initial", "z", "initial", "z"}, ids)
		p.Calls = f.link.RequestProgress().CandidateCalls
		pages = append(pages, p)
		return err
	}))
	require.Len(t, pages, 2)
	checkFrontierScratch(t, opts.ScratchParent)
	return pages
}

func frontierGoldenCaller(t *testing.T, legacy bool) any {
	ctx, b, _ := frontierFixture(t, 1, 0)
	const file = "repo/caller.go"
	source := &graph.Node{ID: "source", FilePath: file, Meta: map[string]any{"raw": []int{1, 2}}}
	target := &graph.Node{ID: "target", FilePath: "repo/old/target.go", Meta: map[string]any{"raw": []int64{3, 4}}}
	b.AddBatch([]*graph.Node{source, target}, []*graph.Edge{{From: source.ID, To: target.ID, Kind: graph.EdgeImports}, {From: source.ID, To: "missing", Kind: graph.EdgeImports}})
	view := b.RebuildView()
	if !legacy {
		view = view.WithCallerPageCache()
	}
	type importRow struct{ ID, Path string }
	type row struct {
		Read    string
		Node    *graph.Node
		Imports []importRow
	}
	var rows []row
	node := func(id string) *graph.Node {
		n, err := view.Node(ctx, id)
		require.NoError(t, err)
		// Record owned JSON-compatible copies, never mutate borrowed nodes.
		if n == nil {
			rows = append(rows, row{Read: id})
			return nil
		}
		copy := *n
		rows = append(rows, row{Read: id, Node: &copy})
		return n
	}
	imports := func() map[string]string {
		var ordered []importRow
		got := map[string]string{}
		require.NoError(t, view.VisitFileImportTargets(ctx, file, func(id, path string) error {
			ordered = append(ordered, importRow{id, path})
			got[id] = path
			return nil
		}))
		rows = append(rows, row{Read: "imports", Imports: ordered})
		return got
	}
	a := node(source.ID)
	again := node(source.ID)
	require.Equal(t, source, a)
	require.Equal(t, source, again)
	if !legacy {
		require.Same(t, a, again)
	}
	require.Nil(t, node("missing"))
	require.Nil(t, node("missing"))
	want := map[string]string{target.ID: target.FilePath, "missing": ""}
	require.Equal(t, want, imports())
	require.Equal(t, want, imports())
	require.Equal(t, target, node(target.ID))
	require.NoError(t, view.ResetCallerPage())
	target = &graph.Node{ID: target.ID, FilePath: "repo/new/target.go", Meta: map[string]any{"fresh": true}}
	missing := &graph.Node{ID: "missing", FilePath: "repo/new"}
	b.AddBatch([]*graph.Node{target, missing, {ID: "new", FilePath: "repo/new/new.go"}}, []*graph.Edge{{From: source.ID, To: "new", Kind: graph.EdgeImports}})
	require.Equal(t, target, node(target.ID))
	require.Equal(t, missing, node("missing"))
	require.Equal(t, map[string]string{target.ID: target.FilePath, "missing": missing.FilePath, "new": "repo/new/new.go"}, imports())
	require.NoError(t, view.ResetCallerPage())
	return rows
}

// Genuine failures must never apply the partially reduced page or admit a
// snapshot offer. The old worker's transport-specific cases are historical;
// file integrity of the new reader is checked at its real file boundary.
func frontierGoldenFailure(t *testing.T, legacy bool, mode string) any {
	ctx, b, opts := frontierFixture(t, 1, 0)
	opts.LegacyCandidates = legacy
	b.AddBatch([]*graph.Node{{ID: "caller", Name: "caller", Language: "go", Kind: graph.KindFunction, FilePath: "a.go"}, {ID: "fn", Name: "A", Kind: graph.KindFunction, Language: "go", FilePath: "a.go"}, {ID: "qualified", Name: "qualified", QualName: "pkg", Kind: graph.KindFunction, Language: "go", FilePath: "pkg.go"}}, nil)
	first := &graph.Edge{From: "caller", To: "unresolved::A", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1}
	later := &graph.Edge{From: "caller", To: "unresolved::A", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 2}
	if mode == "visitor" {
		later.To = "unresolved::import::pkg"
		later.Kind = graph.EdgeImports
	}
	if mode == "semantic" {
		later.To = "unresolved::zzz::unsupported"
	}
	b.AddBatch(nil, []*graph.Edge{first, later})
	before := rebuildFnPointerSaved(t, b)
	failure := errors.New("fixture candidate visitor")
	var out frontierGoldenResult
	err := runRebuildLinkFrontier(ctx, b, opts, nil, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
		f.imports.goOwnership = func(GoImportCandidate) (GoPackageOwnershipResult, error) { return GoPackageOwnershipUnknown, failure }
		if mode == "cancel" {
			c, cancel := context.WithCancel(ctx)
			cancel()
			ctx = c
		}
		moves, err := reduceRebuildMainPage(ctx, f, page)
		require.Empty(t, moves)
		out.Pages = append(out.Pages, frontierGoldenPage{Rows: len(page), Calls: f.link.RequestProgress().CandidateCalls, Moves: moves})
		return err
	})
	switch mode {
	case "visitor":
		require.ErrorIs(t, err, failure)
	case "cancel":
		require.ErrorIs(t, err, context.Canceled)
	case "semantic":
		require.ErrorIs(t, err, errRebuildMainRule)
	}
	out.Error = mode
	out.Saved = rebuildFnPointerSaved(t, b)
	require.Equal(t, before, out.Saved)
	_, err = b.OpenSnapshot()
	require.ErrorIs(t, err, store_pebble.ErrUnpublishedBuild)
	checkFrontierScratch(t, opts.ScratchParent)
	return out
}

func runFrontierGoldens(t *testing.T, capture bool) []frontierGoldenCase {
	var out []frontierGoldenCase
	cases := []struct {
		name string
		run  func(*testing.T, bool) any
	}{
		{"mixed", frontierGoldenMixed}, {"types", frontierGoldenTypes}, {"trace", frontierGoldenTrace}, {"caller", frontierGoldenCaller},
		{"type-wire", func(t *testing.T, legacy bool) any { return frontierGoldenAggregate(t, legacy, "type-wire") }},
		{"scalar-wire", func(t *testing.T, legacy bool) any { return frontierGoldenAggregate(t, legacy, "wire") }},
		{"scalar-result", func(t *testing.T, legacy bool) any { return frontierGoldenAggregate(t, legacy, "result") }},
	}
	for _, mode := range []string{"visitor", "cancel", "semantic"} {
		cases = append(cases, struct {
			name string
			run  func(*testing.T, bool) any
		}{mode, func(t *testing.T, legacy bool) any { return frontierGoldenFailure(t, legacy, mode) }})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { out = append(out, compareFrontierGolden(t, capture, c.name, c.run)) })
	}
	return out
}

func TestRebuildV3FrontierDifferential(t *testing.T) { runFrontierGoldens(t, false) }

func TestCaptureRebuildFrontierGoldens(t *testing.T) {
	prefix := os.Getenv("GORTEX_CAPTURE_GOLDEN")
	if prefix == "" {
		t.Skip("explicit H98 golden capture only")
	}
	if !filepath.IsAbs(prefix) {
		t.Fatal("capture prefix must be absolute")
	}
	cases := runFrontierGoldens(t, true)
	if t.Failed() {
		return
	}
	require.Len(t, cases, 10)
	raw, err := json.Marshal(cases)
	require.NoError(t, err)
	const maxBytes = 16 << 20
	require.LessOrEqual(t, len(raw), maxBytes)
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	_, err = w.Write(raw)
	require.NoError(t, errors.Join(err, w.Close()))
	require.LessOrEqual(t, compressed.Len(), maxBytes)
	header := struct {
		Cases, Bytes, CompressedBytes int
		SHA256, CompressedSHA256      string
	}{len(cases), len(raw), compressed.Len(), fmt.Sprintf("%x", sha256.Sum256(raw)), fmt.Sprintf("%x", sha256.Sum256(compressed.Bytes()))}
	meta, err := json.Marshal(header)
	require.NoError(t, err)
	for _, file := range []struct {
		path string
		data []byte
	}{{prefix + ".frontier.json.gz", compressed.Bytes()}, {prefix + ".frontier.header.json", meta}} {
		f, err := os.OpenFile(file.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		require.NoError(t, err)
		_, err = f.Write(file.data)
		require.NoError(t, errors.Join(err, f.Close()))
	}
}

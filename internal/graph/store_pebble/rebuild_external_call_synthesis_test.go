package store_pebble

import (
	"context"
	"iter"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildExternalCallOptionalCreator(t *testing.T) {
	const packageID = "external-call::dep::example.test/pkg"
	for _, tc := range []struct {
		name     string
		second   string
		existing bool
		refuse   bool
	}{
		{name: "later different creator", second: "dart", refuse: true},
		{name: "same language frontier", second: "python"},
		{name: "preexisting module", second: "dart", existing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			nodes := []*graph.Node{
				{ID: "a", FilePath: "a.py", Language: "python"},
				{ID: "b", FilePath: "b.deferred", Language: tc.second},
				{ID: "z", FilePath: "z.go", Language: "go"},
			}
			var facts []ParseFacts
			for _, n := range nodes {
				n.Kind, n.Name, n.QualName = graph.KindFunction, n.ID, n.ID
				e := &graph.Edge{From: n.ID, To: "dep::example.test/pkg::Call", Kind: graph.EdgeCalls, FilePath: n.FilePath, Line: 1}
				b.AddBatch([]*graph.Node{n}, []*graph.Edge{e})
				facts = append(facts, ParseFacts{EffectiveLanguage: n.Language, File: graph.FileMetaRow{FilePath: n.FilePath}, Nodes: []*graph.Node{n}, Edges: []*graph.Edge{e}})
			}
			if tc.existing {
				incumbent := &graph.Node{ID: packageID, Kind: graph.KindModule, Language: "existing"}
				b.AddNode(incumbent)
				facts[0].Nodes = append(facts[0].Nodes, incumbent)
			}
			candidate := func(n *graph.Node) RebuildExternalCallCandidate {
				return RebuildExternalCallCandidate{
					Node: &graph.Node{ID: packageID, Kind: graph.KindModule, Language: n.Language}, Language: n.Language,
				}
			}
			rules := RebuildExternalCallRules{
				Candidate: func(e *graph.Edge, n *graph.Node) (RebuildExternalCallCandidate, bool) {
					return candidate(n), strings.HasPrefix(e.To, "dep::")
				},
				Maybe: func(*graph.Edge, *graph.Node, string) []RebuildExternalCallCandidate { return nil },
				MayChangeLive: func(_ context.Context, _ RebuildView, e *graph.Edge, _ *graph.Node) (bool, bool, error) {
					return e.From == "a", false, nil
				},
				Rebind: func(e *graph.Edge, to, _ string) graph.EdgeReindex {
					old := e.To
					e.To = to
					e.Meta = map[string]any{"external_call": true}
					return graph.EdgeReindex{Edge: e, OldTo: old}
				},
			}
			receipts := iter.Seq2[ParseFacts, error](func(yield func(ParseFacts, error) bool) {
				for i := len(facts) - 1; i >= 0; i-- {
					if !yield(facts[i], nil) {
						return
					}
				}
			})
			before := rebuildFrameworkCustodyRows(t, b)
			count, err := b.RebuildExternalCallSynthesis(t.Context(), receipts, rules)
			if tc.refuse {
				require.ErrorIs(t, err, ErrRebuildExternalCallSynthesis)
				require.ErrorContains(t, err, "uncertain retained external-call package creator")
				require.Zero(t, count)
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b), "refusal cleans scratch without mutating the graph")
				return
			}
			require.NoError(t, err)
			require.Equal(t, uint64(3), count)
			n, err := b.pendingNode(packageID)
			require.NoError(t, err)
			require.NotNil(t, n)
			wantLanguage := "python"
			if tc.existing {
				wantLanguage = "existing"
			}
			require.Equal(t, wantLanguage, n.Language)
			for _, f := range facts {
				old := f.Edges[0]
				rebound := *old
				rebound.To = packageID
				rebound.Meta = map[string]any{"external_call": true}
				wantKey := pendingEdgeKey(&rebound)
				stored, err := b.pendingEdge(wantKey)
				require.NoError(t, err)
				require.Equal(t, &rebound, stored)
				requireRebuildLinkCustody(t, b, &rebound, rebuildEdgeCustody{raw: pendingEdgeKey(old)})
				gone, err := b.pendingEdge(pendingEdgeKey(old))
				require.NoError(t, err)
				require.Nil(t, gone)
			}
			after := rebuildFrameworkCustodyRows(t, b)
			count, err = b.RebuildExternalCallSynthesis(t.Context(), receipts, rules)
			require.NoError(t, err)
			require.Equal(t, uint64(3), count)
			require.Equal(t, after, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

const testExternalCallPackage = "external-call::dep::acme.tools"

func testExternalCallRules() RebuildExternalCallRules {
	return RebuildExternalCallRules{
		Candidate: func(e *graph.Edge, n *graph.Node) (RebuildExternalCallCandidate, bool) {
			if !strings.HasPrefix(e.To, "dep::acme.tools::") && !strings.HasPrefix(e.To, "dep::caller.tools::") {
				return RebuildExternalCallCandidate{}, false
			}
			lang := "go"
			if n != nil {
				lang = n.Language
			}
			id := testExternalCallPackage
			if strings.HasPrefix(e.To, "dep::caller.tools::") {
				id = "external-call::dep::caller.tools"
			}
			return RebuildExternalCallCandidate{Node: &graph.Node{ID: id, Kind: graph.KindModule, Language: lang}, Language: lang}, true
		},
		Maybe: func(e *graph.Edge, n *graph.Node, lang string) []RebuildExternalCallCandidate {
			if e.To != "unresolved::maybe" || lang != "custom" {
				return nil
			}
			if n != nil {
				lang = n.Language
			}
			return []RebuildExternalCallCandidate{{Node: &graph.Node{ID: testExternalCallPackage, Kind: graph.KindModule, Language: lang}, Language: lang, OldTo: "dep::acme.tools::Z"}}
		},
		Rebind: func(e *graph.Edge, to, _ string) graph.EdgeReindex {
			old := e.To
			e.To = to
			return graph.EdgeReindex{Edge: e, OldTo: old}
		},
	}
}

func testExternalCallFacts(file, language string, nodes []*graph.Node, edges ...*graph.Edge) ParseFacts {
	return ParseFacts{EffectiveLanguage: language, File: graph.FileMetaRow{FilePath: file}, Nodes: nodes, Edges: edges}
}

func testExternalCallReceipts(facts ...ParseFacts) iter.Seq2[ParseFacts, error] {
	return func(yield func(ParseFacts, error) bool) {
		for _, f := range facts {
			if !yield(f, nil) {
				return
			}
		}
	}
}

func TestRebuildExternalCallDeferredMovedReceipt(t *testing.T) {
	b := newBuild(t, FullProfile)
	deferred := &graph.Node{ID: "a", Language: "custom"}
	retained := &graph.Node{ID: "z", Language: "go"}
	raw := &graph.Edge{From: deferred.ID, Kind: graph.EdgeCalls, To: "unresolved::maybe", FilePath: "a.custom"}
	call := &graph.Edge{From: retained.ID, Kind: graph.EdgeCalls, To: "dep::acme.tools::A", FilePath: "z.go"}
	b.AddBatch([]*graph.Node{deferred, retained}, []*graph.Edge{raw, call})
	live := *raw
	live.To = "ordinary"
	require.NoError(t, b.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{{Edge: &live, OldTo: raw.To}}))
	requireRebuildLinkCustody(t, b, &live, rebuildEdgeCustody{raw: pendingEdgeKey(raw)})
	receipts := testExternalCallReceipts(
		testExternalCallFacts(raw.FilePath, "custom", []*graph.Node{deferred}, raw),
		testExternalCallFacts(call.FilePath, "go", []*graph.Node{retained}, call),
	)
	before := rebuildFrameworkCustodyRows(t, b)
	count, err := b.RebuildExternalCallSynthesis(t.Context(), receipts, testExternalCallRules())
	require.ErrorIs(t, err, ErrRebuildExternalCallSynthesis)
	require.ErrorContains(t, err, "uncertain retained external-call package creator")
	require.Zero(t, count)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
}

func TestRebuildExternalCallGroupReceiptProof(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
	}{
		{"missing winner", "uncertain retained external-call winner"},
		{"missing loser", ""},
		{"possible late deletion", "uncertain retained external-call deletion"},
		{"duplicate winner", "uncertain retained external-call duplicate payload"},
		{"duplicate loser", ""},
		{"caller language", "uncertain retained external-call terminal"},
		{"caller language alternate first", "uncertain retained external-call winner"},
		{"unproved terminal", "uncertain retained external-call terminal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			n := &graph.Node{ID: "caller", Language: "go"}
			a := &graph.Edge{From: n.ID, To: "dep::acme.tools::A", Kind: graph.EdgeCalls, FilePath: "m.go", Line: 1}
			if tc.name == "caller language" {
				// The optional live Go vote sorts before the possible receipt
				// TypeScript vote at the same native tuple.
				n.Language, a.FilePath = "typescript", "m.ts"
			}
			z := *a
			z.To = "dep::acme.tools::Z"
			facts := testExternalCallFacts(a.FilePath, n.Language, []*graph.Node{n}, a, &z)
			b.AddNode(n)
			var extra []ParseFacts
			switch tc.name {
			case "missing winner":
				b.AddEdge(&z)
			case "missing loser":
				b.AddEdge(a)
			case "possible late deletion":
				b.AddEdge(a)
				unresolved := z
				unresolved.To = "unresolved::maybe"
				b.AddEdge(&unresolved)
				facts.Edges = []*graph.Edge{a}
				extra = append(extra, testExternalCallFacts("plugin.custom", "custom", nil, &unresolved))
			case "duplicate winner", "duplicate loser":
				first := a
				if tc.name == "duplicate loser" {
					first = &z
				}
				last := *first
				last.Meta = map[string]any{"marker": "last"}
				facts.Edges = append(facts.Edges, &last)
				b.AddBatch(nil, facts.Edges)
			case "caller language", "caller language alternate first":
				changed := *n
				changed.Language = "typescript"
				if tc.name == "caller language" {
					changed.Language = "go"
				}
				b.AddNode(&changed)
				b.AddBatch(nil, facts.Edges)
			case "unproved terminal":
				b.AddEdge(a)
				facts.Edges = nil
			}
			before := rebuildFrameworkCustodyRows(t, b)
			count, err := b.RebuildExternalCallSynthesis(t.Context(), testExternalCallReceipts(append([]ParseFacts{facts}, extra...)...), testExternalCallRules())
			if tc.want != "" {
				require.ErrorIs(t, err, ErrRebuildExternalCallSynthesis)
				require.ErrorContains(t, err, tc.want)
				require.Zero(t, count)
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 1, count)
			want := *a
			want.To = testExternalCallPackage
			stored, err := b.pendingEdge(pendingEdgeKey(&want))
			require.NoError(t, err)
			require.Equal(t, &want, stored)
			gone, err := b.pendingEdge(pendingEdgeKey(&z))
			require.NoError(t, err)
			require.Nil(t, gone)
		})
	}
}

func TestRebuildExternalCallStationaryPackageProof(t *testing.T) {
	for _, mode := range []string{"stationary", "unproved live package", "proved live package", "possible creator"} {
		t.Run(mode, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			n := &graph.Node{ID: "z", Language: "go"}
			e := &graph.Edge{From: n.ID, To: testExternalCallPackage, FilePath: "a.go", Kind: graph.EdgeCalls}
			b.AddBatch([]*graph.Node{n}, []*graph.Edge{e})
			facts := []ParseFacts{testExternalCallFacts(e.FilePath, n.Language, []*graph.Node{n}, e)}
			switch mode {
			case "unproved live package", "proved live package":
				pkg := &graph.Node{ID: testExternalCallPackage, Kind: graph.KindModule, Language: "python"}
				b.AddNode(pkg)
				if mode == "proved live package" {
					facts[0].Nodes = append(facts[0].Nodes, pkg)
				}
			case "possible creator":
				a := &graph.Node{ID: "a", Language: "custom"}
				unresolved := &graph.Edge{From: a.ID, To: "unresolved::maybe", FilePath: "a.custom", Kind: graph.EdgeCalls}
				b.AddBatch([]*graph.Node{a}, []*graph.Edge{unresolved})
				facts = append(facts, testExternalCallFacts(unresolved.FilePath, a.Language, []*graph.Node{a}, unresolved))
			}
			before := rebuildFrameworkCustodyRows(t, b)
			count, err := b.RebuildExternalCallSynthesis(t.Context(), testExternalCallReceipts(facts...), testExternalCallRules())
			switch mode {
			case "unproved live package":
				require.ErrorContains(t, err, "unproved retained external-call package incumbent")
				require.Zero(t, count)
			case "possible creator":
				require.ErrorContains(t, err, "uncertain retained external-call package creator")
				require.Zero(t, count)
			default:
				require.NoError(t, err)
				require.EqualValues(t, 1, count)
			}
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildExternalCallNormalizedRecordingPath(t *testing.T) {
	b := newBuild(t, FullProfile)
	n := &graph.Node{ID: "caller", Language: "custom"}
	e := &graph.Edge{From: n.ID, To: "dep::acme.tools::A", FilePath: "café.go", Kind: graph.EdgeCalls}
	b.AddBatch([]*graph.Node{n}, []*graph.Edge{e})
	// File ownership is retained even when the receipt file path is NFD
	// and the caller belongs to a deferred extractor.
	facts := testExternalCallFacts("cafe\u0301.go", "go", []*graph.Node{n})
	before := rebuildFrameworkCustodyRows(t, b)
	count, err := b.RebuildExternalCallSynthesis(t.Context(), testExternalCallReceipts(facts), testExternalCallRules())
	require.ErrorContains(t, err, "uncertain retained external-call terminal")
	require.Zero(t, count)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
}

func TestRebuildExternalCallNonVotingRows(t *testing.T) {
	for _, mode := range []string{"stale producer", "large nonvote", "large vote"} {
		t.Run(mode, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			n := &graph.Node{ID: "caller", Language: "go"}
			e := &graph.Edge{From: n.ID, To: "dep::unclassified::X", FilePath: "m.go", Kind: graph.EdgeCalls}
			if mode != "stale producer" {
				e.Meta = map[string]any{"padding": strings.Repeat("x", rebuildExternalCallRowBytes)}
			}
			if mode == "large vote" {
				e.To = "dep::acme.tools::A"
			}
			b.AddBatch([]*graph.Node{n}, []*graph.Edge{e})
			if mode == "stale producer" {
				stamp, err := (rebuildEdgeCustody{proof: rebuildProofDirect}).encode([]byte("stale"))
				require.NoError(t, err)
				require.NoError(t, b.pending.Set(rebuildLinkRecorderKey(pendingEdgeKey(e)), stamp, pebble.NoSync))
			}
			before := rebuildFrameworkCustodyRows(t, b)
			facts := testExternalCallFacts(e.FilePath, n.Language, []*graph.Node{n}, e)
			count, err := b.RebuildExternalCallSynthesis(t.Context(), testExternalCallReceipts(facts), testExternalCallRules())
			if mode == "large vote" {
				require.ErrorIs(t, err, ErrRebuildLinkInputSize)
			} else {
				require.NoError(t, err)
			}
			require.Zero(t, count)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildExternalCallRefusalInputs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		want  string
		large bool
	}{
		{"unproved incumbent", "unproved retained external-call incumbent", false},
		{"first duplicate oversized", "external-call duplicate receipt", true},
		{"absent receipt oversized", "external-call receipt input", true},
		{"voting caller oversized", "external-call caller input", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			n := &graph.Node{ID: "caller", Language: "go"}
			e := &graph.Edge{From: n.ID, To: "dep::acme.tools::A", FilePath: "m.go", Kind: graph.EdgeCalls}
			facts := testExternalCallFacts(e.FilePath, n.Language, []*graph.Node{n}, e)
			switch tc.name {
			case "unproved incumbent":
				e.To = testExternalCallPackage
				facts.Edges = nil
				b.AddBatch(facts.Nodes, []*graph.Edge{e})
			case "first duplicate oversized":
				first := *e
				first.Meta = map[string]any{"padding": strings.Repeat("x", rebuildExternalCallRowBytes)}
				facts.Edges = []*graph.Edge{&first, e}
				b.AddBatch(facts.Nodes, facts.Edges)
			case "absent receipt oversized":
				e.Meta = map[string]any{"padding": strings.Repeat("x", rebuildExternalCallRowBytes)}
				b.AddNode(n)
			case "voting caller oversized":
				n.Meta = map[string]any{"padding": strings.Repeat("x", rebuildExternalCallRowBytes)}
				b.AddBatch(facts.Nodes, facts.Edges)
			}
			before := rebuildFrameworkCustodyRows(t, b)
			count, err := b.RebuildExternalCallSynthesis(t.Context(), testExternalCallReceipts(facts), testExternalCallRules())
			require.ErrorIs(t, err, ErrRebuildExternalCallSynthesis)
			if tc.large {
				require.ErrorIs(t, err, ErrRebuildLinkInputSize)
			}
			require.ErrorContains(t, err, tc.want)
			require.Zero(t, count)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildExternalCallLiveKeyFilter(t *testing.T) {
	for _, to := range []string{"ordinary", "dep::acme.tools::A"} {
		t.Run(to, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			n := &graph.Node{ID: "caller", Language: "go"}
			e := &graph.Edge{From: n.ID, To: to, FilePath: "m.go", Kind: graph.EdgeCalls}
			b.AddBatch([]*graph.Node{n}, []*graph.Edge{e})
			// A malformed stamp makes a custody read observable. Only the
			// terminal key belongs to the central live election.
			require.NoError(t, b.pending.Set(rebuildLinkRecorderKey(pendingEdgeKey(e)), []byte{0}, pebble.NoSync))
			before := rebuildFrameworkCustodyRows(t, b)
			facts := testExternalCallFacts(e.FilePath, n.Language, []*graph.Node{n}, e)
			count, err := b.RebuildExternalCallSynthesis(t.Context(), testExternalCallReceipts(facts), testExternalCallRules())
			if to == "ordinary" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrRebuildExternalCallSynthesis)
				require.ErrorContains(t, err, "edge custody format")
			}
			require.Zero(t, count)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildExternalCallReceiptPayloadFilter(t *testing.T) {
	b := newBuild(t, FullProfile)
	p := rebuildExternalCallPass{b: b, rules: testExternalCallRules()}
	for _, tc := range []struct {
		name string
		lang string
		to   string
		want bool
	}{
		{"Go unresolved", "go", "unresolved::maybe", false},
		{"JS unresolved", "javascript", "unresolved::maybe", false},
		{"TS unresolved", "typescript", "unresolved::maybe", false},
		{"no extractor", "", "unresolved::maybe", false},
		{"ordinary deferred", "custom", "ordinary", false},
		{"ordinary retained", "go", "ordinary", false},
		{"stationary package", "go", testExternalCallPackage, false},
		{"deferred unresolved", "custom", "unresolved::maybe", true},
		{"retained terminal", "go", "dep::acme.tools::A", true},
		{"deferred terminal", "custom", "external::pkg", true},
		{"stdlib terminal", "typescript", "repo::stdlib::pkg::A", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &graph.Edge{From: tc.name, To: tc.to, FilePath: tc.lang + ".source", Kind: graph.EdgeCalls}
			last := *e
			last.Meta = map[string]any{"marker": "last"}
			facts := testExternalCallFacts(e.FilePath, tc.lang, nil, e, &last)
			require.NoError(t, p.receipts(t.Context(), testExternalCallReceipts(facts)))
			key := pendingEdgeKey(e)
			for _, column := range []byte{'r', 'j'} {
				present, err := p.present(append(rebuildExternalCallColumn(column), key...))
				require.NoError(t, err)
				require.True(t, present, "receipt identity and duplicate evidence must survive")
			}
			h, err := pendingGet(b.pending, append(rebuildExternalCallColumn('h'), key...), copyValue)
			require.NoError(t, err)
			if !tc.want {
				require.Nil(t, h)
				return
			}
			lang, raw, err := readKeyString(h)
			require.NoError(t, err)
			require.Equal(t, tc.lang, lang)
			first, err := decodeEdge(raw)
			require.NoError(t, err)
			require.Equal(t, e, first, "must keep the first payload, not the last")
		})
	}
}

func TestRebuildExternalCallDuplicateOutputProof(t *testing.T) {
	b := newBuild(t, FullProfile)
	n := &graph.Node{ID: "caller", Language: "go"}
	first := &graph.Edge{
		From: n.ID, To: "dep::acme.tools::A", FilePath: "m.go", Kind: graph.EdgeCalls,
		Meta: map[string]any{"marker": "keep", "go_import": "first"},
	}
	last := *first
	last.Meta = map[string]any{"marker": "keep", "go_import": "last"}
	facts := testExternalCallFacts(first.FilePath, n.Language, []*graph.Node{n}, first, &last)
	b.AddBatch(facts.Nodes, facts.Edges)
	rules := testExternalCallRules()
	rebind := rules.Rebind
	rules.Rebind = func(e *graph.Edge, to, lang string) graph.EdgeReindex {
		delete(e.Meta, "go_import")
		return rebind(e, to, lang)
	}
	count, err := b.RebuildExternalCallSynthesis(t.Context(), testExternalCallReceipts(facts), rules)
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
	want := *first
	want.To = testExternalCallPackage
	want.Meta = map[string]any{"marker": "keep"}
	stored, err := b.pendingEdge(pendingEdgeKey(&want))
	require.NoError(t, err)
	require.Equal(t, &want, stored)
	gone, err := b.pendingEdge(pendingEdgeKey(first))
	require.NoError(t, err)
	require.Nil(t, gone)
}

func TestRebuildExternalCallChunkCaller(t *testing.T) {
	const callerID = "external-call::dep::caller.tools"
	b := newBuild(t, FullProfile)
	a := &graph.Node{ID: "a", Language: "python"}
	z := &graph.Node{ID: "z", Language: "go"}
	var first []*graph.Edge
	for i := 0; i < 512; i++ {
		first = append(first, &graph.Edge{From: a.ID, Kind: graph.EdgeCalls, To: "dep::caller.tools::A", FilePath: "a.py", Line: i + 1})
	}
	missing := &graph.Edge{From: callerID, Kind: graph.EdgeCalls, To: "dep::acme.tools::A", FilePath: "deferred.custom"}
	last := &graph.Edge{From: z.ID, Kind: graph.EdgeCalls, To: "dep::acme.tools::A", FilePath: "z.go"}
	b.AddBatch([]*graph.Node{a, z}, first)
	b.AddBatch(nil, []*graph.Edge{missing, last})
	facts := testExternalCallReceipts(
		testExternalCallFacts("a.py", "python", []*graph.Node{a}, first...),
		testExternalCallFacts(missing.FilePath, "custom", nil, missing),
		testExternalCallFacts(last.FilePath, "go", []*graph.Node{z}, last),
	)
	before := rebuildFrameworkCustodyRows(t, b)
	count, err := b.RebuildExternalCallSynthesis(t.Context(), facts, testExternalCallRules())
	require.ErrorContains(t, err, "uncertain retained external-call chunk caller")
	require.Zero(t, count)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
}

func TestRebuildExternalCallInputInvariants(t *testing.T) {
	for _, mode := range []string{"nil receipts", "nil rules", "occupied scratch", "candidate terminal", "candidate module", "candidate identity", "candidate old terminal", "rebind identity"} {
		t.Run(mode, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			n := &graph.Node{ID: "caller", Language: "go"}
			e := &graph.Edge{From: n.ID, Kind: graph.EdgeCalls, To: "dep::acme.tools::A", FilePath: "m.go"}
			b.AddBatch([]*graph.Node{n}, []*graph.Edge{e})
			rules := testExternalCallRules()
			receipts := testExternalCallReceipts(testExternalCallFacts(e.FilePath, "go", []*graph.Node{n}, e))
			var want string
			switch mode {
			case "nil receipts":
				receipts = nil
				want = "requires receipts and rules"
			case "nil rules":
				rules.Maybe = nil
				want = "requires receipts and rules"
			case "occupied scratch":
				require.NoError(t, b.pending.Set(rebuildExternalCallColumn('g'), []byte("occupied"), pebble.NoSync))
				want = "scratch range occupied"
			case "candidate terminal":
				original := rules.Candidate
				rules.Candidate = func(e *graph.Edge, n *graph.Node) (RebuildExternalCallCandidate, bool) {
					c, ok := original(e, n)
					c.OldTo = "wrong"
					return c, ok
				}
				want = "live terminal changed by candidate classifier"
			case "candidate module":
				original := rules.Candidate
				rules.Candidate = func(e *graph.Edge, n *graph.Node) (RebuildExternalCallCandidate, bool) {
					c, ok := original(e, n)
					c.Node.Kind = graph.KindFunction
					return c, ok
				}
				want = "module candidate"
			case "candidate identity", "candidate old terminal":
				rules.Extra = func(_ context.Context, _ RebuildView, yield func(*graph.Edge, RebuildExternalCallCandidate) error) error {
					c := RebuildExternalCallCandidate{Node: &graph.Node{ID: testExternalCallPackage}, OldTo: testExternalCallPackage}
					if mode == "candidate identity" {
						c.Node = nil
					}
					return yield(e, c)
				}
				want = mode
			case "rebind identity":
				rules.Rebind = func(e *graph.Edge, to, _ string) graph.EdgeReindex {
					old := e.To
					e.To = to
					e.Line++
					return graph.EdgeReindex{Edge: e, OldTo: old}
				}
				want = "rebind changed the site identity"
			}
			before := rebuildFrameworkCustodyRows(t, b)
			count, err := b.RebuildExternalCallSynthesis(t.Context(), receipts, rules)
			require.ErrorIs(t, err, ErrRebuildExternalCallSynthesis)
			require.ErrorContains(t, err, want)
			require.Zero(t, count)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildExternalCallExtraInputCaps(t *testing.T) {
	b := newBuild(t, FullProfile)
	padding := strings.Repeat("x", rebuildExternalCallRowBytes)
	n := &graph.Node{ID: "large", Kind: graph.KindFile, Meta: map[string]any{"padding": padding}}
	e := &graph.Edge{From: "large", To: "target", Kind: graph.EdgeReferences, Meta: map[string]any{"padding": padding}}
	b.AddBatch([]*graph.Node{n}, []*graph.Edge{e})
	view := b.RebuildView()
	selectEdge := func(id graph.EdgeIdentity) bool { return id.Kind == graph.EdgeCalls }
	require.NoError(t, view.VisitExternalCallEdges(t.Context(), selectEdge, func(*graph.Edge) error {
		t.Fatal("unselected edge decoded")
		return nil
	}))
	require.ErrorIs(t, view.VisitExternalCallEdges(t.Context(), func(graph.EdgeIdentity) bool { return true }, func(*graph.Edge) error {
		t.Fatal("oversized edge yielded")
		return nil
	}), ErrRebuildLinkInputSize)
	selectNode := func(n *graph.Node) bool { return n.Kind != graph.KindFile }
	require.NoError(t, view.VisitExternalCallNodes(t.Context(), selectNode, func(*graph.Node) error {
		t.Fatal("unselected node yielded")
		return nil
	}))
	require.ErrorIs(t, view.VisitExternalCallNodes(t.Context(), func(*graph.Node) bool { return true }, func(*graph.Node) error {
		t.Fatal("oversized node yielded")
		return nil
	}), ErrRebuildLinkInputSize)
	_, err := view.ExternalCallNode(t.Context(), n.ID)
	require.ErrorIs(t, err, ErrRebuildLinkInputSize)
	_, err = view.ExternalCallTarget(t.Context(), n.ID)
	require.ErrorIs(t, err, ErrRebuildLinkInputSize)
}

func TestRebuildExternalCallPayloadRefresh(t *testing.T) {
	for _, mode := range []string{"winner", "loser", "occupied", "deferred creator"} {
		t.Run(mode, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			first := &graph.Node{ID: "a", Language: "go"}
			lang := "go"
			if mode == "deferred creator" {
				first.Language = "python"
				lang = "python"
			}
			a := &graph.Edge{From: first.ID, To: "dep::acme.tools::A", Kind: graph.EdgeCalls, FilePath: "a.source"}
			z := *a
			z.To = "dep::acme.tools::Z"
			facts := []ParseFacts{testExternalCallFacts(a.FilePath, lang, []*graph.Node{first}, a, &z)}
			var incumbent graph.Edge
			if mode == "occupied" {
				incumbent = *a
				incumbent.To = testExternalCallPackage
				facts[0].Edges = append(facts[0].Edges, &incumbent)
			}
			if mode == "deferred creator" {
				n := &graph.Node{ID: "z", Language: "go"}
				later := &graph.Edge{From: n.ID, To: a.To, Kind: graph.EdgeCalls, FilePath: "z.go"}
				facts = append(facts, testExternalCallFacts(later.FilePath, n.Language, []*graph.Node{n}, later))
			}
			for _, f := range facts {
				b.AddBatch(f.Nodes, f.Edges)
			}
			changed := a.To
			if mode == "loser" {
				changed = z.To
			}
			rules := testExternalCallRules()
			rules.MayChangeLive = func(_ context.Context, _ RebuildView, e *graph.Edge, _ *graph.Node) (bool, bool, error) {
				return false, e.From == first.ID && e.To == changed, nil
			}
			before := rebuildFrameworkCustodyRows(t, b)
			count, err := b.RebuildExternalCallSynthesis(t.Context(), testExternalCallReceipts(facts...), rules)
			if mode == "winner" {
				require.ErrorIs(t, err, ErrRebuildExternalCallSynthesis)
				require.ErrorContains(t, err, "uncertain retained external-call winner payload")
				require.Zero(t, count)
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
				return
			}
			require.NoError(t, err)
			want := 1
			if mode == "occupied" || mode == "deferred creator" {
				want = 2
			}
			require.EqualValues(t, want, count)
			if mode == "occupied" {
				stored, err := b.pendingEdge(pendingEdgeKey(&incumbent))
				require.NoError(t, err)
				require.Equal(t, &incumbent, stored)
			}
			if mode == "deferred creator" {
				packageNode, err := b.pendingNode(testExternalCallPackage)
				require.NoError(t, err)
				require.Equal(t, "python", packageNode.Language)
			}
		})
	}
}

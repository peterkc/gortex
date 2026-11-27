package store_pebble

import (
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func ginProofTestRules() RebuildGinRules {
	return RebuildGinRules{
		Dispatcher: func(n *graph.Node) bool {
			flag, _ := n.Meta["gin_dispatcher"].(bool)
			return flag
		},
		Names: func(n *graph.Node) []string {
			names, _ := n.Meta["gin_handlers"].([]string)
			return names
		},
		Target: func(d, h *graph.Node) bool { return d.ID != h.ID && d.WorkspaceID == h.WorkspaceID },
		Edge: func(d, h *graph.Node) *graph.Edge {
			return &graph.Edge{From: d.ID, To: h.ID, Kind: graph.EdgeCalls, FilePath: d.FilePath, Line: d.StartLine}
		},
		SeenKey:    func(d, h string) string { return d + "\x00" + h },
		GoLanguage: func(language string) bool { return language == "go" },
		Threshold:  1,
	}
}

func TestRebuildGinMiddlewareScratchAndBounds(t *testing.T) {
	const file = "gin.go"
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	d := &graph.Node{ID: "dispatcher", Kind: graph.KindMethod, Language: "go", FilePath: file,
		Meta: map[string]any{"gin_dispatcher": true, "gin_handlers": []string{"Handle"}}}
	h := &graph.Node{ID: "handler", Name: "Handle", Kind: graph.KindFunction, FilePath: file}
	b.AddBatch([]*graph.Node{d, h}, nil)
	facts := ParseFacts{EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: file}, Nodes: []*graph.Node{d, h}}
	receipts := func(yield func(ParseFacts, error) bool) { yield(facts, nil) }
	rules := ginProofTestRules()
	before := rebuildFrameworkCustodyRows(t, b)
	for _, tc := range []struct {
		name   string
		change func(*RebuildGinRules)
	}{
		{"zero threshold", func(r *RebuildGinRules) { r.Threshold = 0 }},
		{"no dispatcher predicate", func(r *RebuildGinRules) { r.Dispatcher = nil }},
		{"no registration names", func(r *RebuildGinRules) { r.Names = nil }},
		{"no target predicate", func(r *RebuildGinRules) { r.Target = nil }},
		{"no edge constructor", func(r *RebuildGinRules) { r.Edge = nil }},
		{"no seen key", func(r *RebuildGinRules) { r.SeenKey = nil }},
		{"no Go language predicate", func(r *RebuildGinRules) { r.GoLanguage = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unusable := rules
			tc.change(&unusable)
			count, err := b.RebuildGinMiddleware(ctx, receipts, unusable)
			require.Zero(t, count)
			require.ErrorIs(t, err, ErrRebuildGinMiddleware)
			require.ErrorContains(t, err, "gin middleware inputs")
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
	count, err := b.RebuildGinMiddleware(ctx, nil, rules)
	require.Zero(t, count)
	require.ErrorIs(t, err, ErrRebuildGinMiddleware)
	require.ErrorContains(t, err, "gin middleware inputs")
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))

	stranger := []byte{'g', 'q', 'G', 'x'}
	require.NoError(t, b.pending.Set(stranger, []byte("owned"), pebble.NoSync))
	count, err = b.RebuildGinMiddleware(ctx, receipts, rules)
	require.Zero(t, count)
	require.ErrorContains(t, err, "scratch range occupied")
	before[string(stranger)] = []byte("owned")
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
	require.NoError(t, b.pending.Delete(stranger, pebble.NoSync))

	big := strings.Repeat("x", 16<<20)
	other := &graph.Node{ID: "irrelevant", Kind: graph.KindVariable, FilePath: file, Meta: map[string]any{"large": big}}
	b.AddNode(other)
	tooBig := *h
	tooBig.Meta = other.Meta
	b.AddNode(&tooBig)
	count, err = b.RebuildGinMiddleware(ctx, receipts, rules)
	require.Zero(t, count)
	require.ErrorIs(t, err, ErrRebuildLinkInputSize)
	busy, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildGinPrefix)
	require.NoError(t, err)
	require.False(t, busy, "the pass must drop its scratch on refusal")

	b.AddNode(h)
	count, err = b.RebuildGinMiddleware(ctx, receipts, rules)
	require.NoError(t, err, "the oversized non-callable row is unrelated")
	require.EqualValues(t, 1, count)
	busy, err = rebuildGuardHasPrefix(ctx, b.pending, rebuildGinPrefix)
	require.NoError(t, err)
	require.False(t, busy, "the pass must drop its scratch after applying")
	key := pendingEdgeKey(rules.Edge(d, h))
	custody, err := readRebuildEdgeCustody(b.pending, key)
	require.NoError(t, err)
	require.Equal(t, rebuildEdgeCustody{proof: rebuildProofUntrackedMove}, custody)
}

func TestRebuildGinMiddlewareReceiptAdmission(t *testing.T) {
	const file = "gin.go"
	d := &graph.Node{ID: "dispatch", Kind: graph.KindMethod, Language: "go", FilePath: file,
		Meta: map[string]any{"gin_dispatcher": true}}
	h := &graph.Node{ID: "handle", Name: "Handle", Kind: graph.KindFunction, Language: "go", FilePath: file}
	r := &graph.Node{ID: "register", Kind: graph.KindFunction, Language: "go", FilePath: file,
		Meta: map[string]any{"gin_handlers": []string{"Handle"}}}
	changed := *r
	changed.Meta = nil
	rules := ginProofTestRules()
	for _, tc := range []struct{ name, file, language string }{
		{"no effective language", file, ""},
		{"another file", "other.go", "go"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			nodes := []*graph.Node{d, h, r}
			b.AddBatch(nodes, nil)
			facts := ParseFacts{EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: file}, Nodes: nodes}
			bad := ParseFacts{EffectiveLanguage: tc.language, File: graph.FileMetaRow{FilePath: tc.file}, Nodes: []*graph.Node{&changed}}
			receipts := func(yield func(ParseFacts, error) bool) {
				if yield(facts, nil) {
					yield(bad, nil)
				}
			}
			count, err := b.RebuildGinMiddleware(t.Context(), receipts, rules)
			require.NoError(t, err)
			require.EqualValues(t, 1, count)
			want := rules.Edge(d, h)
			got, err := b.pendingEdge(pendingEdgeKey(want))
			require.NoError(t, err)
			require.Equal(t, want, got)
		})
	}
}

func TestRebuildGinMiddlewareOptionalOccupiedThreshold(t *testing.T) {
	const file = "gin.go"
	callable := func(id, name, workspace string) *graph.Node {
		return &graph.Node{ID: id, Name: name, Kind: graph.KindFunction, Language: "go", FilePath: file, WorkspaceID: workspace}
	}
	early := callable("a-dispatch", "dispatch", "live")
	early.Meta = map[string]any{"gin_dispatcher": true}
	rawEarly := *early
	rawEarly.WorkspaceID = "receipt"
	late := callable("z-dispatch", "dispatch", "late")
	late.Meta = map[string]any{"gin_dispatcher": true, "gin_handlers": []string{"B"}}
	registrar := callable("register", "register", "live")
	registrar.Meta = map[string]any{"gin_handlers": []string{"A"}}
	first, second := callable("late-1", "B", "late"), callable("late-2", "B", "late")
	rawFirst, rawSecond := callable("raw-1", "A", "receipt"), callable("raw-2", "A", "receipt")
	rules := ginProofTestRules()
	rules.Threshold = 2
	for _, tc := range []struct {
		name       string
		liveOffers bool
	}{
		{"possible early offers can remove a live output", false},
		{"optional group cannot guarantee a full budget", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			nodes := []*graph.Node{early, registrar, late, first, second, rawFirst, rawSecond}
			rawNodes := []*graph.Node{&rawEarly, late, first, second, rawFirst, rawSecond}
			// Only a missing registration receipt makes the early group optional.
			// Both receipt-only offers hit occupied keys, but still count.
			edges := []*graph.Edge{rules.Edge(&rawEarly, rawFirst), rules.Edge(&rawEarly, rawSecond)}
			if tc.liveOffers {
				liveFirst, liveSecond := callable("live-1", "A", "live"), callable("live-2", "A", "live")
				nodes = append(nodes, liveFirst, liveSecond)
				rawNodes = append(rawNodes, liveFirst, liveSecond)
				edges = append(edges, rules.Edge(early, liveFirst), rules.Edge(early, liveSecond))
			}
			b.AddBatch(nodes, edges)
			before := rebuildFrameworkCustodyRows(t, b)
			facts := ParseFacts{EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: file}, Nodes: rawNodes}
			receipts := func(yield func(ParseFacts, error) bool) { yield(facts, nil) }
			count, err := b.RebuildGinMiddleware(t.Context(), receipts, rules)
			require.Zero(t, count)
			require.ErrorIs(t, err, ErrRebuildGinMiddleware)
			var refused *RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			require.EqualValues(t, 1, refused.Count)
			require.Equal(t, graph.EdgeIdentityFor(rules.Edge(late, second)), refused.First)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildGinMiddlewareSharedNULSeenKey(t *testing.T) {
	const file = "gin.go"
	first := &graph.Node{ID: "a", Kind: graph.KindMethod, Language: "go", FilePath: file, WorkspaceID: "one",
		Meta: map[string]any{"gin_dispatcher": true, "gin_handlers": []string{"Handle"}}}
	second := &graph.Node{ID: "a\x00b", Kind: graph.KindMethod, Language: "go", FilePath: file, WorkspaceID: "two",
		Meta: map[string]any{"gin_dispatcher": true}}
	x := &graph.Node{ID: "b\x00c", Name: "Handle", Kind: graph.KindFunction, Language: "go", FilePath: file, WorkspaceID: "one"}
	y := &graph.Node{ID: "c", Name: "Handle", Kind: graph.KindFunction, Language: "go", FilePath: file, WorkspaceID: "two"}
	rules := ginProofTestRules()
	require.Equal(t, rules.SeenKey(first.ID, x.ID), rules.SeenKey(second.ID, y.ID))
	for _, omit := range []bool{false, true} {
		b := newBuild(t, FullProfile)
		incumbent := rules.Edge(first, x)
		b.AddBatch([]*graph.Node{second, y, first, x}, []*graph.Edge{incumbent})
		before := rebuildFrameworkCustodyRows(t, b)
		nodes := []*graph.Node{second, y, first}
		if !omit {
			nodes = append(nodes, x)
		}
		facts := ParseFacts{EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: file}, Nodes: nodes}
		receipts := func(yield func(ParseFacts, error) bool) { yield(facts, nil) }
		count, err := b.RebuildGinMiddleware(t.Context(), receipts, rules)
		if omit {
			require.Zero(t, count)
			var refused *RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			require.EqualValues(t, 1, refused.Count)
			require.Equal(t, graph.EdgeIdentityFor(rules.Edge(second, y)), refused.First)
		} else {
			require.NoError(t, err)
			require.EqualValues(t, 1, count)
		}
		require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
	}
}

func TestRebuildGinMiddlewareOccupiedDispatcherLine(t *testing.T) {
	const file = "gin.go"
	d := &graph.Node{ID: "dispatch", Kind: graph.KindMethod, Language: "go", FilePath: file, StartLine: 1,
		Meta: map[string]any{"gin_dispatcher": true, "gin_handlers": []string{"Handle"}}}
	h := &graph.Node{ID: "handle", Name: "Handle", Kind: graph.KindFunction, Language: "go", FilePath: file}
	changed := *d
	changed.StartLine++
	rules := ginProofTestRules()
	b := newBuild(t, FullProfile)
	b.AddBatch([]*graph.Node{d, h}, []*graph.Edge{rules.Edge(d, h), rules.Edge(&changed, h)})
	before := rebuildFrameworkCustodyRows(t, b)
	facts := ParseFacts{EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: file}, Nodes: []*graph.Node{&changed, h}}
	receipts := func(yield func(ParseFacts, error) bool) { yield(facts, nil) }
	count, err := b.RebuildGinMiddleware(t.Context(), receipts, rules)
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
}

func TestRebuildGinMiddlewareOptionalNULSeenKey(t *testing.T) {
	const file = "gin.go"
	first := &graph.Node{ID: "a", Kind: graph.KindMethod, Language: "go", FilePath: file, WorkspaceID: "one",
		Meta: map[string]any{"gin_dispatcher": true}}
	second := &graph.Node{ID: "a\x00b", Kind: graph.KindMethod, Language: "go", FilePath: file, WorkspaceID: "two",
		Meta: map[string]any{"gin_dispatcher": true, "gin_handlers": []string{"Second"}}}
	x := &graph.Node{ID: "b\x00c", Name: "First", Kind: graph.KindFunction, Language: "go", FilePath: file, WorkspaceID: "one"}
	y := &graph.Node{ID: "c", Name: "Second", Kind: graph.KindFunction, Language: "go", FilePath: file, WorkspaceID: "two"}
	registrar := &graph.Node{ID: "register", Kind: graph.KindFunction, Language: "go", FilePath: file,
		Meta: map[string]any{"gin_handlers": []string{"First"}}}
	rules := ginProofTestRules()
	require.Equal(t, rules.SeenKey(first.ID, x.ID), rules.SeenKey(second.ID, y.ID))
	b := newBuild(t, FullProfile)
	// The earlier pair only exists on the receipt side: a missing registrar
	// receipt could leave its name known or absent. Its physical key is occupied.
	liveFirst := *first
	liveFirst.WorkspaceID = "other"
	b.AddBatch([]*graph.Node{&liveFirst, second, x, y, registrar}, []*graph.Edge{rules.Edge(first, x)})
	before := rebuildFrameworkCustodyRows(t, b)
	facts := ParseFacts{EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: file}, Nodes: []*graph.Node{first, second, x, y}}
	receipts := func(yield func(ParseFacts, error) bool) { yield(facts, nil) }
	count, err := b.RebuildGinMiddleware(t.Context(), receipts, rules)
	require.Zero(t, count)
	var refused *RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &refused)
	require.EqualValues(t, 1, refused.Count)
	require.Equal(t, graph.EdgeIdentityFor(rules.Edge(second, y)), refused.First)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
}

package resolver

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

const (
	rebuildCCDispatch = "cc_dispatch_field"
	rebuildCCAppend   = "cc_append_field"
)

func TestRebuildClosureCollectionSQLiteOracle(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra int
	}{
		{"base fixture", 0},
		{"extra fields", 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const file, language = "request.swift", "swift"
			dispatch := rebuildClosureCollectionNode(file, "dispatch", language, rebuildCCDispatch, "handlers")
			keep := rebuildClosureCollectionNode(file, "keep", language, rebuildCCAppend, "handlers")
			register := rebuildClosureCollectionNode(file, "register", language, rebuildCCAppend, "handlers")
			incumbent := rebuildObserverEdge(dispatch, keep, graph.EdgeCalls, dispatch.StartLine)
			incumbent.Meta = map[string]any{"fixture": "durable incumbent"}
			nodes := []*graph.Node{dispatch, keep, register}
			if tc.extra != 0 {
				// Each role's candidate stream exceeds 1 MiB, but no field is over cap.
				padding := strings.Repeat("x", 32<<10)
				for i := range tc.extra {
					field := fmt.Sprintf("items%02d%s", i, padding)
					dispatchName, registerName := fmt.Sprintf("dispatch%02d", i), fmt.Sprintf("register%02d", i)
					nodes = append(nodes,
						rebuildClosureCollectionNode(file, dispatchName, language, rebuildCCDispatch, field),
						rebuildClosureCollectionNode(file, registerName, language, rebuildCCAppend, field),
					)
				}
			}
			facts := rebuildEventFacts(file, language, nodes, incumbent)
			proof, b, native := openRebuildObserverStores(t, nil, facts)
			receipts := rebuildGRPCReceipts(proof...)
			before := rebuildEventSavedRows(t, b)
			disabled, err := NewFrameworkSynthesizerSelection(nil)
			require.NoError(t, err)
			stats, err := RunRebuildClosureCollection(t.Context(), b, receipts, disabled)
			require.NoError(t, err)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, b))

			selection := rebuildClosureCollectionSelection(t)
			var first rebuildEventRows
			for run := range 2 {
				require.NoError(t, RunFrameworkSynthesizersWithSelection(native, selection).Err)
				want := rebuildEventSQLiteRows(native)
				require.Len(t, want.edges, 2+tc.extra)
				require.Contains(t, want.edges, incumbent)
				stats, err := RunRebuildClosureCollection(t.Context(), b, receipts, selection)
				require.NoError(t, err)
				got := rebuildEventSavedRows(t, b)
				require.Equal(t, want, got)
				if run == 0 {
					require.Positive(t, stats.Edges)
					first = got
					continue
				}
				require.Equal(t, first, got)
			}
		})
	}
}

func TestRebuildClosureCollectionRawRoleCaps(t *testing.T) {
	for _, role := range []struct {
		name, capped, opposite string
	}{
		{"registrars", rebuildCCAppend, rebuildCCDispatch},
		{"dispatchers", rebuildCCDispatch, rebuildCCAppend},
	} {
		for _, raw := range []int{8, 9} {
			t.Run(fmt.Sprintf("%s/%d", role.name, raw), func(t *testing.T) {
				const file, language, field = "request.swift", "swift", "handlers"
				both := rebuildClosureCollectionNode(file, "both", language, role.capped, field)
				both.Meta[role.opposite] = field
				nonCallable := rebuildClosureCollectionNode(file, "notCallable", language, role.capped, field)
				nonCallable.Kind = graph.KindField
				nodes := []*graph.Node{both, nonCallable}
				for i := range raw - 3 {
					nodes = append(nodes,
						rebuildClosureCollectionNode(file, fmt.Sprintf("peer%02d", i), language, role.capped, field))
				}
				// The only opposite-role callable is also a self candidate. The
				// foreign Python row still counts, and the Go peer can pair at 8.
				py := rebuildClosureCollectionNode("foreign.py", "foreign", "python", role.capped, field)
				goPeer := rebuildClosureCollectionNode("peer.go", "peer", "go", role.capped, field)
				goPeer.Kind = graph.KindFunction
				proof, b, native := openRebuildObserverStores(t, map[string]string{py.ID: "elsewhere"},
					rebuildEventFacts(file, language, nodes),
					rebuildEventFacts(py.FilePath, py.Language, []*graph.Node{py}),
					rebuildEventFacts(goPeer.FilePath, goPeer.Language, []*graph.Node{goPeer}),
				)
				var wantCount int
				if raw == 8 {
					wantCount = 6
				}
				selection := rebuildClosureCollectionSelection(t)
				report := RunFrameworkSynthesizersWithSelection(native, selection)
				require.NoError(t, report.Err)
				want := rebuildEventSQLiteRows(native)
				require.Len(t, want.edges, wantCount)
				stats, err := RunRebuildClosureCollection(t.Context(), b, rebuildGRPCReceipts(proof...), selection)
				require.NoError(t, err)
				require.Equal(t, uint64(wantCount), stats.Edges)
				require.Equal(t, want, rebuildEventSavedRows(t, b))
				if raw > 8 {
					// Keep the live census, but omit witnesses from both
					// buckets. The over-cap field must not replay receipts.
					receipts := func(func(store_pebble.ParseFacts, error) bool) {
						t.Fatal("replayed receipts for an over-cap field")
					}
					stats, err := RunRebuildClosureCollection(t.Context(), b, receipts, selection)
					require.NoError(t, err)
					require.Zero(t, stats)
					require.Equal(t, want, rebuildEventSavedRows(t, b))
				}
			})
		}
	}
}

func TestRebuildClosureCollectionSkipsReceiptsWithoutRegistrars(t *testing.T) {
	for _, name := range []string{"no roles", "dispatcher only"} {
		t.Run(name, func(t *testing.T) {
			const file, language = "bus.swift", "swift"
			dispatch := rebuildClosureCollectionNode(file, "dispatch", language, rebuildCCDispatch, "listeners")
			if name == "no roles" {
				dispatch.Meta = nil
			}
			_, b, native := openRebuildObserverStores(t, nil, rebuildEventFacts(file, language, []*graph.Node{dispatch}))
			selection := rebuildClosureCollectionSelection(t)
			report := RunFrameworkSynthesizersWithSelection(native, selection)
			require.NoError(t, report.Err)
			require.Zero(t, report.Total)
			receipts := func(func(store_pebble.ParseFacts, error) bool) {
				t.Fatal("replayed receipts without a registrar")
			}
			stats, err := RunRebuildClosureCollection(t.Context(), b, receipts, selection)
			require.NoError(t, err)
			require.Zero(t, stats.Edges)
			require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildClosureCollectionIgnoresUnprovedRolesWithoutOutput(t *testing.T) {
	for _, tc := range []struct {
		name         string
		addRegistrar bool
	}{
		{name: "no new call"},
		{name: "one new call", addRegistrar: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const file, language, field = "bus.swift", "swift", "listeners"
			dispatch := rebuildClosureCollectionNode(file, "dispatch", language, rebuildCCDispatch, field)
			foreign := rebuildClosureCollectionNode("foreign.swift", "register", language, rebuildCCAppend, field)
			self := rebuildClosureCollectionNode("self.swift", "both", language, rebuildCCDispatch, "self")
			self.Meta[rebuildCCAppend] = "self"
			nodes := []*graph.Node{dispatch}
			var wantCalls int
			if tc.addRegistrar {
				nodes = append(nodes, rebuildClosureCollectionNode(file, "register", language, rebuildCCAppend, field))
				wantCalls = 1
			}
			proof, b, native := openRebuildObserverStores(t, map[string]string{foreign.ID: "elsewhere"},
				rebuildEventFacts(file, language, nodes),
				rebuildEventFacts(foreign.FilePath, language, []*graph.Node{foreign}),
				rebuildEventFacts(self.FilePath, language, []*graph.Node{self}),
			)
			selection := rebuildClosureCollectionSelection(t)
			require.NoError(t, RunFrameworkSynthesizersWithSelection(native, selection).Err)
			want := rebuildEventSQLiteRows(native)
			require.Len(t, want.edges, wantCalls)

			// Neither the cross-workspace registrar nor the sole self pair
			// contributes an endpoint to a new call.
			receipts := rebuildGRPCReceipts(proof[0])
			if !tc.addRegistrar {
				receipts = func(func(store_pebble.ParseFacts, error) bool) {
					t.Fatal("replayed receipts without a new call")
				}
			}
			stats, err := RunRebuildClosureCollection(t.Context(), b, receipts, selection)
			require.NoError(t, err)
			require.Equal(t, uint64(wantCalls), stats.Edges)
			require.Equal(t, want, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildClosureCollectionRefusesUnprovedNode(t *testing.T) {
	for _, mode := range []string{"complete", "missing", "changed field", "changed dispatch field", "absent versus nil", "nil versus absent", "absent versus empty", "absent versus non-string", "different non-strings"} {
		t.Run(mode, func(t *testing.T) {
			const earlyFile, lateFile, language = "early.swift", "late.swift", "swift"
			earlyDispatch := rebuildClosureCollectionNode(earlyFile, "dispatch", language, rebuildCCDispatch, "alpha")
			earlyRegister := rebuildClosureCollectionNode(earlyFile, "register", language, rebuildCCAppend, "alpha")
			lateDispatch := rebuildClosureCollectionNode(lateFile, "dispatch", language, rebuildCCDispatch, "zeta")
			lateRegister := rebuildClosureCollectionNode(lateFile, "register", language, rebuildCCAppend, "zeta")
			switch mode {
			case "different non-strings":
				lateRegister.Meta[rebuildCCDispatch] = []string{"zeta"}
			case "nil versus absent":
				lateRegister.Meta[rebuildCCDispatch] = nil
			}
			proof, b, native := openRebuildObserverStores(t, nil,
				rebuildEventFacts(earlyFile, language, []*graph.Node{earlyDispatch, earlyRegister}),
				rebuildEventFacts(lateFile, language, []*graph.Node{lateDispatch, lateRegister}),
			)
			before := rebuildEventSavedRows(t, b)
			selection := rebuildClosureCollectionSelection(t)
			require.NoError(t, RunFrameworkSynthesizersWithSelection(native, selection).Err)
			want := rebuildEventSQLiteRows(native)
			require.Len(t, want.edges, 2)
			// Change only the later pair's receipt. Equal native field
			// names still link; a missing or different field refuses.
			switch mode {
			case "missing":
				proof[1].Nodes = proof[1].Nodes[:1]
				proof[1].File.NodeCount = 1
			case "changed field":
				proof[1].Nodes[1].Meta[rebuildCCAppend] = "different"
			case "changed dispatch field":
				proof[1].Nodes[0].Meta[rebuildCCDispatch] = "different"
			case "absent versus nil":
				proof[1].Nodes[1].Meta[rebuildCCDispatch] = nil
			case "nil versus absent":
				_, present := proof[1].Nodes[1].Meta[rebuildCCDispatch]
				require.True(t, present)
				delete(proof[1].Nodes[1].Meta, rebuildCCDispatch)
			case "absent versus empty":
				proof[1].Nodes[1].Meta[rebuildCCDispatch] = ""
			case "absent versus non-string":
				proof[1].Nodes[1].Meta[rebuildCCDispatch] = true
			case "different non-strings":
				proof[1].Nodes[1].Meta[rebuildCCDispatch] = []string{"other"}
			}
			stats, err := RunRebuildClosureCollection(t.Context(), b, rebuildGRPCReceipts(proof...), selection)
			if mode != "missing" && mode != "changed field" && mode != "changed dispatch field" {
				require.NoError(t, err)
				require.Equal(t, uint64(2), stats.Edges)
				require.Equal(t, want, rebuildEventSavedRows(t, b))
				return
			}
			var refusal *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refusal)
			require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{
				Count: 1,
				First: graph.EdgeIdentityFor(closureCollectionEdge(lateDispatch, lateRegister, "zeta")),
			}, refusal)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
		})
	}
}

func rebuildClosureCollectionNode(file, name, language, role, field string) *graph.Node {
	n := rebuildEventNode(file, name, language, "")
	n.Kind = graph.KindMethod
	n.StartLine = 12
	n.Meta = map[string]any{role: field}
	return n
}

func rebuildClosureCollectionSelection(t *testing.T) FrameworkSynthesizerSelection {
	t.Helper()
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthClosureCollection})
	require.NoError(t, err)
	return selection
}

func TestRebuildClosureCollectionProvesFrozenAppleAdmission(t *testing.T) {
	for _, tc := range []struct {
		name     string
		claims   []string
		wantEdge bool
	}{
		{name: "exact", claims: []string{"exact"}, wantEdge: true},
		{name: "missing"},
		{name: "wrong-id", claims: []string{"wrong-id"}},
		{name: "wrong-node-file", claims: []string{"wrong-node-file"}},
		{name: "exact-then-wrong-language", claims: []string{"exact", "wrong-language"}},
		{name: "wrong-language-then-exact", claims: []string{"wrong-language", "exact"}},
		{name: "exact-then-wrong-file", claims: []string{"exact", "wrong-file"}},
		{name: "wrong-file-then-exact", claims: []string{"wrong-file", "exact"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const file, language, field = "plugin.go", "go", "handlers"
			dispatch := rebuildClosureCollectionNode(file, "dispatch", language, rebuildCCDispatch, field)
			register := rebuildClosureCollectionNode(file, "register", language, rebuildCCAppend, field)
			apple := rebuildEventNode("remote/support.swift", "support", "swift", "")
			// Only the out-of-workspace declaration admits the stage. It has
			// no closure role and is not an endpoint of the Go call.
			proof, b, native := openRebuildObserverStores(t, map[string]string{apple.ID: "remote"},
				rebuildEventFacts(file, language, []*graph.Node{dispatch, register}),
				rebuildEventFacts(apple.FilePath, apple.Language, []*graph.Node{apple}),
			)
			ctx := t.Context()
			selection := rebuildClosureCollectionSelection(t)
			frozen, err := FreezeRebuildFrameworkSelection(ctx, b.RebuildView(), selection)
			require.NoError(t, err)
			require.True(t, frozen.allows(SynthClosureCollection))
			before := rebuildEventSavedRows(t, b)
			receipts := proof[:1:1]
			for _, claim := range tc.claims {
				f := proof[1]
				n := *f.Nodes[0]
				switch claim {
				case "exact":
				case "wrong-id":
					n.ID = n.FilePath + "::different"
				case "wrong-language":
					// The claim is still Apple-family, but not the same language.
					n.Language = "objc"
					f.EffectiveLanguage = n.Language
				case "wrong-file", "wrong-node-file":
					n.FilePath = "remote/other.swift"
					if claim == "wrong-file" {
						f.File.FilePath = n.FilePath
					}
				}
				f.Nodes = []*graph.Node{&n}
				if claim == "wrong-node-file" {
					// The codec retains the node path independently of the receipt path.
					f = roundTripRebuildRNBridgeFacts(t, f)
				}
				receipts = append(receipts, f)
			}
			stats, err := RunRebuildClosureCollection(ctx, b, rebuildGRPCReceipts(receipts...), frozen)
			if !tc.wantEdge {
				var refusal *store_pebble.RebuildUnknownProvenanceError
				require.ErrorIs(t, err, store_pebble.ErrRebuildClosureCollection)
				require.ErrorAs(t, err, &refusal)
				require.EqualValues(t, 1, refusal.Count)
				require.Equal(t, graph.EdgeIdentityFor(closureCollectionEdge(dispatch, register, field)), refusal.First)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				return
			}

			require.NoError(t, err)
			require.Equal(t, uint64(1), stats.Edges)
			require.NoError(t, RunFrameworkSynthesizersWithSelection(native, selection).Err)
			want := rebuildEventSQLiteRows(native)
			require.Len(t, want.edges, 1)
			require.Equal(t, dispatch.ID, want.edges[0].From)
			require.Equal(t, register.ID, want.edges[0].To)
			require.Equal(t, want, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildClosureCollectionAppleMarkerNodeCap(t *testing.T) {
	const file, lang, field = "plugin.go", "go", "handlers"
	dispatch := rebuildClosureCollectionNode(file, "dispatch", lang, rebuildCCDispatch, field)
	register := rebuildClosureCollectionNode(file, "register", lang, rebuildCCAppend, field)
	marker := &graph.Node{ID: "a.swift", Kind: graph.KindFile, Language: "swift", FilePath: "a.swift"}
	spare := &graph.Node{ID: "b.swift", Kind: graph.KindFile, Language: "swift", FilePath: "b.swift"}
	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(file, lang, []*graph.Node{dispatch, register}),
		rebuildEventFacts(marker.FilePath, marker.Language, []*graph.Node{marker}),
		rebuildEventFacts(spare.FilePath, spare.Language, []*graph.Node{spare}),
	}
	for _, count := range []int{2, 3} {
		t.Run(fmt.Sprintf("%d receipts", count), func(t *testing.T) {
			b := openRebuildGRPCBuild(t, facts...)
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), rebuildClosureCollectionSelection(t))
			require.NoError(t, err)
			require.True(t, frozen.allows(SynthClosureCollection))
			large := *marker
			large.Meta = map[string]any{"padding": strings.Repeat("x", 16<<20)}
			b.AddNode(&large)
			stats, err := RunRebuildClosureCollection(t.Context(), b, rebuildGRPCReceipts(facts[:count]...), frozen)
			edges := rebuildEventSavedRows(t, b).edges
			if count == 2 {
				require.ErrorIs(t, err, store_pebble.ErrRebuildClosureCollection)
				require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
				require.ErrorContains(t, err, "Apple marker node")
				require.Zero(t, stats)
				require.Empty(t, edges)
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 1, stats.Edges)
			require.Equal(t, []*graph.Edge{closureCollectionEdge(dispatch, register, field)}, edges)
		})
	}
}

func TestRebuildClosureCollectionEarlyContractMasksRole(t *testing.T) {
	const file, language, field = "roles.swift", "swift", "handlers"
	dispatch := rebuildClosureCollectionNode(file, "dispatch", language, rebuildCCDispatch, field)
	registrar := rebuildClosureCollectionNode(file, "register", language, rebuildCCAppend, field)
	dispatch.ID = "env::DISPATCH"
	for _, tc := range []struct {
		name string
		ids  map[string]string
		want int
	}{
		{name: "built-empty", ids: map[string]string{}, want: 1},
		{name: "contract replaces dispatcher", ids: map[string]string{dispatch.ID: "settings.rb"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proof, b, native := openRebuildObserverStores(t, nil, rebuildEventFacts(file, language, []*graph.Node{dispatch, registrar}))
			for id, path := range tc.ids {
				require.NoError(t, native.AddBatchChecked([]*graph.Node{{
					ID: id, Name: id, Kind: graph.KindContract, FilePath: path, Language: "contract",
				}}, nil))
			}
			require.Equal(t, tc.want, ResolveClosureCollectionCalls(native))
			selection := rebuildClosureCollectionSelection(t).WithRebuildEarlyContractIDs(tc.ids)
			selection, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
			require.NoError(t, err)
			s, err := RunRebuildClosureCollection(t.Context(), b, rebuildGRPCReceipts(proof...), selection)
			require.NoError(t, err)
			require.Equal(t, uint64(tc.want), s.Edges)
			n, err := b.RebuildView().Node(t.Context(), dispatch.ID)
			require.NoError(t, err)
			require.Equal(t, graph.KindMethod, n.Kind)
		})
	}
}

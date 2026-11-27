package resolver

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildReactNativeBridgeSQLiteOracle(t *testing.T) {
	for _, tc := range []struct {
		name       string
		padding    int
		firstLine  int
		firstWrite uint64
	}{
		{"line 30 first, one byte", 1, 30, 2},
		{"line 30 first, one MiB", 1 << 20, 30, 2},
		{"line 4 first, one byte", 1, 4, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := "Calendar"
			if tc.padding > 1 {
				// Exercise a join key larger than 1 MiB.
				module += strings.Repeat("x", tc.padding)
			}
			facts := rebuildRNBridgeFacts(t, module)
			if tc.firstLine == 4 {
				edges := facts[0].Edges
				edges[0], edges[1] = edges[1], edges[0]
			}
			native := openRebuildTemporalOracle(t, facts...)
			private := openRebuildRNBridgePrivate(t, facts)
			before := rebuildEventSQLiteRows(native)
			require.Equal(t, before, rebuildEventSavedRows(t, private))

			app, android, ios, other := facts[0], facts[1], facts[2], facts[3]
			first, second, existing := app.Edges[0], app.Edges[1], app.Edges[2]
			require.Equal(t, tc.firstLine, first.Line)
			targets := []*graph.Node{ios.Nodes[0], other.Nodes[0]}
			if first.Line != existing.Line {
				targets = append(targets, android.Nodes[0])
			}
			var sites []int
			for e := range native.EdgesByKind(graph.EdgeCalls) {
				if e.From == first.From && e.To == first.To {
					sites = append(sites, e.Line)
				}
			}
			require.Equal(t, []int{first.Line, second.Line}, sites)
			selection, err := NewFrameworkSynthesizerSelection([]string{SynthReactNative})
			require.NoError(t, err)
			for _, wantNew := range []uint64{tc.firstWrite, 0} {
				report := RunFrameworkSynthesizersWithSelection(native, selection)
				require.NoError(t, report.Err)
				require.Equal(t, 3, report.Total) // Native counts the attempt on the existing key too.
				stats, err := RunRebuildReactNativeBridge(t.Context(), private, rebuildGRPCReceipts(facts...), selection)
				require.NoError(t, err)
				require.Equal(t, wantNew, stats.Edges)

				want := rebuildEventSQLiteRows(native)
				require.Equal(t, before.nodes, want.nodes)
				require.Len(t, want.edges, len(before.edges)+int(tc.firstWrite))
				require.Contains(t, want.edges, existing)
				for _, n := range targets {
					require.Contains(t, want.edges, &graph.Edge{
						From:            first.From,
						To:              n.ID,
						Kind:            graph.EdgeCalls,
						FilePath:        first.FilePath,
						Line:            first.Line,
						Confidence:      0.6,
						ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeCalls, 0.6),
						Origin:          graph.OriginASTInferred,
						Meta: map[string]any{
							"via":             "rn.bridge",
							"rn_module":       module,
							"rn_method":       "open",
							"native_language": n.Language,
							MetaSynthesizedBy: SynthReactNative,
							MetaProvenance:    ProvenanceHeuristic,
						},
					})
				}
				require.Equal(t, want, rebuildEventSavedRows(t, private))
			}
		})
	}
}

func TestRebuildReactNativeBridgeRejectsMissingFirstSite(t *testing.T) {
	facts := rebuildRNBridgeFacts(t, "Calendar")
	private := openRebuildRNBridgePrivate(t, facts)
	before := rebuildEventSavedRows(t, private)
	// The pending graph still has the winner. The submitted portable receipt
	// still has a valid second site, which must not be promoted to winner.
	proof := slices.Clone(facts)
	proof[0].Edges = proof[0].Edges[1:]
	proof[0] = roundTripRebuildRNBridgeFacts(t, proof[0])

	selection, err := NewFrameworkSynthesizerSelection([]string{SynthReactNative})
	require.NoError(t, err)
	stats, err := RunRebuildReactNativeBridge(t.Context(), private, rebuildGRPCReceipts(proof...), selection)
	var refused *store_pebble.RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &refused)
	// Missing order affects all three pairs before election. The existing
	// Android row still consumes its pair but would not be rewritten.
	require.Equal(t, uint64(3), refused.Count)
	require.Equal(t, graph.EdgeIdentityFor(facts[0].Edges[2]), refused.First)
	require.Zero(t, stats)
	require.Equal(t, before, rebuildEventSavedRows(t, private))
}

func TestRebuildReactNativeBridgeDuplicateRecorder(t *testing.T) {
	for _, tc := range []struct {
		name, field, value string
		wantCount          uint64
	}{
		{name: "matching"},
		{"conflicting via", "via", "not-rn", 3},
		{"missing via", "via", "", 3},
		{"conflicting module", "rn_module", "Contacts", 3},
		{"missing module", "rn_module", "", 3},
		// The conflicting receipt also names the existing Calendar.close bucket.
		{"conflicting method", "rn_method", "close", 4},
		{"missing method", "rn_method", "", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := rebuildRNBridgeFacts(t, "Calendar")
			private := openRebuildRNBridgePrivate(t, facts)
			before := rebuildEventSavedRows(t, private)
			// Change only the portable receipt: the live row has the first payload.
			proof := slices.Clone(facts)
			duplicate := *facts[0].Edges[0]
			duplicate.Meta = maps.Clone(duplicate.Meta)
			if tc.field != "" {
				if tc.value == "" {
					delete(duplicate.Meta, tc.field)
				} else {
					duplicate.Meta[tc.field] = tc.value
				}
			}
			proof[0].Edges = slices.Insert(slices.Clone(proof[0].Edges), 1, &duplicate)
			proof[0] = roundTripRebuildRNBridgeFacts(t, proof[0])

			selection, err := NewFrameworkSynthesizerSelection([]string{SynthReactNative})
			require.NoError(t, err)
			stats, err := RunRebuildReactNativeBridge(t.Context(), private, rebuildGRPCReceipts(proof...), selection)
			if tc.wantCount != 0 {
				var refused *store_pebble.RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refused)
				require.Equal(t, tc.wantCount, refused.Count)
				require.Equal(t, graph.EdgeIdentityFor(facts[0].Edges[2]), refused.First)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildEventSavedRows(t, private))
				return
			}
			require.NoError(t, err)
			require.Equal(t, uint64(2), stats.Edges)
			native := openRebuildTemporalOracle(t, facts...)
			report := RunFrameworkSynthesizersWithSelection(native, selection)
			require.NoError(t, report.Err)
			require.Equal(t, 3, report.Total)
			require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, private))
		})
	}
}

func TestRebuildReactNativeBridgeRejectsOverwrittenMarker(t *testing.T) {
	for _, tc := range []struct {
		name, field, value string
	}{
		{"not a bridge call", "via", "other"},
		{"missing module", "rn_module", ""},
		{"missing method", "rn_method", ""},
		{"no module bucket", "rn_module", "Contacts"},
		{"no method bucket", "rn_method", "delete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := rebuildRNBridgeFacts(t, "Calendar")
			marker := facts[0].Edges[0]
			last := *marker
			last.Meta = maps.Clone(marker.Meta)
			if tc.value == "" {
				delete(last.Meta, tc.field)
			} else {
				last.Meta[tc.field] = tc.value
			}
			// No second valid site can hide the absent live marker. Both
			// stores consume the same receipt in the same insertion order.
			facts[0].Edges = []*graph.Edge{marker, &last}
			facts[0] = roundTripRebuildRNBridgeFacts(t, facts[0])
			private, native := openRebuildEventStores(t, facts...)
			before := rebuildEventSavedRows(t, private)
			require.Equal(t, []*graph.Edge{&last}, before.edges)
			require.Equal(t, []*graph.Edge{marker}, rebuildEventSQLiteRows(native).edges)

			selection, err := NewFrameworkSynthesizerSelection([]string{SynthReactNative})
			require.NoError(t, err)
			report := RunFrameworkSynthesizersWithSelection(native, selection)
			require.NoError(t, report.Err)
			require.Equal(t, 3, report.Total)
			stats, err := RunRebuildReactNativeBridge(t.Context(), private, rebuildGRPCReceipts(facts...), selection)
			var refused *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			require.ErrorIs(t, err, store_pebble.ErrRebuildReactNativeBridge)
			require.Equal(t, uint64(3), refused.Count)
			first := graph.EdgeIdentityFor(marker)
			first.To = facts[1].Nodes[0].ID
			require.Equal(t, first, refused.First)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, private))
		})
	}
}

func TestRebuildNativeModuleBridgeHTMLCaller(t *testing.T) {
	const page = "page.html"
	parsed, err := languages.NewHTMLExtractor().Extract(page, []byte(`<script>
function show() {
    NativeModules.Calendar.open();
}
</script>`))
	require.NoError(t, err)
	var markers []*graph.Edge
	for _, e := range parsed.Edges {
		if e.Kind == graph.EdgeCalls && e.Meta["via"] == rnNativeVia {
			markers = append(markers, e)
		}
	}
	require.Len(t, markers, 1)
	marker := markers[0]
	require.Equal(t, page, marker.FilePath)
	require.Equal(t, "Calendar", marker.Meta["rn_module"])
	require.Equal(t, "open", marker.Meta["rn_method"])
	index := slices.IndexFunc(parsed.Nodes, func(n *graph.Node) bool {
		return n.ID == marker.From
	})
	require.GreaterOrEqual(t, index, 0)
	caller := parsed.Nodes[index]
	require.Equal(t, page, caller.FilePath)
	require.Equal(t, "javascript", caller.Language)
	require.Equal(t, true, caller.Meta["inline_script"])

	for _, tc := range []struct {
		synth, file, language, metaPrefix string
		stage                             error
	}{
		{SynthReactNative, "Calendar.java", "java", "rn", store_pebble.ErrRebuildReactNativeBridge},
		{SynthExpoModules, "Calendar.kt", "kotlin", "expo", store_pebble.ErrRebuildExpoModuleBridge},
	} {
		t.Run(tc.synth, func(t *testing.T) {
			target := &graph.Node{
				ID:       tc.file + "::open",
				Name:     "open",
				Kind:     graph.KindMethod,
				Language: tc.language,
				FilePath: tc.file,
				Meta:     map[string]any{tc.metaPrefix + "_module": "Calendar", tc.metaPrefix + "_method": "open"},
			}
			selection, err := NewFrameworkSynthesizerSelection([]string{tc.synth})
			require.NoError(t, err)
			run := RunRebuildReactNativeBridge
			if tc.synth == SynthExpoModules {
				run = RunRebuildExpoModuleBridge
			}
			for _, liveLanguage := range []string{"javascript", "typescript"} {
				t.Run(liveLanguage, func(t *testing.T) {
					receipts, private, native := openRebuildObserverStores(t, nil,
						rebuildEventFacts(page, "html", []*graph.Node{caller}, marker),
						rebuildEventFacts(tc.file, tc.language, []*graph.Node{target}),
					)
					if liveLanguage != caller.Language {
						changed := *caller
						changed.Language = liveLanguage
						private.AddNode(&changed)
					}
					before := rebuildEventSavedRows(t, private)
					stats, err := run(t.Context(), private, rebuildGRPCReceipts(receipts...), selection)
					if liveLanguage != caller.Language {
						require.ErrorIs(t, err, tc.stage)
						require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
						var refusal *store_pebble.RebuildUnknownProvenanceError
						require.ErrorAs(t, err, &refusal)
						first := graph.EdgeIdentityFor(marker)
						first.To = target.ID
						require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: first}, refusal)
						require.Zero(t, stats)
						require.Equal(t, before, rebuildEventSavedRows(t, private))
						return
					}
					require.NoError(t, err)
					require.Equal(t, uint64(1), stats.Edges)
					report := RunFrameworkSynthesizersWithSelection(native, selection)
					require.NoError(t, report.Err)
					require.Equal(t, 1, report.Total)
					require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, private))
				})
			}
		})
	}
}

func TestRebuildReactNativeBridgeRejectsUnprovedEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name  string
		file  int
		alter func(*store_pebble.ParseFacts)
		count uint64
	}{
		{"missing caller", 0, func(f *store_pebble.ParseFacts) {
			f.Nodes, f.File.NodeCount = nil, 0
		}, 2},
		{"wrong caller name", 0, func(f *store_pebble.ParseFacts) { f.Nodes[0].Name = "other" }, 2},
		{"wrong iOS file", 2, func(f *store_pebble.ParseFacts) { f.File.FilePath = "Other.m" }, 1},
		{"missing iOS declaration", 2, func(f *store_pebble.ParseFacts) {
			f.Nodes, f.File.NodeCount = f.Nodes[1:], 1
		}, 1},
		{"empty iOS extractor", 2, func(f *store_pebble.ParseFacts) { f.EffectiveLanguage = "" }, 1},
		{"wrong iOS node language", 2, func(f *store_pebble.ParseFacts) { f.Nodes[0].Language = "swift" }, 1},
		{"wrong iOS kind", 2, func(f *store_pebble.ParseFacts) { f.Nodes[0].Kind = graph.KindFunction }, 1},
		{"wrong iOS module", 2, func(f *store_pebble.ParseFacts) { f.Nodes[0].Meta["rn_module"] = "Contacts" }, 1},
		{"wrong iOS method", 2, func(f *store_pebble.ParseFacts) { f.Nodes[0].Meta["rn_method"] = "close" }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := rebuildRNBridgeFacts(t, "Calendar")
			private := openRebuildRNBridgePrivate(t, facts)
			before := rebuildEventSavedRows(t, private)
			wantFirst := graph.EdgeIdentityFor(facts[0].Edges[0])
			wantFirst.To = facts[2].Nodes[0].ID
			// All marker proofs remain valid. Only the endpoint receipt differs.
			proof := roundTripRebuildRNBridgeFacts(t, facts[tc.file])
			tc.alter(&proof)
			facts[tc.file] = roundTripRebuildRNBridgeFacts(t, proof)

			selection, err := NewFrameworkSynthesizerSelection([]string{SynthReactNative})
			require.NoError(t, err)
			stats, err := RunRebuildReactNativeBridge(t.Context(), private, rebuildGRPCReceipts(facts...), selection)
			var refused *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, tc.count, refused.Count)
			require.Equal(t, wantFirst, refused.First)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, private))
		})
	}
}

func TestRebuildReactNativeBridgeRejectsChangedLiveCaller(t *testing.T) {
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthReactNative})
	require.NoError(t, err)
	for _, tc := range []struct {
		name  string
		alter func(*graph.Node)
	}{
		{"FilePath", func(n *graph.Node) { n.FilePath = "other/app.ts" }},
		{"Language", func(n *graph.Node) { n.Language = "javascript" }},
		{"RepoPrefix", func(n *graph.Node) { n.RepoPrefix = "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := rebuildRNBridgeFacts(t, "Calendar")
			private := openRebuildRNBridgePrivate(t, facts)
			live := *facts[0].Nodes[0]
			tc.alter(&live)
			private.AddNode(&live)
			stored, err := private.RebuildView().Node(t.Context(), live.ID)
			require.NoError(t, err)
			require.Equal(t, &live, stored)
			before := rebuildEventSavedRows(t, private)

			stats, err := RunRebuildReactNativeBridge(t.Context(), private, rebuildGRPCReceipts(facts...), selection)
			var refused *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, uint64(2), refused.Count)
			first := graph.EdgeIdentityFor(facts[0].Edges[0])
			first.To = facts[2].Nodes[0].ID
			require.Equal(t, first, refused.First)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, private))
		})
	}
}

func TestRebuildReactNativeBridgeRejectsConflictingLinks(t *testing.T) {
	facts := rebuildRNBridgeFacts(t, "Calendar")
	private := openRebuildRNBridgePrivate(t, facts)
	ctx := t.Context()
	raw := facts[0].Edges[0]
	for _, to := range []string{"unresolved::alias1", "unresolved::alias2"} {
		// Reuse the exact raw key, leaving two live moves that claim its ordinal.
		private.AddEdge(raw)
		live := *raw
		live.To = to
		require.NoError(t, private.ApplyLinkReindexes(ctx, []graph.EdgeReindex{{Edge: &live, OldTo: raw.To}}))
	}
	before := rebuildEventSavedRows(t, private)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthReactNative})
	require.NoError(t, err)
	stats, err := RunRebuildReactNativeBridge(ctx, private, rebuildGRPCReceipts(facts...), selection)
	var refused *store_pebble.RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, uint64(3), refused.Count)
	require.Equal(t, graph.EdgeIdentityFor(facts[0].Edges[2]), refused.First)
	require.Zero(t, stats)
	require.Equal(t, before, rebuildEventSavedRows(t, private))
}

func TestRebuildReactNativeBridgeEligibility(t *testing.T) {
	for _, tc := range []struct {
		name, language, module string
		kind                   graph.EdgeKind
		disabled               bool
		want                   int
	}{
		{"RN unselected", "typescript", "Calendar", graph.EdgeCalls, true, 0},
		{"no admitted family", "go", "Calendar", graph.EdgeCalls, false, 0},
		{"web only", "typescript", "Calendar", graph.EdgeCalls, false, 1},
		{"Apple only", "objc", "Calendar", graph.EdgeCalls, false, 1},
		{"JVM only", "java", "Calendar", graph.EdgeCalls, false, 1},
		{"non-Calls", "typescript", "Calendar", graph.EdgeReferences, false, 0},
		{"unmatched module", "typescript", "Contacts", graph.EdgeCalls, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := rebuildRNBridgeFacts(t, "Calendar")
			app := facts[0]
			app.Nodes[0].Language = tc.language
			app.EffectiveLanguage = tc.language
			call := app.Edges[0]
			call.Kind = tc.kind
			call.Meta["rn_module"] = tc.module
			app.Edges = app.Edges[:1]
			// Keep only the caller's family and an otherwise eligible Go Method.
			facts = []store_pebble.ParseFacts{roundTripRebuildRNBridgeFacts(t, app), facts[3]}
			private, native := openRebuildEventStores(t, facts...)
			before := rebuildEventSavedRows(t, private)
			require.Equal(t, before, rebuildEventSQLiteRows(native))
			names := []string{SynthReactNative}
			if tc.disabled {
				names = nil
			}
			selection, err := NewFrameworkSynthesizerSelection(names)
			require.NoError(t, err)
			report := RunFrameworkSynthesizersWithSelection(native, selection)
			require.NoError(t, report.Err)
			require.Equal(t, tc.want, report.Total)
			want := rebuildEventSQLiteRows(native)
			require.Len(t, want.edges, len(before.edges)+tc.want)
			stats, err := RunRebuildReactNativeBridge(t.Context(), private, rebuildGRPCReceipts(facts...), selection)
			require.NoError(t, err)
			require.Equal(t, uint64(tc.want), stats.Edges)
			require.Equal(t, want, rebuildEventSavedRows(t, private))
		})
	}
}

func TestRebuildReactNativeBridgeSkipsSelfCall(t *testing.T) {
	for _, tc := range []struct {
		name      string
		wantPeer  bool
		wantReads int
	}{
		{name: "sole candidate needs no recorder", wantReads: 1},
		{name: "distinct candidate still links", wantPeer: true, wantReads: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := rebuildRNBridgeFacts(t, "Calendar")
			app, ios := facts[0], facts[2]
			app.Nodes[0].Kind = graph.KindMethod
			app.Nodes[0].Meta = maps.Clone(ios.Nodes[0].Meta)
			app.Edges = app.Edges[:1]
			facts = []store_pebble.ParseFacts{roundTripRebuildRNBridgeFacts(t, app)}
			if tc.wantPeer {
				facts = append(facts, ios)
			}
			private, native := openRebuildEventStores(t, facts...)
			before := rebuildEventSavedRows(t, private)
			selection, err := NewFrameworkSynthesizerSelection([]string{SynthReactNative})
			require.NoError(t, err)
			report := RunFrameworkSynthesizersWithSelection(native, selection)
			require.NoError(t, report.Err)
			want := rebuildEventSQLiteRows(native)
			var wantEdges uint64
			if tc.wantPeer {
				require.Equal(t, 1, report.Total)
				linked := graph.EdgeIdentityFor(app.Edges[0])
				linked.To = ios.Nodes[0].ID
				require.Len(t, want.edges, len(before.edges)+1)
				require.True(t, slices.ContainsFunc(want.edges, func(e *graph.Edge) bool {
					return graph.EdgeIdentityFor(e) == linked
				}))
				wantEdges = 1
			} else {
				require.Zero(t, report.Total)
				// The marker is live but lacks any ordinal in the submitted proof.
				facts[0].Edges = nil
				require.Equal(t, before, want)
			}
			reads := 0
			receipts := func(yield func(store_pebble.ParseFacts, error) bool) {
				reads++
				rebuildGRPCReceipts(facts...)(yield)
			}
			stats, err := RunRebuildReactNativeBridge(t.Context(), private, receipts, selection)
			require.NoError(t, err)
			// Order still scans for hidden markers; only a new edge needs endpoint proof.
			require.Equal(t, tc.wantReads, reads)
			require.Equal(t, wantEdges, stats.Edges)
			require.Equal(t, want, rebuildEventSavedRows(t, private))
		})
	}
}

func rebuildRNBridgeFacts(t *testing.T, module string) []store_pebble.ParseFacts {
	t.Helper()
	const app, android, ios, other = "app.ts", "Calendar.java", "Calendar.m", "calendar.go"
	const caller, method = app + "::show", "open"
	placeholder := "unresolved::rn::" + module + "::" + method
	call := &graph.Edge{
		From: caller, To: placeholder, Kind: graph.EdgeCalls, FilePath: app, Line: 30,
		Meta: map[string]any{"via": "rn.native", "rn_module": module, "rn_method": method},
	}
	second := *call
	second.Line = 4
	noModule := *call
	noModule.To = "unresolved::rn::::" + method
	noModule.Line = 2
	noModule.Meta = map[string]any{"via": "rn.native", "rn_module": "", "rn_method": method}

	nativeMethod := func(file, language, name, rnMethod string) *graph.Node {
		return &graph.Node{
			ID: file + "::" + name, Name: name, Kind: graph.KindMethod, Language: language, FilePath: file,
			Meta: map[string]any{"rn_module": module, "rn_method": rnMethod},
		}
	}
	androidMethod := nativeMethod(android, "java", method, method)
	ignoredKind := nativeMethod(other, "go", "openFunction", method)
	ignoredKind.Kind = graph.KindFunction
	existing := &graph.Edge{
		From: caller, To: androidMethod.ID, Kind: graph.EdgeCalls, FilePath: app, Line: call.Line,
		Confidence: 0.9, Origin: graph.OriginASTInferred, Meta: map[string]any{"fixture": "durable"},
	}
	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(app, "typescript", []*graph.Node{
			{ID: caller, Name: "show", Kind: graph.KindFunction, Language: "typescript", FilePath: app},
		}, call, &second, existing, &noModule),
		rebuildEventFacts(android, "java", []*graph.Node{androidMethod}),
		rebuildEventFacts(ios, "objc", []*graph.Node{nativeMethod(ios, "objc", method, method), nativeMethod(ios, "objc", "close", "close")}),
		rebuildEventFacts(other, "go", []*graph.Node{nativeMethod(other, "go", method, method), ignoredKind}),
	}
	for i := range facts {
		facts[i] = roundTripRebuildRNBridgeFacts(t, facts[i])
	}
	return facts
}

func openRebuildRNBridgePrivate(t *testing.T, receipts []store_pebble.ParseFacts) *store_pebble.BuildStore {
	t.Helper()
	// SQLite consumes the frozen receipt order. Reverse private insertion
	// as a control; complete keys also sort the lower line first. Neither
	// private order may select the bridge site.
	pending := slices.Clone(receipts)
	pending[0].Edges = slices.Clone(pending[0].Edges)
	slices.Reverse(pending[0].Edges)
	return openRebuildGRPCBuild(t, pending...)
}

func roundTripRebuildRNBridgeFacts(t *testing.T, facts store_pebble.ParseFacts) store_pebble.ParseFacts {
	t.Helper()
	encoded, err := store_pebble.EncodeParseFacts(facts)
	require.NoError(t, err)
	decoded, err := store_pebble.DecodeParseFacts(encoded)
	require.NoError(t, err)
	return decoded
}

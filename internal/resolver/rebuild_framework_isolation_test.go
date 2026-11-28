package resolver

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildFrameworkIsolationBridge(t *testing.T) {
	for _, tc := range []struct {
		name, pass, deferred, module, method, via string
		kind                                      graph.NodeKind
		disabled, heldCall, gateOnly              bool
		want                                      string
	}{
		{name: "RN actual held method", pass: SynthReactNative, deferred: "java", module: "Calendar", method: "create", via: rnNativeVia, kind: graph.KindMethod, want: "held-endpoint"},
		{name: "Expo pruned frozen gate", pass: SynthExpoModules, deferred: "swift", module: "Calendar", method: "create", via: rnNativeVia, kind: graph.KindMethod, want: "held-endpoint"},
		{name: "wrong method", pass: SynthReactNative, deferred: "java", module: "Calendar", method: "other", via: rnNativeVia, kind: graph.KindMethod},
		{name: "wrong module", pass: SynthExpoModules, deferred: "swift", module: "Other", method: "create", via: rnNativeVia, kind: graph.KindMethod},
		{name: "wrong kind", pass: SynthReactNative, deferred: "java", module: "Calendar", method: "create", via: rnNativeVia, kind: graph.KindFunction},
		{name: "unrelated via", pass: SynthReactNative, deferred: "java", module: "Calendar", method: "create", via: "other", kind: graph.KindMethod},
		{name: "disabled", pass: SynthReactNative, deferred: "java", module: "Calendar", method: "create", via: rnNativeVia, kind: graph.KindMethod, disabled: true},
		{name: "no held inputs", pass: SynthReactNative, deferred: "typescript", module: "Calendar", method: "create", via: rnNativeVia, kind: graph.KindMethod},
		{name: "held marker reaches selected method", pass: SynthReactNative, deferred: "java", module: "Calendar", method: "create", via: rnNativeVia, kind: graph.KindMethod, heldCall: true, want: "rn-bridge"},
		{name: "held family alone admits Expo", pass: SynthExpoModules, deferred: "swift", module: "Calendar", method: "create", via: rnNativeVia, kind: graph.KindMethod, gateOnly: true, want: "expo-bridge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caller := &graph.Node{ID: "caller.ts::run", Name: "run", FilePath: "caller.ts", Language: "typescript", Kind: graph.KindFunction}
			method := &graph.Node{ID: "native::create", Name: "create", FilePath: "native.txt", Language: tc.deferred, Kind: tc.kind,
				Meta: map[string]any{"rn_module": tc.module, "rn_method": tc.method, "expo_module": tc.module, "expo_method": tc.method}}
			recorder, recorderLanguage := "record.ts", "typescript"
			methodLanguage := tc.deferred
			if tc.gateOnly || tc.heldCall {
				method.Language, methodLanguage = "typescript", "typescript"
			}
			if tc.heldCall {
				recorder, recorderLanguage = "record.java", tc.deferred
			}
			marker := &graph.Edge{From: caller.ID, To: "unresolved::Calendar.create", Kind: graph.EdgeCalls, FilePath: recorder, Line: 12,
				Meta: map[string]any{"via": tc.via, "rn_module": "Calendar", "rn_method": "create"}}
			second := *marker
			second.Line = 3 // receipt order, not the lowest line, elects the first site
			facts := []store_pebble.ParseFacts{
				rebuildEventFacts(caller.FilePath, "typescript", []*graph.Node{caller}),
				rebuildEventFacts(recorder, recorderLanguage, nil, marker, &second),
				rebuildEventFacts(method.FilePath, methodLanguage, []*graph.Node{method}),
			}
			if tc.gateOnly {
				n := &graph.Node{ID: "family.swift::Tag", Name: "Tag", FilePath: "family.swift", Language: "swift", Kind: graph.KindType}
				facts = append(facts, rebuildEventFacts(n.FilePath, tc.deferred, []*graph.Node{n}))
			}
			var want []store_pebble.RebuildIsolationAffectedScope
			if tc.want != "" {
				want = append(want, frameworkIsolationScope(caller.FilePath, tc.deferred, tc.want, 1))
				if tc.heldCall {
					want = append(want, frameworkIsolationScope(caller.FilePath, tc.deferred, "held-endpoint", 2))
				}
				if tc.heldCall || tc.gateOnly {
					want = append(want, frameworkIsolationScope(method.FilePath, tc.deferred, tc.want, 1))
				}
				if !tc.heldCall {
					want = append(want, frameworkIsolationScope(recorder, tc.deferred, tc.want, 1))
				}
			}
			// The native oracle proves the selected-call/held-method target and
			// first-site ordering without putting held rows into the private pass.
			if tc.want == "held-endpoint" {
				native := openRebuildTemporalOracle(t, facts...)
				selection, err := NewFrameworkSynthesizerSelection([]string{tc.pass})
				require.NoError(t, err)
				report := RunFrameworkSynthesizersWithSelection(native, selection)
				require.NoError(t, report.Err)
				var lines []int
				for e := range native.EdgesByKind(graph.EdgeCalls) {
					if e.From == caller.ID && e.To == method.ID {
						lines = append(lines, e.Line)
					}
				}
				require.Equal(t, []int{12}, lines)
			}
			assertFrameworkIsolation(t, tc.pass, tc.disabled, facts, nil, want)
		})
	}
}

func TestRebuildFrameworkIsolationFabric(t *testing.T) {
	for _, tc := range []struct {
		name, component                      string
		selectedNative, heldNative, gateOnly bool
		kind                                 graph.NodeKind
		want                                 uint64
	}{
		{name: "selected spec", component: "RCTView", heldNative: true, kind: graph.KindType, want: 2},
		{name: "selected manager", component: "RNView", selectedNative: true, kind: graph.KindType, want: 2},
		{name: "wrong component", component: "Other", heldNative: true, kind: graph.KindType},
		{name: "same role", component: "View", kind: graph.KindType},
		{name: "wrong kind", component: "View", heldNative: true, kind: graph.KindMethod},
		{name: "held family admits selected pair", component: "View", heldNative: true, gateOnly: true, kind: graph.KindType, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selected := &graph.Node{ID: "view.ts::View", Name: "View", FilePath: "view.ts", Language: "typescript", Kind: graph.KindType, StartLine: 2,
				Meta: map[string]any{"fabric_component": "View"}}
			held := &graph.Node{ID: "native.txt::View", Name: "View", FilePath: "native.txt", Language: "java", Kind: tc.kind, StartLine: 4,
				Meta: map[string]any{"fabric_component": tc.component}}
			if tc.selectedNative {
				selected.Meta["fabric_native"] = false
			}
			if tc.heldNative {
				held.Meta["fabric_native"] = false
			}
			language := "java"
			if tc.gateOnly {
				held.Language, language = "typescript", "typescript"
			}
			facts := []store_pebble.ParseFacts{rebuildEventFacts(selected.FilePath, "typescript", []*graph.Node{selected}), rebuildEventFacts(held.FilePath, language, []*graph.Node{held})}
			var want []store_pebble.RebuildIsolationAffectedScope
			if tc.gateOnly {
				family := &graph.Node{ID: "Family.java::Tag", Name: "Tag", FilePath: "Family.java", Kind: graph.KindType, Language: "java"}
				facts = append(facts, rebuildEventFacts(family.FilePath, "java", []*graph.Node{family}))
				want = append(want, frameworkIsolationScope(held.FilePath, "java", "fabric", 1), frameworkIsolationScope(selected.FilePath, "java", "fabric", 1))
			} else if tc.want != 0 {
				want = append(want, frameworkIsolationScope(selected.FilePath, "java", "held-endpoint", tc.want))
			}
			assertFrameworkIsolation(t, SynthFabric, false, facts, nil, want)
		})
	}
}

func TestRebuildFrameworkIsolationSvelte(t *testing.T) {
	for _, tc := range []struct {
		name, skipPath                   string
		load, differentWorkspace, parsed bool
		want                             string
	}{
		{name: "checked skip", skipPath: "routes/+page.server.ts", want: "held-endpoint"},
		{name: "different route", skipPath: "other/+page.server.ts"},
		{name: "unrelated basename", skipPath: "routes/server.ts"},
		{name: "load wins", skipPath: "routes/+page.server.ts", load: true},
		{name: "wrong workspace", skipPath: "routes/+page.server.ts", differentWorkspace: true},
		{name: "parsed server role", skipPath: "routes/+page.server.ts", parsed: true, want: "sveltekit-load"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := &graph.Node{ID: "routes/+page.ts::Page", Name: "Page", FilePath: "routes/+page.ts", Language: "typescript", Kind: graph.KindType,
				StartLine: 1, Meta: map[string]any{"component": true}}
			server := &graph.Node{ID: tc.skipPath, FilePath: tc.skipPath, Name: tc.skipPath, Language: "json", Kind: graph.KindFile}
			if tc.differentWorkspace {
				server.WorkspaceID = "other"
			}
			facts := []store_pebble.ParseFacts{rebuildEventFacts(page.FilePath, "typescript", []*graph.Node{page})}
			if tc.load {
				load := &graph.Node{ID: "routes/+layout.server.ts::load", Name: "load", FilePath: "routes/+layout.server.ts", Language: "typescript", Kind: graph.KindFunction}
				facts = append(facts, rebuildEventFacts(load.FilePath, "typescript", []*graph.Node{load}))
			}
			var skip *graph.Node
			if tc.parsed {
				facts = append(facts, rebuildEventFacts(server.FilePath, "json", []*graph.Node{server}))
			} else {
				skip = server
			}
			var want []store_pebble.RebuildIsolationAffectedScope
			if tc.want != "" {
				want = append(want, frameworkIsolationScope(page.FilePath, "json", tc.want, 1))
			}
			assertFrameworkIsolation(t, SynthSvelteKitLoad, false, facts, skip, want)
		})
	}
}

func frameworkIsolationScope(file, deferred, cause string, count uint64) store_pebble.RebuildIsolationAffectedScope {
	return store_pebble.RebuildIsolationAffectedScope{FilePath: file, SupportedLanguage: "typescript", DeferredLanguage: deferred, Cause: cause, Count: count}
}

// Assert the sealed scopes, not just the marker callback. The framework
// observer must leave the live graph byte-equivalent on every input and replay.
func assertFrameworkIsolation(t *testing.T, pass string, disabled bool, facts []store_pebble.ParseFacts,
	skip *graph.Node, want []store_pebble.RebuildIsolationAffectedScope,
) {
	t.Helper()
	ctx := t.Context()
	b, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "build"), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	const incarnation = "framework-isolation"
	head, err := store_pebble.OpenHead(filepath.Join(t.TempDir(), "head"), incarnation, store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, head.Close()) })
	var selected []store_pebble.ParseFacts
	var manifestFiles []store_pebble.ManifestFile
	var deferred []string
	languages := make(map[string]string)
	for _, f := range facts {
		file, language := f.File.FilePath, f.EffectiveLanguage
		languages[file] = language
		manifestFiles = append(manifestFiles, store_pebble.ManifestFile{Path: file, Language: language, Kind: 'f'})
		keep := func(string) (bool, error) { return true, nil }
		if store_pebble.RebuildLinkedExtractor(language) {
			require.NoError(t, b.AddParsedFileSelected("", f, keep, true))
			selected = append(selected, f)
		} else {
			require.NoError(t, b.AddRebuildDeferredFile("", f, keep))
			deferred = append(deferred, language)
		}
	}
	if skip != nil {
		require.NoError(t, b.AddRebuildHeldSkip(skip))
		languages[skip.FilePath] = skip.Language
		deferred = append(deferred, skip.Language)
	}
	slices.Sort(deferred)
	deferred = slices.Compact(deferred)
	manifest, err := store_pebble.PrepareManifest(ctx, t.TempDir(), manifestFiles, store_pebble.Witness{
		Stage: store_pebble.StageManifest, Profile: store_pebble.FullProfile, Input: store_pebble.HeadDigest{1}, Recipe: store_pebble.HeadDigest{2},
	}, incarnation)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, manifest)
	require.NoError(t, err)
	fileGraph, err := b.PrepareFileGraph(ctx, t.TempDir(), manifest.Witness, incarnation)
	require.NoError(t, err)
	base, err := b.RetainFileGraph(ctx, fileGraph)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, fileGraph)
	require.NoError(t, err)
	require.NoError(t, b.IsolateRebuildLinks(ctx))
	requestedPass := pass
	if disabled {
		requestedPass = SynthGRPCStub
	}
	requested, err := NewFrameworkSynthesizerSelection([]string{requestedPass})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(ctx, b.RebuildView(), requested)
	require.NoError(t, err)
	before := rebuildEventSavedRows(t, b)
	for range 2 {
		require.NoError(t, MarkRebuildFrameworkIsolation(ctx, b, requested, frozen, pass, func(file string) string { return languages[file] }, rebuildGRPCReceipts(selected...)))
		require.Equal(t, before, rebuildEventSavedRows(t, b))
	}
	require.NoError(t, b.RestoreRebuildDeclarations(ctx))
	require.NoError(t, b.CheckRebuildIsolationCoverage(deferred))
	relations, err := b.PrepareRelations(ctx, t.TempDir(), base, nil)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, relations)
	require.NoError(t, err)
	snap := head.OpenSnapshot()
	defer func() { require.NoError(t, snap.Close()) }()
	err = snap.RequireRelationCoverageContext(ctx, []string{"typescript"}, nil)
	if len(want) == 0 {
		require.NoError(t, err)
	} else {
		var missing *store_pebble.ErrLinksNotAvailable
		require.ErrorAs(t, err, &missing)
		require.ElementsMatch(t, want, missing.Affected)
	}
	require.NoError(t, snap.RequireRelationCoverageContext(ctx, []string{"typescript"}, []string{"unrelated.ts"}))
}

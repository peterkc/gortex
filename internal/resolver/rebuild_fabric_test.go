package resolver

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildFabricSQLiteOracle(t *testing.T) {
	ts, spec := rebuildFabricExtract(t, languages.NewTypeScriptExtractor(), "web/ColorViewNativeComponent.ts", "ColorView", `import type { ViewProps } from 'react-native';
import type { DirectEventHandler } from 'react-native/Libraries/Types/CodegenTypes';
import codegenNativeComponent from 'react-native/Libraries/Utilities/codegenNativeComponent';

export interface NativeProps extends ViewProps {
  color?: string;
  onColorChanged?: DirectEventHandler<{ color: string }>;
}

export default codegenNativeComponent<NativeProps>('ColorView');
`)
	objc, apple := rebuildFabricExtract(t, languages.NewObjCExtractor(), "ios/RCTColorViewManager.m", "RCTColorView", `@implementation RCTColorViewManager

RCT_EXPORT_MODULE()

RCT_EXPORT_VIEW_PROPERTY(color, NSString)
RCT_EXPORT_VIEW_PROPERTY(onColorChanged, RCTDirectEventBlock)

@end
`)
	java, jvm := rebuildFabricExtract(t, languages.NewJavaExtractor(), "android/ColorViewManager.java", "RCTColorView", `package com.example;
import com.facebook.react.uimanager.annotations.ReactProp;

public class ColorViewManager extends SimpleViewManager<ColorView> {
    @Override
    public String getName() { return "RCTColorView"; }

    @ReactProp(name = "color")
    public void setColor(ColorView view, String color) {}
}
`)
	require.NotContains(t, spec.Meta, "fabric_native")
	require.Equal(t, "objc", apple.Meta["fabric_native"])
	require.Equal(t, "java", jvm.Meta["fabric_native"])

	// The Web + Apple/JVM census gates the whole turn, not individual
	// endpoints. Even false makes a node native if the role key exists.
	goSpec := &graph.Node{
		ID: "alternate.go::Ring", Name: "Ring", Kind: graph.KindType,
		FilePath: "alternate.go", Language: "go", StartLine: 5,
		Meta: map[string]any{"fabric_component": "RNRing"},
	}
	const jsFile = "roles.js"
	jsNative := &graph.Node{
		ID: jsFile + "::native", Name: "RingManager", Kind: graph.KindType,
		FilePath: jsFile, Language: "javascript", StartLine: 7,
		Meta: map[string]any{"fabric_component": "rCtRnRING", "fabric_native": false},
	}
	wrongKind := &graph.Node{
		ID: jsFile + "::a", Name: "ColorView", Kind: graph.KindMethod,
		FilePath: jsFile, Language: "javascript", StartLine: 2,
		Meta: map[string]any{"fabric_component": "ColorView", "fabric_native": true},
	}
	missingMarker := &graph.Node{
		ID: jsFile + "::b", Name: "ColorView", Kind: graph.KindType,
		FilePath: jsFile, Language: "javascript", StartLine: 3,
		Meta: map[string]any{"fabric_native": true},
	}
	incompatible := &graph.Node{
		// Native strips rct, then rn, once each; the rct here survives.
		ID: jsFile + "::c", Name: "ColorView", Kind: graph.KindType,
		FilePath: jsFile, Language: "javascript", StartLine: 4,
		Meta: map[string]any{"fabric_component": "rnRCTColorView", "fabric_native": true},
	}
	goFacts := rebuildEventFacts(goSpec.FilePath, goSpec.Language, []*graph.Node{goSpec})
	jsFacts := rebuildEventFacts(jsFile, jsNative.Language, []*graph.Node{jsNative, incompatible, missingMarker, wrongKind})

	// This oracle binds the direct SQLite path. Occupy one direction of
	// each ColorView pair at the exact native key. Direct SQLite keeps
	// the incumbent payload; the shadow Graph can replace it.
	forward := rebuildFabricExpectedEdge(spec, apple, "ColorView")
	forward.Confidence, forward.Origin = 0.9, graph.OriginASTResolved
	forward.ConfidenceLabel = graph.ConfidenceLabelFor(forward.Kind, forward.Confidence)
	forward.Meta = map[string]any{"fixture": "Web to Apple incumbent"}
	reverse := rebuildFabricExpectedEdge(jvm, spec, "ColorView")
	reverse.Confidence, reverse.Origin = 0.95, graph.OriginASTResolved
	reverse.ConfidenceLabel = graph.ConfidenceLabelFor(reverse.Kind, reverse.Confidence)
	reverse.Meta = map[string]any{"fixture": "JVM to Web incumbent"}
	ts.Edges = append(ts.Edges, forward)
	java.Edges = append(java.Edges, reverse)

	facts := []store_pebble.ParseFacts{ts, objc, java, jsFacts, goFacts}
	for i := range facts {
		facts[i] = roundTripRebuildRNBridgeFacts(t, facts[i])
	}
	native := openRebuildTemporalOracle(t, facts...)
	// SQLite's NodesByKinds reads by ID, not by file load order. Both
	// matching managers must be visited, regardless of which arrives first.
	var order []string
	for _, n := range native.NodesByKinds([]graph.NodeKind{graph.KindType}) {
		switch n.ID {
		case spec.ID, apple.ID, jvm.ID:
			order = append(order, n.ID)
		}
	}
	require.Equal(t, []string{jvm.ID, apple.ID, spec.ID}, order)
	pending := slices.Clone(facts)
	slices.Reverse(pending)
	private := openRebuildGRPCBuild(t, pending...)
	before := rebuildEventSQLiteRows(native)
	require.Equal(t, before, rebuildEventSavedRows(t, private))
	require.Contains(t, before.edges, forward)
	require.Contains(t, before.edges, reverse)

	appleToSpec := rebuildFabricExpectedEdge(apple, spec, "ColorView")
	specToJVM := rebuildFabricExpectedEdge(spec, jvm, "ColorView")
	goToJS := rebuildFabricExpectedEdge(goSpec, jsNative, "RNRing")
	jsToGo := rebuildFabricExpectedEdge(jsNative, goSpec, "RNRing")
	want := rebuildEventRows{nodes: before.nodes, edges: append(slices.Clone(before.edges), appleToSpec, specToJVM, goToJS, jsToGo)}
	want.sort()

	selection, err := NewFrameworkSynthesizerSelection([]string{SynthFabric})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), private.RebuildView(), selection)
	require.NoError(t, err)
	require.True(t, frozen.allows(SynthFabric))

	for _, tc := range []struct {
		name  string
		count uint64
		first *graph.Edge
	}{
		{"missing JVM receipt", 1, specToJVM},
		// Both directions are affected. alternate.go sorts before roles.js
		// by the From field of the full edge key.
		{"changed JS role key", 2, goToJS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proof := slices.Clone(facts)
			switch tc.name {
			case "missing JVM receipt":
				proof = slices.Delete(proof, 2, 3)
			case "changed JS role key":
				proof[3] = roundTripRebuildRNBridgeFacts(t, proof[3])
				for _, n := range proof[3].Nodes {
					if n.ID == jsNative.ID {
						delete(n.Meta, "fabric_native")
					}
				}
				proof[3] = roundTripRebuildRNBridgeFacts(t, proof[3])
			}
			slices.Reverse(proof)
			stats, err := RunRebuildFabric(t.Context(), private, rebuildGRPCReceipts(proof...), frozen)
			require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
			var refused *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, tc.count, refused.Count)
			require.Equal(t, graph.EdgeIdentityFor(tc.first), refused.First)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, private))
		})
	}

	// The full receipts succeed on the same private root after refusal;
	// replay then inserts nothing and keeps both incumbents as saved.
	for _, wantNew := range []uint64{4, 0} {
		report := RunFrameworkSynthesizersWithSelection(native, selection)
		require.NoError(t, report.Err)
		require.Equal(t, 2, report.Total) // Native counts bound specs, even on replay.
		require.Equal(t, want, rebuildEventSQLiteRows(native))
		stats, err := RunRebuildFabric(t.Context(), private, rebuildGRPCReceipts(facts...), frozen)
		require.NoError(t, err)
		require.Equal(t, wantNew, stats.Edges)
		require.Equal(t, want, rebuildEventSavedRows(t, private))
	}
}

func TestRebuildFabricSQLiteLargeScratch(t *testing.T) {
	// 23 x 23 pairs give 1,058 proposals. The pending overlay is not
	// bounded by the separate sorted-run helper's 1,024-run cap.
	const count = 23
	const specFile, managerFile = "specs.ts", "Managers.java"
	specs, managers := make([]*graph.Node, 0, count), make([]*graph.Node, 0, count)
	pad := strings.Repeat("x", 1<<10)
	for i := range count {
		specs = append(specs, &graph.Node{
			ID: fmt.Sprintf("%s::spec%d%s", specFile, i, pad), Name: "ColorView", Kind: graph.KindType,
			FilePath: specFile, Language: "typescript", StartLine: i + 2,
			Meta: map[string]any{"fabric_component": "ColorView"},
		})
		managers = append(managers, &graph.Node{
			ID: fmt.Sprintf("%s::manager%d%s", managerFile, i, pad), Name: "RCTColorView", Kind: graph.KindType,
			FilePath: managerFile, Language: "java", StartLine: i + 3,
			Meta: map[string]any{"fabric_component": "RCTColorView", "fabric_native": "java"},
		})
	}
	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(specFile, "typescript", specs),
		rebuildEventFacts(managerFile, "java", managers),
	}
	for i := range facts {
		facts[i] = roundTripRebuildRNBridgeFacts(t, facts[i])
	}
	native := openRebuildTemporalOracle(t, facts...)
	private := openRebuildGRPCBuild(t, facts...)
	require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, private))
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthFabric})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), private.RebuildView(), selection)
	require.NoError(t, err)
	require.True(t, frozen.allows(SynthFabric))
	report := RunFrameworkSynthesizersWithSelection(native, selection)
	require.NoError(t, report.Err)
	require.Equal(t, count, report.Total)
	want := rebuildEventSQLiteRows(native)
	require.Len(t, want.edges, 2*count*count)
	stats, err := RunRebuildFabric(t.Context(), private, rebuildGRPCReceipts(facts...), frozen)
	require.NoError(t, err)
	require.EqualValues(t, 2*count*count, stats.Edges)
	require.Equal(t, want, rebuildEventSavedRows(t, private))
}

func rebuildFabricExtract(t *testing.T, extractor parser.Extractor, file, component, source string) (store_pebble.ParseFacts, *graph.Node) {
	t.Helper()
	parsed, err := extractor.Extract(file, []byte(source))
	require.NoError(t, err)
	t.Cleanup(parsed.ReleaseTree)
	var marker *graph.Node
	for _, n := range parsed.Nodes {
		if n.Meta["fabric_component"] != component {
			continue
		}
		require.Nil(t, marker)
		marker = n
	}
	require.NotNil(t, marker)
	require.Equal(t, file+"::fabric:"+component, marker.ID)
	require.Equal(t, graph.KindType, marker.Kind)
	require.Equal(t, extractor.Language(), marker.Language)
	return rebuildEventFacts(file, extractor.Language(), parsed.Nodes, parsed.Edges...), marker
}

func rebuildFabricExpectedEdge(from, to *graph.Node, component string) *graph.Edge {
	return &graph.Edge{
		From:            from.ID,
		To:              to.ID,
		Kind:            graph.EdgeReferences,
		FilePath:        from.FilePath,
		Line:            from.StartLine,
		Confidence:      0.6,
		ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeReferences, 0.6),
		Origin:          graph.OriginASTInferred,
		Meta: map[string]any{
			"via":              "fabric.component",
			"fabric_component": component,
			"to_language":      to.Language,
			MetaSynthesizedBy:  SynthFabric,
			MetaProvenance:     ProvenanceHeuristic,
		},
	}
}

func TestRebuildFabricFrozenFamilyAndDisabledSelection(t *testing.T) {
	spec := &graph.Node{ID: "host.go::View", Kind: graph.KindType, FilePath: "host.go", Language: "go", StartLine: 2,
		Meta: map[string]any{"fabric_component": "View"}}
	manager := &graph.Node{ID: "View.java::RCTView", Kind: graph.KindType, FilePath: "View.java", Language: "java", StartLine: 3,
		Meta: map[string]any{"fabric_component": "RCTView", "fabric_native": "java"}}
	web := &graph.Node{ID: "marker.ts", Kind: graph.KindFile, FilePath: "marker.ts", Language: "typescript"}
	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(spec.FilePath, spec.Language, []*graph.Node{spec}),
		rebuildEventFacts(manager.FilePath, manager.Language, []*graph.Node{manager}),
		rebuildEventFacts(web.FilePath, web.Language, []*graph.Node{web}),
	}
	b := openRebuildGRPCBuild(t, facts[:2]...)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthFabric})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
	require.NoError(t, err)
	require.False(t, frozen.allows(SynthFabric))
	require.NoError(t, b.AddParsedFile("", facts[2]))

	disabled, err := NewFrameworkSynthesizerSelection(nil)
	require.NoError(t, err)
	before := rebuildEventSavedRows(t, b)
	for _, excluded := range []FrameworkSynthesizerSelection{frozen, disabled} {
		stats, err := RunRebuildFabric(t.Context(), b, func(func(store_pebble.ParseFacts, error) bool) {
			t.Fatal("ineligible Fabric selection read receipts")
		}, excluded)
		require.NoError(t, err)
		require.Zero(t, stats)
		require.Equal(t, before, rebuildEventSavedRows(t, b))
	}

	// This is the same live graph. Only taking a new census can admit it.
	fresh, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
	require.NoError(t, err)
	require.True(t, fresh.allows(SynthFabric))
	native := openRebuildTemporalOracle(t, facts...)
	report := RunFrameworkSynthesizersWithSelection(native, selection)
	require.NoError(t, report.Err)
	want := rebuildEventSQLiteRows(native)
	require.Len(t, want.edges, 2)
	stats, err := RunRebuildFabric(t.Context(), b, rebuildGRPCReceipts(facts...), fresh)
	require.NoError(t, err)
	require.EqualValues(t, 2, stats.Edges)
	require.Equal(t, want, rebuildEventSavedRows(t, b))
}

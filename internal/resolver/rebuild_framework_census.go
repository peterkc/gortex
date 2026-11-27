package resolver

import (
	"context"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// FreezeRebuildFrameworkSelection applies the native cold pre-loop gates to
// the wired private turns. Only the decision is frozen: admitted reducers
// still read the live graph after the preceding turns have written to it.
func FreezeRebuildFrameworkSelection(ctx context.Context, view store_pebble.RebuildView,
	selection FrameworkSynthesizerSelection,
) (FrameworkSynthesizerSelection, error) {
	frozen := selection
	frozen.configured = true
	frozen.names = make(map[string]struct{})
	for _, name := range [...]string{
		SynthGRPCStub, SynthTemporalStub, SynthEventChannel, SynthSwiftObjC,
		SynthReactNative, SynthReactNativePair, SynthObserverChannel, SynthClosureCollection,
		SynthReactSetState, SynthFlutterSetState, SynthKMPExpectActual, SynthExpoModules, SynthFabric,
	} {
		if selection.allows(name) {
			frozen.names[name] = struct{}{}
		}
	}
	if frozen.none() {
		return frozen, nil
	}

	present, markers := map[string]int{}, map[string]int{}
	var observerRoles int
	if err := view.VisitFrameworkGateNodes(ctx, func(n *graph.Node) error {
		family := frameworkLanguageFamily(n.Language)
		if family != "" {
			present[family]++
		}
		if role := recordFrameworkNodeCandidates(markers, n, family); role != 0 && n.ID != "" {
			observerRoles++
		}
		return nil
	}); err != nil {
		return FrameworkSynthesizerSelection{}, err
	}
	if frozen.allows(SynthObserverChannel) {
		if err := completeRebuildObserverGate(ctx, view, markers, observerRoles); err != nil {
			return FrameworkSynthesizerSelection{}, err
		}
	}
	for name := range frozen.names {
		if !frameworkSynthNodeGatesPass(name, present, markers) {
			delete(frozen.names, name)
		}
	}
	var edges frameworkEdgeCensus
	_, grpc := frozen.names[SynthGRPCStub]
	_, temporal := frozen.names[SynthTemporalStub]
	_, react := frozen.names[SynthReactSetState]
	_, flutter := frozen.names[SynthFlutterSetState]
	if grpc || temporal || react || flutter {
		if err := view.VisitFrameworkGateEdges(ctx, temporal, func(e graph.FrameworkCensusEdge) error {
			if e.Kind == graph.EdgeCalls {
				edges.setStateTarget = edges.setStateTarget || isSetStateTarget(e.To)
				edges.grpcStub = edges.grpcStub || e.Via == "grpc.stub" && e.GRPCService != "" && e.GRPCMethod != ""
				edges.temporalVia = edges.temporalVia || strings.HasPrefix(e.Via, "temporal.")
			} else {
				role, member := temporalRoleForJavaAnnotation(e.To)
				edges.temporalAnnotation = edges.temporalAnnotation || role != "" || member != ""
			}
			return nil
		}); err != nil {
			return FrameworkSynthesizerSelection{}, err
		}
	}
	for name := range frozen.names {
		if gate := frameworkSynthEdgePreflights[name]; gate != nil && !gate(edges) {
			delete(frozen.names, name)
		}
	}
	return frozen, nil
}

func completeRebuildObserverGate(ctx context.Context, view store_pebble.RebuildView, markers map[string]int, roles int) error {
	if roles > frameworkScopeRetainedRowCap {
		// Native retains Observer conservatively on role-cache overflow.
		markers[SynthObserverChannel]++
		return nil
	}
	if markers[frameworkMarkerObserverRegistrar] == 0 || markers[frameworkMarkerObserverDispatcher] == 0 {
		return nil
	}
	both := frameworkObserverRegistrarRole | frameworkObserverDispatcherRole
	found, err := view.HasFrameworkAccessRoles(ctx, both, func(n *graph.Node) uint8 {
		var role uint8
		if (n.Kind == graph.KindMethod || n.Kind == graph.KindFunction) && n.Name != "" {
			if observerRegistrarRe.MatchString(n.Name) {
				role |= frameworkObserverRegistrarRole
			}
			if observerDispatcherRe.MatchString(n.Name) {
				role |= frameworkObserverDispatcherRole
			}
		}
		return role
	})
	if found {
		markers[SynthObserverChannel]++
	}
	return err
}

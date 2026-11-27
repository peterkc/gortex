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
		SynthReactSetState, SynthFlutterSetState, SynthKMPExpectActual, SynthExpoModules, SynthFabric, SynthMyBatis,
		SynthSQLCallsite, SynthStoreFactory, SynthReduxThunk, SynthNgRxEffect,
		SynthObjectRegistry, SynthRTKQuery, SynthVuexDispatch, SynthCelery, SynthSpringEvent, SynthMediatR, SynthCSharpDIIfaces, SynthCSharpIfaceDispatch, SynthCSharpEFCoreModels, SynthSidekiq, SynthLaravelEvent, SynthFnPointerDispatch, SynthMacroExpansion, SynthGinMiddleware, SynthExpressResolve, SynthReactResolve, SynthFastAPIResolve, SynthRailsResolve, SynthGodotAutoload, SynthGodotPreloadAlias, SynthSwiftUIResolve, SynthUIKitResolve, SynthVaporResolve, SynthGoFrameRoute, SynthFactoryChain, SynthFnValue, SynthValueRefName,
	} {
		if selection.allows(name) {
			frozen.names[name] = struct{}{}
		}
	}
	if frozen.none() {
		return frozen, nil
	}
	if len(frozen.names) == 1 && frozen.allows(SynthMacroExpansion) {
		admitted, err := view.FrameworkMacroGate(ctx, rebuildCFamilyLanguage)
		if err != nil {
			return FrameworkSynthesizerSelection{}, err
		}
		if !admitted {
			delete(frozen.names, SynthMacroExpansion)
		}
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
	_, redux := frozen.names[SynthReduxThunk]
	_, ngrx := frozen.names[SynthNgRxEffect]
	_, registry := frozen.names[SynthObjectRegistry]
	_, rtk := frozen.names[SynthRTKQuery]
	_, vuex := frozen.names[SynthVuexDispatch]
	_, celery := frozen.names[SynthCelery]
	_, spring := frozen.names[SynthSpringEvent]
	_, mediatr := frozen.names[SynthMediatR]
	_, laravel := frozen.names[SynthLaravelEvent]
	_, express := frozen.names[SynthExpressResolve]
	_, rails := frozen.names[SynthRailsResolve]
	if redux || ngrx || rtk || vuex || celery || spring || mediatr || laravel {
		edges.via = make(map[string]bool, 8)
	}
	if grpc || temporal || react || flutter || redux || ngrx || registry || rtk || vuex || celery || spring || mediatr || laravel || express || rails {
		if err := view.VisitFrameworkGateEdges(ctx, temporal, func(e graph.FrameworkCensusEdge) error {
			if e.Kind == graph.EdgeCalls {
				via := (redux && e.Via == reduxThunkVia) || (ngrx && e.Via == ngrxEffectVia) ||
					(rtk && e.Via == rtkQueryVia) || (vuex && e.Via == vuexDispatchVia) || (celery && e.Via == celeryVia) ||
					(mediatr && e.Via == mediatrVia) || (laravel && e.Via == laravelEventVia)
				setState := (react || flutter) && isSetStateTarget(e.To)
				grpcStub := grpc && e.Via == "grpc.stub" && e.GRPCService != "" && e.GRPCMethod != ""
				temporalVia := temporal && strings.HasPrefix(e.Via, "temporal.")
				objectRegistry := registry && e.Via == objectRegistryVia && e.RegistryValue != ""
				if via || (spring && e.Via == springEventVia) {
					edges.via[e.Via] = true
				}
				edges.recvConst = edges.recvConst || rails && e.RecvConst != "" && graph.IsUnresolvedTarget(e.To)
				edges.expressHandlerRef = edges.expressHandlerRef || express && e.HasExpressHandlerRef && graph.IsUnresolvedTarget(e.To)
				edges.objectRegistryValue = edges.objectRegistryValue || objectRegistry
				edges.setStateTarget = edges.setStateTarget || setState
				edges.grpcStub = edges.grpcStub || grpcStub
				edges.temporalVia = edges.temporalVia || temporalVia
			} else {
				role, member := temporalRoleForJavaAnnotation(e.To)
				annotation := role != "" || member != ""
				edges.temporalAnnotation = edges.temporalAnnotation || annotation
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
	found, err := view.HasFrameworkAccessRoles(ctx, both, frameworkObserverRole)
	if found {
		markers[SynthObserverChannel]++
	}
	return err
}

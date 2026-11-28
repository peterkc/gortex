package resolver

import (
	"context"
	"iter"
	"slices"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// MarkRebuildFrameworkIsolation records relation coverage immediately before
// the named turn. Requested selection controls coverage admission; frozen
// selection continues to control only the actual reducer. The held census
// cannot add a node or edge to the working graph.
func MarkRebuildFrameworkIsolation(ctx context.Context, b *store_pebble.BuildStore, requested, frozen FrameworkSynthesizerSelection,
	pass string, language func(string) string, receipts iter.Seq2[store_pebble.ParseFacts, error],
) error {
	if !requested.allows(pass) {
		return nil
	}
	required, family := expoBridgeFamilyMask()
	role := func(n *graph.Node) uint8 { return family(n.Language) }
	switch pass {
	case SynthReactNative:
		required = 1
		role = func(n *graph.Node) uint8 {
			if slices.Contains(frameworkSynthLanguageFamilies[SynthReactNative], frameworkLanguageFamily(n.Language)) {
				return 1
			}
			return 0
		}
	case SynthSvelteKitLoad:
		required = 7
		markers := make(map[string]int)
		role = func(n *graph.Node) uint8 {
			var bits uint8
			if frameworkLanguageFamily(n.Language) == "web" {
				bits |= 1
			}
			clear(markers)
			recordFrameworkNodeCandidates(markers, n, frameworkLanguageFamily(n.Language))
			if markers[frameworkMarkerSvelteKitPage] != 0 {
				bits |= 2
			}
			if markers[frameworkMarkerSvelteKitServer] != 0 {
				bits |= 4
			}
			return bits
		}
	case SynthExpoModules, SynthFabric:
	default:
		return nil
	}
	full, causes, err := b.RebuildFrameworkIsolationGate(ctx, required, role)
	if err != nil || !full {
		return err
	}
	admitted := frozen.allows(pass)
	switch pass {
	case SynthReactNative, SynthExpoModules:
		return b.MarkRebuildBridgeIsolation(ctx, receipts, pass == SynthExpoModules, admitted, causes, language)
	case SynthFabric:
		return b.MarkRebuildFabricIsolation(ctx, admitted, causes, language, fabricComponentCandidate, fabricBridgeEdge)
	case SynthSvelteKitLoad:
		return b.MarkRebuildSvelteKitIsolation(ctx, admitted, causes, language, store_pebble.RebuildSvelteKitLoadRules{
			RouteNode: svelteKitRouteNode,
			ServerDir: svelteKitServerDir,
			PageFile: func(path string) bool {
				base, _ := pathBaseDir(path)
				return isSvelteKitPageFile(base)
			},
			Target:    svelteKitLoadTarget,
			Reference: svelteKitLoadReference,
		})
	}
	return nil
}

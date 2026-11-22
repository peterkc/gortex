package resolver

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func runRebuildGoBuiltins(ctx context.Context, build *store_pebble.BuildStore, reconcileSources bool, spillBytes int) error {
	sources := make(map[string]*graph.Node, 3)
	materialized := make(map[string]*graph.Node, 1)
	return build.RebuildGoBuiltins(ctx, reconcileSources, spillBytes,
		func(e *graph.Edge, readSource func(string) (*graph.Node, error)) (*graph.Node, error) {
			if !goBuiltinPendingCandidate(e) {
				return nil, nil
			}
			clear(sources)
			clear(materialized)
			ids := [3]string{e.From, enclosingFunctionForBinding(e.From)}
			if detachedSource(e.From) {
				ids[2] = e.FilePath
			}
			for _, id := range ids {
				if _, seen := sources[id]; id == "" || seen {
					continue
				}
				n, err := readSource(id)
				if err != nil {
					return nil, err
				}
				sources[id] = n
			}
			candidate := *e
			if tryAttributeGoBuiltin(&candidate, sources, materialized) == "" {
				return nil, nil
			}
			return materialized[candidate.To], nil
		}, sameBuiltinSentinel)
}

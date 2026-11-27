package resolver

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func runRebuildGoBuiltins(ctx context.Context, build *store_pebble.BuildStore, reconcileSources bool, spillBytes int) error {
	sources := make(map[string]*graph.Node, 3)
	return build.RebuildGoBuiltins(ctx, reconcileSources, spillBytes,
		func(e *graph.Edge, readSource func(string) (*graph.Node, error)) (string, error) {
			if !goBuiltinPendingCandidate(e) {
				return "", nil
			}
			clear(sources)
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
					return "", err
				}
				sources[id] = n
			}
			name, repo, _ := goBuiltinSource(e, sources)
			id, _, _ := goBuiltinTarget(repo, name)
			if _, ok := graph.LookupPlatformBuiltin(graph.PlatformCatalogRevision, id); !ok {
				return "", nil
			}
			return id, nil
		})
}

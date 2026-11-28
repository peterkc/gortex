package indexer

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// visitRebuildSpringBeanCalls captures the native contract-era Call effects.
// Keep these rows as a private delta until Contracts validates its Calls.
func visitRebuildSpringBeanCalls(ctx context.Context, b *store_pebble.BuildStore, repo string, yield func(*graph.Edge) error) error {
	return b.VisitSpringBeanConsumers(ctx, repo,
		func(e *graph.Edge) string {
			bean, _ := springBeanFromEdge(e)
			return bean.typeName
		},
		func(n *graph.Node) (string, map[string]struct{}) {
			params, _ := n.Meta["params_src"].(string)
			return enclosingClassID(n), javaIdentifierSet(params)
		},
		func(class string, e *graph.Edge) error {
			bean, _ := springBeanFromEdge(e)
			return yield(springBeanCall(class, bean))
		})
}

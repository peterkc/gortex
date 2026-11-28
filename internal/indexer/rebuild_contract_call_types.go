package indexer

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// rebuildCallReturnName uses the native method candidate and signature rules
// with one current name at a time. Agreement is independent of index order.
func rebuildCallReturnName(ctx context.Context, view store_pebble.RebuildView, key contractCallKey) (rawContractCallType, error) {
	name, receiver := contractCallName(key.callExpr)
	if name == "" {
		return rawContractCallType{}, ctx.Err()
	}
	var choice contractCallReturnChoice
	if err := view.VisitNameCandidates(ctx, name, nil, func(n *graph.Node) error {
		choice.observe(n, key.repoPrefix, receiver)
		return nil
	}); err != nil {
		return rawContractCallType{}, err
	}
	return choice.result(), nil
}

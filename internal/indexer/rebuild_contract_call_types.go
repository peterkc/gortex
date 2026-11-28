package indexer

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"go.uber.org/zap"
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

// logRebuildCallReturnTypeOrder gives the VM one count per replay and debug
// detail for every order-sensitive native lookup, without holding a list.
func logRebuildCallReturnTypeOrder(records *store_pebble.RebuildContractRecords, logger *zap.Logger) error {
	var cases int
	if err := records.VisitCallReturnTypeOrderDifferences(func(repo, name, id string, candidates int) error {
		cases++
		logger.Debug("rebuild call return type native order difference", zap.String("repo", repo), zap.String("type", name),
			zap.String("selected_id", id), zap.Int("candidates", candidates))
		return nil
	}); err != nil {
		return err
	}
	logger.Info("rebuild call return type native order differences", zap.Int("cases", cases))
	return nil
}

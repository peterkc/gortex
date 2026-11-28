package indexer

import (
	"context"
	"fmt"
	"iter"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// visitRebuildContractGraphRows reduces ordered native registry records at the
// contract cut. The source must supply the live Registry.All representatives,
// already in stable contractNodeRecordLess order. A nil node means an earlier
// record won that ID. The edges are logical owner rows, not yet the unique
// physical delta: D83 and any edge-key collisions are decided downstream.
func visitRebuildContractGraphRows(ctx context.Context, view store_pebble.RebuildView, records iter.Seq2[contracts.Contract, error],
	yield func(contracts.Contract, *graph.Node, []*graph.Edge) error,
) error {
	var previous contracts.Contract
	if records == nil || yield == nil {
		return fmt.Errorf("%w: contract row reduction needs a stream and visitor", graph.ErrContractProjectionStale)
	}
	for c, err := range records {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if c.ID == "" || contractNodeRecordLess(c, previous) {
			return fmt.Errorf("%w: unordered or invalid contract record %q", graph.ErrContractProjectionStale, c.ID)
		}
		owner, err := rebuildContractOwner(ctx, view, c)
		if err != nil {
			return err
		}
		var node *graph.Node
		if c.ID != previous.ID {
			node = contractGraphNode(c, owner != "")
		}
		if err := yield(c, node, appendContractGraphEdges(nil, c, owner)); err != nil {
			return err
		}
		previous = contracts.Contract{ID: c.ID, FilePath: c.FilePath, SymbolID: c.SymbolID, Role: c.Role, Line: c.Line}
	}
	return ctx.Err()
}

func rebuildContractOwner(ctx context.Context, view store_pebble.RebuildView, c contracts.Contract) (string, error) {
	if c.SymbolID == "" {
		return view.ContractFileOwner(ctx, c.RepoPrefix, c.FilePath)
	}
	n, err := view.Node(ctx, c.SymbolID)
	if err != nil || n == nil || n.RepoPrefix != c.RepoPrefix {
		return "", err
	}
	return n.ID, nil
}

package indexer

import (
	"context"
	"errors"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// bindRebuildProviderHandlers uses a frozen representative turn for native
// tasks and edits only their live matching registrations. Unproved body
// writes keep the exact logical symbol move, but cannot publish a payload.
func (r rebuildContractReceiptReader) bindRebuildProviderHandlers(ctx context.Context, view store_pebble.RebuildView, records *store_pebble.RebuildContractRecords) error {
	pass := r.handlerPass()
	if err := records.VisitRelevantRepresentatives(func(task store_pebble.RebuildContractRecord) error {
		winner, err := pass.providerHandler(ctx, view, task.Contract)
		if err != nil || winner == nil {
			return err
		}
		return records.UpdateByID(task.ID, func(live *store_pebble.RebuildContractRecord) (bool, error) {
			if live.Role != contracts.RoleProvider || live.RepoPrefix != task.RepoPrefix || live.FilePath != task.FilePath ||
				live.SymbolID != task.SymbolID || live.Line != task.Line {
				return false, nil
			}
			body, known := pass.bodies[winner.ID]
			if !known {
				body.unproved = r.sizeSkippedContractSource(winner.FilePath)
				if !body.unproved {
					var err error
					body.facts, err = r.providerBody(ctx, view, winner)
					body.unproved = errors.Is(err, errRebuildContractBodyUnproved)
					if err != nil && !body.unproved {
						return false, err
					}
				}
				pass.bodies[winner.ID] = body
			}
			if body.unproved {
				live.SymbolID = winner.ID
				live.BodyUnproved = true
				removeRebuildHandlerHints(&live.Contract)
			} else {
				bindRebuildProviderRecord(&live.Contract, winner.ID, body.facts)
			}
			return true, nil
		})
	}, func(item store_pebble.RebuildContractRecord) bool {
		trail, fallback, _ := contracts.ProviderHandlerHints(item.Contract)
		return trail != "" || fallback != ""
	}); err != nil {
		return err
	}
	// Native takes a fresh All turn for hint cleanup. An unresolved or
	// already enriched representative may still open its live ByID bucket.
	return records.VisitRelevantRepresentatives(func(item store_pebble.RebuildContractRecord) error {
		if !hasRebuildHandlerHints(item.Contract) {
			return nil
		}
		return records.UpdateByID(item.ID, func(live *store_pebble.RebuildContractRecord) (bool, error) {
			if !hasRebuildHandlerHints(live.Contract) {
				return false, nil
			}
			removeRebuildHandlerHints(&live.Contract)
			return true, nil
		})
	}, func(item store_pebble.RebuildContractRecord) bool { return hasRebuildHandlerHints(item.Contract) })
}

func hasRebuildHandlerHints(c contracts.Contract) bool {
	_, ident := c.Meta["handler_ident"]
	_, trail := c.Meta["handler_trail"]
	return ident || trail
}

func removeRebuildHandlerHints(c *contracts.Contract) {
	delete(c.Meta, "handler_ident")
	delete(c.Meta, "handler_trail")
}

// sizeSkippedContractSource proves native source presence, not a body answer.
func (r rebuildContractReceiptReader) sizeSkippedContractSource(file string) bool {
	origin := r.census.originByGraphPath[file]
	return origin != "" && r.census.byPath[origin] == "" && r.job.classifications[origin].sizeSkipped()
}

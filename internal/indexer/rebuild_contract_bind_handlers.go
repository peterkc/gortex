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
	if err := records.VisitRepresentatives(func(task store_pebble.RebuildContractRecord) error {
		winner, err := r.providerHandler(ctx, view, task.Contract)
		if err != nil || winner == nil {
			return err
		}
		var body contracts.HandlerFacts
		var bodyLoaded, unproved bool
		return records.UpdateByID(task.ID, func(live *store_pebble.RebuildContractRecord) (bool, error) {
			if live.Role != contracts.RoleProvider || live.RepoPrefix != task.RepoPrefix || live.FilePath != task.FilePath ||
				live.SymbolID != task.SymbolID || live.Line != task.Line {
				return false, nil
			}
			if !bodyLoaded {
				unproved = r.sizeSkippedContractSource(winner.FilePath)
				if !unproved {
					var err error
					body, err = r.providerBody(ctx, view, winner)
					unproved = errors.Is(err, errRebuildContractBodyUnproved)
					if err != nil && !unproved {
						return false, err
					}
				}
				bodyLoaded = true
			}
			if unproved {
				live.SymbolID = winner.ID
				live.BodyUnproved = true
				removeRebuildHandlerHints(&live.Contract)
			} else {
				bindRebuildProviderRecord(&live.Contract, winner.ID, body)
			}
			return true, nil
		})
	}); err != nil {
		return err
	}
	// Native takes a fresh All turn for hint cleanup. An unresolved or
	// already enriched representative may still open its live ByID bucket.
	return records.VisitRepresentatives(func(item store_pebble.RebuildContractRecord) error {
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
	})
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

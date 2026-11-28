package indexer

import (
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// providerBody checks the linked inputs to the saved native body writes.
// Capture used the accepted file's declaration range and its first match for
// each local type name. Refuse if the linked file cannot prove those inputs.
func (r rebuildContractReceiptReader) providerBody(ctx context.Context, view store_pebble.RebuildView, handler *graph.Node) (contracts.HandlerFacts, error) {
	if handler.StartLine <= 0 {
		// Native uses the registration line in this case; the generic
		// handler receipt does not record that site-specific answer.
		return contracts.HandlerFacts{}, fmt.Errorf("%w: handler %q has no declaration start line", graph.ErrContractProjectionStale, handler.ID)
	}
	base, facts, err := r.forLinkedDeclaration(ctx, handler)
	if err != nil {
		return contracts.HandlerFacts{}, err
	}
	body, ok := facts.Handlers[handler.ID]
	if !ok {
		return contracts.HandlerFacts{}, fmt.Errorf("%w: missing body facts for %q", graph.ErrContractProjectionStale, handler.ID)
	}
	local := make(map[string]string)
	var foundHandler bool
	for _, n := range base.Nodes {
		if n.ID == handler.ID {
			if foundHandler || n.Name != handler.Name {
				return contracts.HandlerFacts{}, fmt.Errorf("%w: ambiguous or renamed handler %q", graph.ErrContractProjectionStale, handler.ID)
			}
			foundHandler = true
		}
		if n.Kind != graph.KindType && n.Kind != graph.KindInterface {
			continue
		}
		if first, exists := local[n.Name]; exists && first != n.ID {
			return contracts.HandlerFacts{}, fmt.Errorf("%w: unordered local contract type %q in %q", graph.ErrContractProjectionStale, n.Name, handler.FilePath)
		}
		local[n.Name] = n.ID
	}
	if err := view.VisitContractFileTypes(ctx, handler.FilePath, func(n *graph.Node) error {
		if local[n.Name] != n.ID {
			return fmt.Errorf("%w: changed local contract type %q in %q", graph.ErrContractProjectionStale, n.Name, handler.FilePath)
		}
		delete(local, n.Name)
		return nil
	}); err != nil {
		return contracts.HandlerFacts{}, err
	}
	if len(local) != 0 {
		return contracts.HandlerFacts{}, fmt.Errorf("%w: missing local contract types in %q", graph.ErrContractProjectionStale, handler.FilePath)
	}
	return body, nil
}

// bindRebuildProviderRecord patches a matching live registration. The linked
// handler supplies the symbol and schema, never the cited file or line.
func bindRebuildProviderRecord(c *contracts.Contract, handlerID string, body contracts.HandlerFacts) {
	c.SymbolID = handlerID
	body.EnrichProvider(c)
	delete(c.Meta, "handler_ident")
	delete(c.Meta, "handler_trail")
}

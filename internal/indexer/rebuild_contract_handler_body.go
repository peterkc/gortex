package indexer

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// errRebuildContractBodyUnproved is a D83 omission input, not an invalid
// receipt. Keep the elected symbol and complete logical ID when this happens.
var errRebuildContractBodyUnproved = errors.New("rebuild contract body is unproved")

// providerBody checks only the linked type names used by the saved body writes.
// Capture used the accepted file's declaration range and its first match for
// each local type name. Refuse if the linked file cannot prove those inputs.
func (r rebuildContractReceiptReader) providerBody(ctx context.Context, view store_pebble.RebuildView, handler *graph.Node) (contracts.HandlerFacts, error) {
	r.linkView = view
	base, facts, err := r.forLinkedDeclaration(ctx, handler)
	if err != nil {
		return contracts.HandlerFacts{}, err
	}
	body, ok := facts.Handlers[handler.ID]
	if !ok {
		return contracts.HandlerFacts{}, fmt.Errorf("%w: missing body facts for %q", graph.ErrContractProjectionStale, handler.ID)
	}
	if handler.StartLine <= 0 {
		// Native uses the registration line; the generic receipt does not
		// record that site-specific answer.
		return contracts.HandlerFacts{}, fmt.Errorf("%w: handler %q has no declaration start line", errRebuildContractBodyUnproved, handler.ID)
	}
	refs := handlerBodyRegexTypeValues(body)
	local := make(map[string]string)
	// A bare captured name with no matching parse declaration may turn
	// into a symbol ID if a linked type appears under that name.
	for ref := range refs {
		if !strings.Contains(ref, "::") {
			local[ref] = ""
		}
	}
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
		if _, referenced := refs[n.ID]; referenced {
			local[n.Name] = n.ID
		} else if local[n.Name] == "" {
			// A captured bare value did not come through local lookup
			// when that lookup already had a physical declaration.
			delete(local, n.Name)
		}
	}
	// AST facts record actual local queries. A primitive literal is not a
	// query even when its spelling names a declaration.
	for name, answer := range body.ASTTypeLookups {
		if answer == name {
			answer = ""
		}
		local[name] = answer
	}
	if len(local) == 0 {
		return body, nil
	}
	seen := make(map[string]bool)
	var changed string
	var hasChange bool
	if err := view.VisitContractFileTypes(ctx, handler.FilePath, func(n *graph.Node) error {
		id, used := local[n.Name]
		if !used {
			return nil
		}
		if id != n.ID && !hasChange {
			changed, hasChange = n.Name, true
		}
		seen[n.Name] = true
		return nil
	}); err != nil {
		return contracts.HandlerFacts{}, err
	}
	for name, id := range local {
		if id != "" && !seen[name] && (!hasChange || name < changed) {
			changed, hasChange = name, true
		}
	}
	if hasChange {
		return contracts.HandlerFacts{}, fmt.Errorf("%w: local type %q in %q", errRebuildContractBodyUnproved, changed, handler.FilePath)
	}
	return body, nil
}

func handlerBodyRegexTypeValues(body contracts.HandlerFacts) map[string]struct{} {
	refs := make(map[string]struct{})
	for _, key := range []string{"request_type", "response_type"} {
		if _, overwritten := body.AST[key]; overwritten {
			continue
		}
		if ref, ok := body.Regex[key].(string); ok && ref != "" {
			refs[ref] = struct{}{}
		}
	}
	return refs
}

// bindRebuildProviderRecord patches a matching live registration. The linked
// handler supplies the symbol and schema, never the cited file or line.
func bindRebuildProviderRecord(c *contracts.Contract, handlerID string, body contracts.HandlerFacts) {
	c.SymbolID = handlerID
	body.EnrichProvider(c)
	removeRebuildHandlerHints(c)
}

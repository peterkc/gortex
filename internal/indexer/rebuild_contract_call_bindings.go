package indexer

import (
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func rebuildCallReturnSite(c contracts.Contract, name string) store_pebble.RebuildCallReturnSite {
	return store_pebble.RebuildCallReturnSite{RepoPrefix: c.RepoPrefix, FilePath: c.FilePath, SymbolID: c.SymbolID, Name: name}
}

// resolveLateCallReturnTypes opens the original binding sites first, then
// applies their answers to live provider buckets, as the native pass does.
func (r rebuildContractReceiptReader) resolveLateCallReturnTypes(ctx context.Context, view store_pebble.RebuildView,
	records *store_pebble.RebuildContractRecords,
) error {
	if _, ok := r.idx.graph.(graph.SemanticBindingTypeReader); ok {
		return fmt.Errorf("%w: semantic binding reader installed for rebuild", errRebuildBuildWire)
	}
	if _, ok := contracts.CurrentBindingResolver().(graph.SemanticBindingTypeReader); ok {
		return fmt.Errorf("%w: semantic binding provider installed for rebuild", errRebuildBuildWire)
	}
	var lastSymbol string
	var body contracts.HandlerFacts
	if err := records.VisitRepresentatives(func(item store_pebble.RebuildContractRecord) error {
		if item.Type != contracts.ContractHTTP || item.Role != contracts.RoleProvider || item.SymbolID == "" {
			return nil
		}
		for _, name := range unresolvedContractBindingNames(item.Contract) {
			site := rebuildCallReturnSite(item.Contract, name)
			if err := records.PrepareCallReturnBinding(site, func() (store_pebble.RebuildCallReturnBinding, error) {
				if item.SymbolID != lastSymbol {
					var err error
					body, err = r.rebuildCallReturnBody(ctx, view, item.SymbolID)
					if err != nil {
						return store_pebble.RebuildCallReturnBinding{}, err
					}
					lastSymbol = item.SymbolID
				}
				return rebuildResolveCallBinding(ctx, view, records, item.RepoPrefix, body.VarBinding(name))
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := records.VisitCurrentRepresentatives(func(item store_pebble.RebuildContractRecord) error {
		if item.Type != contracts.ContractHTTP || item.Role != contracts.RoleProvider {
			return nil
		}
		return applyRebuildCallReturnBinding(records, item.Contract)
	}); err != nil {
		return err
	}
	return logRebuildCallReturnTypeOrder(records, r.idx.logger)
}

func (r rebuildContractReceiptReader) rebuildCallReturnBody(ctx context.Context, view store_pebble.RebuildView, symbol string) (contracts.HandlerFacts, error) {
	var empty contracts.HandlerFacts
	handler, err := view.Node(ctx, symbol)
	if err != nil || handler == nil {
		return empty, err
	}
	// The handler binder retains the logical owner and marks its body
	// unproved. There is no checked late binding to apply to that row.
	if r.sizeSkippedContractSource(handler.FilePath) {
		return empty, nil
	}
	lang := handler.Language
	if lang == "" {
		lang = detectLangFromPath(handler.FilePath)
	}
	// Go is the only native BodyFacts factory, and C1 captures it
	// using the declaration's raw Go source, not the registration site.
	if lang != "go" {
		return empty, nil
	}
	// C1 does not certify arbitrary Go body owners (including variable
	// declarations with function literals). Refuse rather than infer from source.
	if detectLangFromPath(handler.FilePath) != "go" || (handler.Kind != graph.KindFunction && handler.Kind != graph.KindMethod) {
		return empty, fmt.Errorf("%w: no checked Go binding source for %q", graph.ErrContractProjectionStale, symbol)
	}
	base, facts, err := r.forLinkedDeclaration(ctx, handler)
	if err != nil {
		return empty, err
	}
	seen := false
	for _, saved := range base.Nodes {
		if saved.ID == symbol {
			if seen || saved.Name != handler.Name {
				return empty, fmt.Errorf("%w: ambiguous or renamed binding declaration %q", graph.ErrContractProjectionStale, symbol)
			}
			seen = true
		}
	}
	// store_pebble.checkContractParseFacts requires a Handlers entry for
	// every function or method; the kind check and forLinkedDeclaration
	// above prove the saved node is one.
	return facts.Handlers[symbol], nil
}

func rebuildResolveCallBinding(ctx context.Context, view store_pebble.RebuildView, records *store_pebble.RebuildContractRecords,
	repo string, binding contracts.Binding,
) (store_pebble.RebuildCallReturnBinding, error) {
	name, repeated := binding.TypeID, binding.Repeated
	if contractBindingIsCall(binding) {
		call, err := rebuildCallReturnName(ctx, view, contractCallKey{callExpr: binding.CallExpr, repoPrefix: repo})
		if err != nil {
			return store_pebble.RebuildCallReturnBinding{}, err
		}
		name, repeated = call.typeName, call.repeated
	}
	if name == "" {
		return store_pebble.RebuildCallReturnBinding{}, nil
	}
	id, err := records.ResolveCallReturnTypeName(repo, name)
	return store_pebble.RebuildCallReturnBinding{TypeID: id, Repeated: repeated}, err
}

func applyRebuildCallReturnBinding(records *store_pebble.RebuildContractRecords, c contracts.Contract) error {
	lookup := func(name string) (string, bool, error) {
		binding, err := records.CallReturnBinding(rebuildCallReturnSite(c, name))
		return binding.TypeID, binding.Repeated, err
	}
	typeID, repeated, err := planContractResponse(c, lookup)
	if err != nil {
		return err
	}
	env, changed, err := planContractEnvelope(c, lookup)
	if err != nil || (typeID == "" && !changed) {
		return err
	}
	fullyTyped := changed && envelopeFullyTyped(env)
	return records.UpdateByID(c.ID, func(item *store_pebble.RebuildContractRecord) (bool, error) {
		if !contractReturnPatchTarget(item.Contract, c) {
			return false, nil
		}
		if typeID != "" {
			applyContractResponse(&item.Contract, typeID, repeated)
		}
		if changed {
			applyContractEnvelope(&item.Contract, env, fullyTyped)
		}
		return true, nil
	})
}

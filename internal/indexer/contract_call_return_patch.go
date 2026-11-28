package indexer

import (
	"strings"

	"github.com/zzet/gortex/internal/contracts"
)

type contractReturnLookup func(name string) (typeID string, repeated bool, err error)

func planContractResponse(c contracts.Contract, lookup contractReturnLookup) (string, bool, error) {
	if rt, _ := c.Meta["response_type"].(string); rt == "" {
		if name := contractResponseVarName(c); name != "" {
			return lookup(name)
		}
	}
	return "", false, nil
}

func planContractEnvelope(c contracts.Contract, lookup contractReturnLookup) ([]map[string]any, bool, error) {
	env, _ := c.Meta["response_envelope"].([]map[string]any)
	var changed bool
	for _, row := range env {
		if t, _ := row["type"].(string); t != "" {
			continue
		}
		expr, _ := row["expr"].(string)
		name := strings.TrimLeft(expr, "&*")
		if !isLikelyIdentifier(name) {
			continue
		}
		typeID, repeated, err := lookup(name)
		if err != nil {
			return nil, false, err
		}
		if typeID != "" {
			row["type"] = typeID
			if repeated {
				row["repeated"] = true
			}
			changed = true
		}
	}
	return env, changed, nil
}

func contractReturnPatchTarget(item, c contracts.Contract) bool {
	return item.Role == contracts.RoleProvider && item.SymbolID == c.SymbolID
}

func applyContractResponse(item *contracts.Contract, typeID string, repeated bool) {
	if item.Meta == nil {
		item.Meta = map[string]any{}
	}
	item.Meta["response_type"] = typeID
	if repeated {
		item.Meta["response_repeated"] = true
	}
	item.Meta["schema_source"] = "extracted"
	delete(item.Meta, "response_expr")
}

func applyContractEnvelope(item *contracts.Contract, env []map[string]any, fullyTyped bool) {
	if item.Meta == nil {
		item.Meta = map[string]any{}
	}
	item.Meta["response_envelope"] = env
	if fullyTyped {
		item.Meta["schema_source"] = "extracted"
	}
}

func contractBindingIsCall(b contracts.Binding) bool {
	return b.Kind == contracts.BindingMethodCall || b.Kind == contracts.BindingFuncCall
}

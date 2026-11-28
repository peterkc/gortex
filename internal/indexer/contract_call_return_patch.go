package indexer

import (
	"strings"

	"github.com/zzet/gortex/internal/contracts"
)

// contractReturnLookup resolves a variable using the prepared binding facts.
// Native lookup is in-memory and never returns an error; rebuild can fail a read.
type contractReturnLookup func(name string) (typeID string, repeated bool, err error)

// planContractResponse traces an unresolved response_expr. It accepts a bare
// identifier or an older helper call with that identifier as the third argument.
func planContractResponse(c contracts.Contract, lookup contractReturnLookup) (string, bool, error) {
	if rt, _ := c.Meta["response_type"].(string); rt == "" {
		if name := contractResponseVarName(c); name != "" {
			return lookup(name)
		}
	}
	return "", false, nil
}

// planContractEnvelope resolves individual fields independently of the top-level
// response_type. It modifies the shared envelope rows in place, including any
// rows resolved before an error from lookup.
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

// applyContractEnvelope stores the updated rows and promotes schema_source to
// extracted only if every row is typed. Otherwise the existing source stays.
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

package indexer

import (
	"strings"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// inlineRebuildContractShapes folds the checked shape snapshot into the live
// records without attaching shapes to ordinary graph nodes.
func inlineRebuildContractShapes(records *store_pebble.RebuildContractRecords, shapeAt func(string) (*contracts.Shape, error)) error {
	lookup := func(raw any) (*contracts.Shape, error) {
		id, _ := raw.(string)
		if !strings.Contains(id, "::") {
			return nil, nil
		}
		return shapeAt(id)
	}
	return records.VisitCurrentRepresentatives(func(c store_pebble.RebuildContractRecord) error {
		if c.BodyUnproved {
			// Do not open stale shape facts. Keep the native write targets'
			// identities, but mark their payloads for D83 omission.
			return records.UpdateByID(c.ID, func(item *store_pebble.RebuildContractRecord) (bool, error) {
				if item.BodyUnproved || item.SymbolID != c.SymbolID || item.Role != contracts.RoleProvider {
					return false, nil
				}
				item.BodyUnproved = true
				return true, nil
			})
		}
		env, _ := c.Meta["response_envelope"].([]map[string]any)
		var envChanged bool
		for _, row := range env {
			if _, has := row["shape"]; has {
				continue
			}
			shape, err := lookup(row["type"])
			if err != nil {
				return err
			}
			if shape != nil {
				row["shape"] = shape
				envChanged = true
			}
		}
		top := make(map[string]*contracts.Shape, 2)
		for _, field := range [...]struct{ ref, shape string }{
			{"response_type", "response_shape"}, {"request_type", "request_shape"},
		} {
			if _, has := c.Meta[field.shape]; has {
				continue
			}
			shape, err := lookup(c.Meta[field.ref])
			if err != nil {
				return err
			}
			if shape != nil {
				top[field.shape] = shape
			}
		}
		if !envChanged && len(top) == 0 {
			return nil
		}
		// Native also mutates a non-provider representative's own shared
		// envelope. Other non-provider rows do not get those writes.
		var self bool
		return records.UpdateByID(c.ID, func(item *store_pebble.RebuildContractRecord) (bool, error) {
			if item.SymbolID != c.SymbolID {
				return false, nil
			}
			if item.Role != contracts.RoleProvider {
				if !envChanged || self || item.Role != c.Role || item.FilePath != c.FilePath {
					return false, nil
				}
				self = true
			}
			if item.Meta == nil {
				item.Meta = map[string]any{}
			}
			if envChanged {
				item.Meta["response_envelope"] = env
			}
			if item.Role == contracts.RoleProvider {
				for key, shape := range top {
					item.Meta[key] = shape
				}
			}
			return true, nil
		})
	})
}

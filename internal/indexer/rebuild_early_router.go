package indexer

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

var errRebuildRouterFactsMissing = errors.New("rebuild router facts are missing")

// joinRebuildRouterPrefixes reuses the native rewrite over the same file
// census. available contains only checked answers, including proved-empty
// answers. A selected file with no answer is not a proved-empty mount.
func joinRebuildRouterPrefixes(ctx context.Context, view store_pebble.RebuildView, reg *contracts.Registry,
	repo, workspace string, available map[string]contracts.RouterFacts,
) error {
	if !routerPrefixScanEnabled(reg) {
		return nil
	}
	selected := make(map[string]contracts.RouterFacts)
	for _, c := range reg.All() {
		if contracts.RouterContractSource(c) {
			if f, ok := available[c.FilePath]; ok {
				selected[c.FilePath] = f
			}
		}
	}
	var missingCensus string
	var scan bool
	if err := view.VisitFileIdentities(ctx, func(path, language, nodeRepo, nodeWorkspace string) error {
		if nodeRepo != repo || workspace != "" && nodeWorkspace != workspace ||
			!contracts.RouterFileCensus(strings.ToLower(path), strings.ToLower(language)) {
			return nil
		}
		scan = true
		if f, ok := available[path]; ok {
			selected[path] = f
		} else if missingCensus == "" || path < missingCensus {
			missingCensus = path
		}
		return nil
	}); err != nil {
		return err
	}
	if !scan {
		return nil
	}
	if missing := contracts.JoinRebuildRouterPrefixFacts(reg, selected, missingCensus); missing != "" {
		return fmt.Errorf("%w: %q", errRebuildRouterFactsMissing, missing)
	}
	return ctx.Err()
}

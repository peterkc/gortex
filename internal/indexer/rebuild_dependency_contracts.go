package indexer

import (
	"context"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// Seed only the minimal native pre-resolve rows. Existing primary keys win,
// even if their saved rows are not eligible for dependency lookup. Full
// contract records and their owning edges belong to the later contract pass.
func seedRebuildDependencyContracts(ctx context.Context, build *store_pebble.BuildStore, root rebuildGoManifest) error {
	view := build.RebuildView()
	var last string
	for _, module := range root.DirectDeps {
		id := "dep::" + module
		existing, err := view.Node(ctx, id)
		if err != nil {
			return err
		}
		if existing != nil {
			continue
		}
		build.AddNode(&graph.Node{
			ID: id, Kind: graph.KindContract, Name: id, FilePath: "go.mod", Language: "contract",
			Meta: map[string]any{"type": string(contracts.ContractDependency), "role": string(contracts.RoleConsumer)},
		})
		last = id
	}
	if last != "" {
		// AddNode records failure in the build; the checked view reads it.
		_, err := view.Node(ctx, last)
		return err
	}
	return ctx.Err()
}

package indexer

import (
	"context"
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/pathkey"
)

// A non-nil empty map proves the set was built. Values are the native
// contract-node paths that the overlay writes as each node's FilePath.
type rebuildEarlyContractIDs map[string]string

func (ids rebuildEarlyContractIDs) contains(id string) bool {
	_, ok := ids[id]
	return ok
}

type rebuildEarlyContractFacts struct {
	registry           *contracts.Registry
	receipts           rebuildContractReceiptReader
	routers            map[string]contracts.RouterFacts
	routerOverlapFiles []string
}

// readRebuildEarlyContractFacts runs at the post-parse, pre-main cut. It
// answers endpoint questions from the selected parse graph and constant
// sidecar, not from a graph that main has changed.
func (idx *Indexer) readRebuildEarlyContractFacts(ctx context.Context, view store_pebble.RebuildView,
	job rebuildFileStages, store rebuildParseStore, census *rebuildExtractorCensus,
	rootDeps []string, markIsolationAffected func(string, contracts.ContractType, string) error,
) (*rebuildEarlyContractFacts, error) {
	reader := rebuildContractReceiptReader{idx: idx, job: job, store: store, census: census, linkView: view, markIsolationAffected: markIsolationAffected}
	out := &rebuildEarlyContractFacts{registry: contracts.NewRegistry(), receipts: reader, routers: make(map[string]contracts.RouterFacts)}
	if err := reader.visitFilesAtParseCut(ctx, view, func(path string, records []contracts.Contract, facts *contracts.FileFacts) error {
		out.registry.AddAllScoped(records, idx.repoPrefix, idx.workspaceID, idx.projectID)
		if facts.Router != nil {
			out.routers[pathkey.Normalize(path)] = *facts.Router
		}
		return nil
	}); err != nil {
		return nil, err
	}
	// The checked root fact contains precisely GoModExtractor's IDs for the
	// admitted empty tracked-repo set, regardless of which IDs seed a row.
	for _, module := range rootDeps {
		out.registry.Add(contracts.Contract{
			ID: "dep::" + module, Type: contracts.ContractDependency, Role: contracts.RoleConsumer, FilePath: "go.mod",
		})
	}
	return out, ctx.Err()
}

type rebuildEndpointConstants struct {
	ctx  context.Context
	view store_pebble.RebuildView
	err  error
}

// lookup streams the native name group without retaining its declarations.
// Read errors and limit excess are sticky, not absent constant answers.
func (s *rebuildEndpointConstants) lookup(name, file, repo string) string {
	name = strings.TrimSpace(name)
	if name == "" || s.err != nil {
		return ""
	}
	var values contracts.EndpointConstantValues
	count := 0
	prefix := repo + "/"
	s.err = s.view.VisitNameCandidates(s.ctx, name, nil, func(n *graph.Node) error {
		// Native charges every exact-name candidate before kind and repo filters.
		count++
		if count > graph.ContractProjectionRowLimit {
			return graph.ErrContractProjectionLimit
		}
		if n.Kind != graph.KindConstant || repo != "" && !strings.HasPrefix(n.ID, prefix) && !strings.HasPrefix(n.FilePath, prefix) {
			return nil
		}
		row, err := s.view.ConstantValue(s.ctx, n.ID)
		if err != nil || row == nil {
			return err
		}
		values.Add(row.Value, file != "" && n.FilePath == file)
		return nil
	})
	if s.err != nil {
		return ""
	}
	value, _ := values.Value()
	return value
}

// finish runs after main. Collect only native DI records, without running
// the Spring linkage writer, and let the native join rewrite route IDs.
func (f *rebuildEarlyContractFacts) finish(ctx context.Context, view store_pebble.RebuildView, repo, workspace, project string) (rebuildEarlyContractIDs, error) {
	if err := view.VisitDIContractEdges(ctx, repo, func(e *graph.Edge) error {
		if c, ok := diContractFromEdge(e); ok {
			c.RepoPrefix, c.WorkspaceID, c.ProjectID = repo, workspace, project
			f.registry.Add(c)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := f.bindRouterHandlerIDs(ctx, view); err != nil {
		return nil, err
	}
	overlapFiles, err := joinRebuildRouterPrefixes(ctx, view, f.registry, repo, workspace, f.routers)
	if err != nil {
		return nil, err
	}
	f.routerOverlapFiles = overlapFiles
	ids := make(rebuildEarlyContractIDs)
	for _, c := range f.registry.All() {
		// contractGraphRows sorts by file path first within each ID.
		if prior, ok := ids[c.ID]; !ok || c.FilePath < prior {
			ids[c.ID] = c.FilePath
		}
	}
	return ids, ctx.Err()
}

// bindRouterHandlerIDs replays the symbol moves that can change the native
// Registry.All doors for the router join. Body enrichment and bare type
// upgrades change schema metadata, not the router fields or registry keys.
func (f *rebuildEarlyContractFacts) bindRouterHandlerIDs(ctx context.Context, view store_pebble.RebuildView) error {
	// Freeze the representative tasks before changing any live bucket.
	pass := f.receipts.handlerPass()
	for _, task := range f.registry.All() {
		trail, fallback, _ := contracts.ProviderHandlerHints(task)
		if trail == "" && fallback == "" {
			continue
		}
		items := f.registry.ByID(task.ID)
		var routerBucket bool
		for _, c := range items {
			if contracts.RouterContractSource(c) {
				routerBucket = true
				break
			}
		}
		if !routerBucket {
			continue
		}
		winner, err := pass.providerHandler(ctx, view, task)
		if err != nil {
			return err
		}
		if winner == nil || winner.ID == task.SymbolID {
			continue
		}
		// Native moves the symbol only when it can read the handler source.
		// The early capture already checked every admitted receipt. The ID
		// move does not use a body range or any body-derived schema value.
		origin := f.receipts.census.originByGraphPath[winner.FilePath]
		if f.receipts.census.byPath[origin] == "" && !f.receipts.sizeSkippedContractSource(winner.FilePath) {
			return fmt.Errorf("%w: no admitted router handler source for %q", graph.ErrContractProjectionStale, winner.ID)
		}
		for i, c := range items {
			if c.Role == contracts.RoleProvider && c.RepoPrefix == task.RepoPrefix && c.FilePath == task.FilePath &&
				c.SymbolID == task.SymbolID && c.Line == task.Line {
				items[i].SymbolID = winner.ID
			}
		}
		f.registry.ReplaceByID(task.ID, items)
	}
	return ctx.Err()
}

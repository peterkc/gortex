package indexer

import (
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/pathkey"
)

// A non-nil empty map proves the set was built. Values are the native
// contract-node file paths, needed by gRPC registration's untyped read.
type rebuildEarlyContractIDs map[string]string

func (ids rebuildEarlyContractIDs) contains(id string) bool {
	_, ok := ids[id]
	return ok
}

type rebuildEarlyContractFacts struct {
	registry           *contracts.Registry
	routers            map[string]contracts.RouterFacts
	routerOverlapFiles []string
}

// readRebuildEarlyContractFacts runs at the post-parse, pre-main cut. It
// answers endpoint questions from the complete parse graph and constant
// sidecar, not from a graph that main has changed.
func (idx *Indexer) readRebuildEarlyContractFacts(ctx context.Context, view store_pebble.RebuildView,
	job rebuildFileStages, store rebuildParseStore, census *rebuildExtractorCensus,
	rootDeps []string,
) (*rebuildEarlyContractFacts, error) {
	out := &rebuildEarlyContractFacts{registry: contracts.NewRegistry(), routers: make(map[string]contracts.RouterFacts)}
	constants := rebuildEndpointConstants{ctx: ctx, view: view}
	for _, input := range job.inputs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		recipe := idx.rebuildExtractorRecipe(job.recipes[input.Language], input)
		raw, err := store.read(input.parseKey(recipe))
		if err != nil {
			return nil, fmt.Errorf("%w: reload %q for early contracts: %w", errParseEntryCorrupt, input.Path, err)
		}
		base, facts, err := idx.decodeRebuildFileReceipt(input, raw)
		if err != nil {
			return nil, err
		}
		if base.EffectiveLanguage != census.byPath[input.Path] {
			return nil, fmt.Errorf("%w: early contract receipt attribution for %q", errParseEntryCorrupt, input.Path)
		}
		// The checked receipt already enforces question positions. Keep
		// the resolver's defensive error rather than treating a failure as empty.
		records, err := contracts.ResolveEndpointQuestions(facts.Records, facts.Questions, &constants)
		if err != nil {
			return nil, fmt.Errorf("%w: endpoint questions for %q: %w", errParseEntryCorrupt, input.Path, err)
		}
		if constants.err != nil {
			return nil, fmt.Errorf("resolve endpoint constants for %q: %w", input.Path, constants.err)
		}
		out.registry.AddAllScoped(records, idx.repoPrefix, idx.workspaceID, idx.projectID)
		if facts.Router != nil {
			out.routers[pathkey.Normalize(input.Path)] = *facts.Router
		}
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

func (s *rebuildEndpointConstants) FindNodesByNames(names []string) map[string][]*graph.Node {
	out := make(map[string][]*graph.Node)
	for _, name := range names {
		if s.err != nil {
			return nil
		}
		s.err = s.view.VisitNameCandidates(s.ctx, name, nil, func(n *graph.Node) error {
			if n.Kind == graph.KindConstant {
				out[name] = append(out[name], n)
			}
			return nil
		})
	}
	return out
}

func (s *rebuildEndpointConstants) ConstantValuesByNodeIDs(ids []string) (map[string]string, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		row, err := s.view.ConstantValue(s.ctx, id)
		if err != nil {
			s.err = err
			return nil, err
		}
		if row != nil {
			out[id] = row.Value
		}
	}
	return out, nil
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

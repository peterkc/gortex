package indexer

import (
	"context"
	"strings"

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
	reader := rebuildContractReceiptReader{idx: idx, job: job, store: store, census: census}
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

type rebuildEndpointValue struct {
	value     string
	ambiguous bool
}

func (v *rebuildEndpointValue) add(s string) {
	if s == "" {
		return
	}
	if v.value == "" {
		v.value = s
	} else if v.value != s {
		v.ambiguous = true
	}
}

// lookup reduces identical values without retaining a name group, even for a
// name declared in many files. Preserve same-file preference and repo-wide
// fallback in resolveConstIdentifier. Errors are sticky, not native misses.
func (s *rebuildEndpointConstants) lookup(name, file, repo string) string {
	name = strings.TrimSpace(name)
	if name == "" || s.err != nil {
		return ""
	}
	var sameFile, anywhere rebuildEndpointValue
	prefix := repo + "/"
	s.err = s.view.VisitNameCandidates(s.ctx, name, nil, func(n *graph.Node) error {
		if n.Kind != graph.KindConstant || repo != "" && !strings.HasPrefix(n.ID, prefix) && !strings.HasPrefix(n.FilePath, prefix) {
			return nil
		}
		row, err := s.view.ConstantValue(s.ctx, n.ID)
		if err != nil || row == nil {
			return err
		}
		anywhere.add(row.Value)
		if file != "" && n.FilePath == file {
			sameFile.add(row.Value)
		}
		return nil
	})
	switch {
	case s.err != nil:
		return ""
	case !sameFile.ambiguous && sameFile.value != "":
		return sameFile.value
	case !anywhere.ambiguous:
		return anywhere.value
	default:
		return ""
	}
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

package resolver

import (
	"context"
	"iter"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildEFCoreModels runs after C# interface dispatch and before Sidekiq.
func RunRebuildEFCoreModels(ctx context.Context, build *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (uint64, error) {
	if !selection.allows(SynthCSharpEFCoreModels) {
		return 0, nil
	}
	return build.RebuildEFCoreModels(ctx, receipts, store_pebble.RebuildEFCoreRules{
		Fact:     rebuildEFCoreFact,
		Names:    rebuildEFCoreNames,
		BareName: csharpEFBareName,
		RoleNode: rebuildEFCoreRoleNode,
		Owned:    csharpEFOwnsModelsTableEdge,
		SameEdge: csharpEFEdgesEqual,
		Project:  rebuildEFCoreProjection,
		Table:    csharpEFTableNodeForEdge,
	})
}

func rebuildEFCoreFact(n *graph.Node) (keep, active bool) {
	if n.Kind == graph.KindFile {
		active = len(csharpEFActionsFromFile(n)) != 0
		return active, active
	}
	if !strings.EqualFold(n.Language, "csharp") {
		return false, false
	}
	switch n.Kind {
	case graph.KindType:
		_, active = csharpEFAttributeMapping(n)
		return true, active // unmarked names can still make a target ambiguous
	case graph.KindField:
		_, active = csharpDbSetFactFromNode(n)
		return active, active
	default:
		return false, false
	}
}

// Rebuild retains only these receipt fields; use the same native readers to
// decide which class-name cohorts need their full declarations.
func rebuildEFCoreRoleNode(n *graph.Node) *graph.Node {
	role := &graph.Node{ID: n.ID, Kind: n.Kind, Name: n.Name, Language: n.Language, FilePath: n.FilePath,
		StartLine: n.StartLine, RepoPrefix: n.RepoPrefix, WorkspaceID: n.WorkspaceID}
	var keys []string
	switch n.Kind {
	case graph.KindFile:
		keys = []string{"ef_fluent"}
	case graph.KindField:
		keys = []string{"kind", "field_type"}
	case graph.KindType:
		keys = []string{"ef_attribute_table", "ef_attribute_schema", "ef_config_entity", "ef_config_table", "ef_config_schema", "ef_config_relation"}
	}
	for _, key := range keys {
		if val, ok := n.Meta[key]; ok {
			if role.Meta == nil {
				role.Meta = make(map[string]any)
			}
			role.Meta[key] = val
		}
	}
	return role
}

func rebuildEFCoreNames(n *graph.Node, visit func(string) error) error {
	switch n.Kind {
	case graph.KindFile:
		for _, action := range csharpEFActionsFromFile(n) {
			if action.kind == csharpEFActionMapping {
				if err := visit(action.mapping.entity); err != nil {
					return err
				}
			}
		}
	case graph.KindField:
		if fact, ok := csharpDbSetFactFromNode(n); ok {
			return visit(fact.entity)
		}
	case graph.KindType:
		if cfg, ok := csharpEFConfigFactFromNode(n); ok {
			return visit(cfg.mapping.entity)
		}
	}
	return nil
}

func rebuildEFCoreProjection(nodes []*graph.Node) func(*graph.Node, []*graph.Edge) (*graph.Edge, bool) {
	classes := make(map[string][]*graph.Node)
	byName := make(map[string][]csharpEFConfigFact)
	configsByEntity := make(map[string][]csharpEFConfigFact)
	var actions []csharpEFAction
	var dbsets []csharpDbSetFact
	collect := csharpEFCollectInput(nil, classes, byName, configsByEntity, &actions, &dbsets, func(cfg csharpEFConfigFact) string {
		return csharpEFNameKey(cfg.repoPrefix, cfg.workspaceID, cfg.mapping.entity)
	})
	for _, n := range nodes {
		collect(n)
	}
	// Reuse the native action reducer one entity at a time. An assembly
	// action can otherwise retain a decision per context and per entity.
	for _, configs := range configsByEntity {
		sort.Slice(configs, func(i, j int) bool { return configs[i].siteID < configs[j].siteID })
	}
	sets := csharpEFResolveDbSets(dbsets, classes)
	return func(n *graph.Node, current []*graph.Edge) (*graph.Edge, bool) {
		key := csharpEFNameKey(n.RepoPrefix, n.WorkspaceID, n.Name)
		var decision csharpEFDecision
		if group := classes[key]; len(group) == 1 && len(actions) != 0 {
			entity := map[string][]*graph.Node{key: group}
			assembly := map[string][]csharpEFConfigFact{csharpEFBoundaryKey(n.RepoPrefix, n.WorkspaceID): configsByEntity[key]}
			decision = csharpEFReduceActions(actions, entity, byName, assembly)[n.ID]
		}
		return csharpEFProjectionChange(n, decision, sets[n.ID], current)
	}
}

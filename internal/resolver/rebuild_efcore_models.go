package resolver

import (
	"context"
	"iter"
	"reflect"
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
		SameFact: sameRebuildEFCoreFact,
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

func sameRebuildEFCoreFact(raw, live *graph.Node) bool {
	if raw.Kind != live.Kind || raw.RepoPrefix != live.RepoPrefix || raw.WorkspaceID != live.WorkspaceID {
		return false
	}
	switch live.Kind {
	case graph.KindFile:
		return reflect.DeepEqual(csharpEFActionsFromFile(raw), csharpEFActionsFromFile(live))
	case graph.KindField:
		a, aOK := csharpDbSetFactFromNode(raw)
		b, bOK := csharpDbSetFactFromNode(live)
		return aOK == bOK && a == b
	default:
		a, aOK := csharpEFAttributeMapping(raw)
		b, bOK := csharpEFAttributeMapping(live)
		ac, acOK := csharpEFConfigFactFromNode(raw)
		bc, bcOK := csharpEFConfigFactFromNode(live)
		return raw.Name == live.Name && aOK == bOK && acOK == bcOK && reflect.DeepEqual(a, b) && reflect.DeepEqual(ac, bc)
	}
}

func rebuildEFCoreProjection(nodes []*graph.Node) func(*graph.Node, []*graph.Edge) (*graph.Edge, bool) {
	classes := make(map[string][]*graph.Node)
	byName := make(map[string][]csharpEFConfigFact)
	configsByEntity := make(map[string][]csharpEFConfigFact)
	var actions []csharpEFAction
	var dbsets []csharpDbSetFact
	for _, n := range nodes {
		switch n.Kind {
		case graph.KindFile:
			actions = append(actions, csharpEFActionsFromFile(n)...)
		case graph.KindType:
			key := csharpEFNameKey(n.RepoPrefix, n.WorkspaceID, n.Name)
			classes[key] = append(classes[key], n)
			if cfg, ok := csharpEFConfigFactFromNode(n); ok {
				key := csharpEFNameKey(cfg.repoPrefix, cfg.workspaceID, cfg.name)
				byName[key] = append(byName[key], cfg)
				entity := csharpEFNameKey(cfg.repoPrefix, cfg.workspaceID, cfg.mapping.entity)
				configsByEntity[entity] = append(configsByEntity[entity], cfg)
			}
		case graph.KindField:
			if fact, ok := csharpDbSetFactFromNode(n); ok {
				dbsets = append(dbsets, fact)
			}
		}
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

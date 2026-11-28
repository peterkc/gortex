package indexer

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// providerHandler selects a handler for one of the original Registry.All
// representatives, before any handler updates change the representative keys.
// A nil winner leaves registration ownership intact. The caller still applies
// changes to matching live ByID siblings, not only to this representative.
func (r rebuildContractReceiptReader) providerHandler(ctx context.Context, view store_pebble.RebuildView, c contracts.Contract) (*graph.Node, error) {
	trail, fallback, class := contracts.ProviderHandlerHints(c)
	if trail == "" && fallback == "" {
		return nil, ctx.Err()
	}
	if class != "" {
		key, _ := contracts.LaravelHandlerClassKey(c)
		origin := r.census.originByGraphPath[c.FilePath]
		i, ok := slices.BinarySearchFunc(r.job.inputs, origin, func(in rebuildInput, key string) int {
			return strings.Compare(in.Path, key)
		})
		if !ok {
			return nil, fmt.Errorf("%w: no admitted handler registration for %q", graph.ErrContractProjectionStale, c.FilePath)
		}
		_, facts, err := r.load(ctx, r.job.inputs[i])
		if err != nil {
			return nil, err
		}
		class, ok = facts.LaravelHandlerClasses[key]
		if !ok {
			return nil, fmt.Errorf("%w: no handler class answer for %q at %s:%d", graph.ErrContractProjectionStale, c.ID, c.FilePath, c.Line)
		}
	}
	handler, err := selectRebuildProviderHandler(ctx, view, trail, fallback, c.RepoPrefix, path.Dir(c.FilePath), class)
	if err != nil || handler == nil || handler.FilePath == "" || handler.RepoPrefix != c.RepoPrefix {
		return nil, err
	}
	return handler, nil
}

func selectRebuildProviderHandler(ctx context.Context, view store_pebble.RebuildView, trail, fallback, repo, dir, class string) (*graph.Node, error) {
	var ownerID string
	if class != "" {
		short := class[strings.LastIndex(class, `\`)+1:]
		var ambiguous bool
		err := view.VisitNameCandidates(ctx, short, nil, func(n *graph.Node) error {
			if n.Kind != graph.KindType {
				return nil
			}
			if short != class {
				ns, _ := n.Meta["scope_ns"].(string)
				if strings.Trim(ns+`\`+n.Name, `\`) != strings.TrimPrefix(class, `\`) {
					return nil
				}
			}
			ambiguous = ownerID != ""
			ownerID = n.ID
			return nil
		})
		if err != nil || ownerID == "" || ambiguous {
			return nil, err
		}
	}
	trailNames := contracts.HandlerCandidatesInTrail(trail)
	if len(trailNames) == 0 && fallback != "" {
		trailNames = []string{fallback}
	}
	var best *graph.Node
	for _, ident := range trailNames {
		name := handlerLookupName(ident)
		if name == "" {
			continue
		}
		winner, err := pickRebuildHandler(ctx, view, name, repo, dir, ownerID)
		if err != nil {
			return nil, err
		}
		if winner != nil {
			best = winner
		}
	}
	return best, ctx.Err()
}

func pickRebuildHandler(ctx context.Context, view store_pebble.RebuildView, name, repo, dir, ownerID string) (*graph.Node, error) {
	// Native selection depends only on uniqueness in each repo pool and
	// uniqueness after narrowing that pool to the registration directory.
	// Keep at most two witnesses per disjoint group; extra rows cannot
	// change either decision. An exact owned method overrides these pools.
	var pools [2][2][]*graph.Node
	var owned *graph.Node
	err := view.VisitNameCandidates(ctx, name, nil, func(n *graph.Node) error {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			return nil
		}
		if ownerID != "" && n.Kind == graph.KindMethod && n.ID == ownerID+"."+name {
			owned = n
			return nil
		}
		r, d := 0, 0
		if repo != "" && strings.HasPrefix(n.ID, repo+"/") {
			r = 1
		}
		if path.Dir(n.FilePath) == dir {
			d = 1
		}
		if len(pools[r][d]) < 2 {
			pools[r][d] = append(pools[r][d], n)
		}
		return nil
	})
	if err != nil || owned != nil {
		return owned, err
	}
	var candidates [8]*graph.Node
	kept := candidates[:0]
	for _, repoPool := range pools {
		for _, dirPool := range repoPool {
			kept = append(kept, dirPool...)
		}
	}
	return pickHandlerCandidate(kept, repo, dir), nil
}

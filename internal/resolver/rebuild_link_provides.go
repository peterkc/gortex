package resolver

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

const (
	rebuildProvidesRows  = 16 << 10
	rebuildProvidesBytes = 16 << 20
)

// rebuildProvidesAbstract applies the native index's metadata gate without
// filtering by the provider's language, caller selection or endpoint.
func rebuildProvidesAbstract(e *graph.Edge) string {
	if e.Kind != graph.EdgeProvides {
		return ""
	}
	binding, _ := e.Meta[graph.MetaDIBinding].(string)
	abstract, _ := e.Meta[graph.MetaDIProvidesFor].(string)
	if binding != graph.DIBindingUseClass {
		return ""
	}
	return abstract
}

func rebuildProvidesName(target string) string {
	if graph.IsUnresolvedTarget(target) {
		return graph.UnresolvedName(target)
	}
	if cut := strings.LastIndex(target, "::"); cut >= 0 {
		return target[cut+2:]
	}
	return target
}

// captureRebuildProvides reads committed logical rows at the first native
// preparation trigger. Only distinct abstract/concrete pairs survive the
// read; a nonnil empty map certifies a successful empty census.
func captureRebuildProvides(ctx context.Context, view store_pebble.RebuildView) (map[string]map[string]struct{}, error) {
	idx := make(map[string]map[string]struct{})
	pairs, remaining := 0, rebuildProvidesBytes
	err := view.VisitEdges(ctx, func(e *graph.Edge) error {
		abstract := rebuildProvidesAbstract(e)
		if abstract == "" {
			return nil
		}
		name := rebuildProvidesName(e.To)
		if _, ok := idx[abstract][name]; ok {
			return nil
		}
		if pairs == rebuildProvidesRows || len(abstract) > remaining || len(name) > remaining-len(abstract) {
			return fmt.Errorf("%w: useClass census", store_pebble.ErrRebuildLinkInputSize)
		}
		pairs++
		remaining -= len(abstract) + len(name)
		if idx[abstract] == nil {
			idx[strings.Clone(abstract)] = make(map[string]struct{})
		}
		idx[abstract][strings.Clone(name)] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return idx, nil
}

// checkRebuildProvidesStationarity proves that moving a useClass row before
// or after the first wildcard cannot change the census. The native pending
// pager and the private frontier use different orders and page bounds;
// receipt selection also defers some providers. Main changes no nodes and
// defers source repoints until after every page. A token move can only choose
// an existing exact compatible-name candidate. At a shared Provides site we
// must prove that no sibling can overwrite a useClass row at any such key.
func checkRebuildProvidesStationarity(ctx context.Context, view store_pebble.RebuildView) error {
	type site struct {
		from, file string
		line       int
	}
	type siteGroup struct {
		site
		useClass bool
		targets  map[string]int // possible complete target ID -> input row index
	}
	type provider struct {
		group             *siteGroup
		target            string
		useClass, generic bool
	}
	sites := make(map[site]*siteGroup)
	var rows []provider
	remaining := rebuildProvidesBytes
	if err := view.VisitEdges(ctx, func(e *graph.Edge) error {
		if e.Kind != graph.EdgeProvides {
			return nil
		}
		if len(rows) == rebuildProvidesRows || len(e.From) > remaining || len(e.FilePath) > remaining-len(e.From) {
			return fmt.Errorf("%w: useClass stationarity sites", store_pebble.ErrRebuildLinkInputSize)
		}
		remaining -= len(e.From) + len(e.FilePath)
		if len(e.To) > remaining {
			return fmt.Errorf("%w: useClass stationarity targets", store_pebble.ErrRebuildLinkInputSize)
		}
		remaining -= len(e.To)
		key := site{e.From, e.FilePath, e.Line}
		g := sites[key]
		if g == nil {
			key.from, key.file = strings.Clone(key.from), strings.Clone(key.file)
			g = &siteGroup{site: key}
			sites[key] = g
		}
		useClass := rebuildProvidesAbstract(e) != ""
		g.useClass = g.useClass || useClass
		generic, _ := e.Meta[graph.MetaGenericInstantiation].(bool)
		rows = append(rows, provider{group: g, target: strings.Clone(e.To), useClass: useClass, generic: generic})
		return nil
	}); err != nil {
		return err
	}
	candidates, candidateBytes := 0, rebuildProvidesBytes
	for i, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		g := row.group
		if !g.useClass {
			continue
		}
		if g.targets == nil {
			g.targets = make(map[string]int)
		}
		// Keep the old target as well as every possible destination. A pair
		// of non-useClass siblings cannot erase a binding on its own. Keep
		// the first owner so a later useClass row still detects that overlap.
		claim := func(target string) error {
			if prev, ok := g.targets[target]; ok {
				if prev != i && (row.useClass || rows[prev].useClass) {
					return fmt.Errorf("%w: useClass stationarity at %q:%d for %q: overlapping target %q", errRebuildMainRule, g.file, g.line, row.target, target)
				}
				return nil
			}
			g.targets[target] = i
			return nil
		}
		if err := claim(row.target); err != nil {
			return err
		}
		// Native's qualified pending LIKE is ASCII-insensitive. Refuse
		// nonstandard qualified spellings if they can enter that frontier.
		if !graph.IsUnresolvedTarget(row.target) {
			lower := strings.ToLower(row.target)
			if strings.Contains(lower, "::"+graph.UnresolvedMarker) && !strings.Contains(lower, "::"+graph.FnValuePlaceholderMarker) {
				return fmt.Errorf("%w: useClass stationarity for nonstandard pending target %q", errRebuildMainRule, row.target)
			}
			continue
		}
		if graph.IsFnValuePlaceholder(row.target) {
			continue // not part of native main's pending frontier
		}
		name := graph.UnresolvedName(row.target)
		switch {
		case strings.HasPrefix(name, "grpc::"), strings.HasPrefix(name, "pyrel::"):
			continue // native main leaves these targets unchanged
		case (name == "" && !strings.HasPrefix(row.target, unresolvedPrefix)),
			strings.HasPrefix(name, "import::"), strings.HasPrefix(name, "extern::"), strings.HasPrefix(name, "*."):
			return fmt.Errorf("%w: useClass stationarity for non-token target %q", errRebuildMainRule, row.target)
		}
		if name == "" {
			continue // a bare unresolved:: has no exact-name candidates
		}
		caller, err := view.Node(ctx, g.from)
		if err != nil {
			return err
		}
		var repo string
		var langs []string
		if caller != nil {
			repo = caller.RepoPrefix
			_, langs = resolverCompatibleLanguages(caller.Language)
		}
		edge := graph.Edge{}
		if row.generic {
			edge.Meta = map[string]any{graph.MetaGenericInstantiation: true}
		}
		// Charge all name hits, including language and repository misses.
		err = view.VisitNameCandidates(ctx, name, nil, func(n *graph.Node) error {
			if candidates == rebuildProvidesRows {
				return fmt.Errorf("%w: useClass stationarity candidate rows", store_pebble.ErrRebuildLinkInputSize)
			}
			candidates++
			for _, identity := range [...]string{n.ID, n.Name, n.RepoPrefix, n.Language} {
				if len(identity) > candidateBytes {
					return fmt.Errorf("%w: useClass stationarity candidate bytes", store_pebble.ErrRebuildLinkInputSize)
				}
				candidateBytes -= len(identity)
			}
			if n.RepoPrefix != repo || (len(langs) != 0 && !slices.Contains(langs, n.Language)) {
				return nil
			}
			one := [1]*graph.Node{n}
			if len(genericInstantiationOnly(&edge, withoutAttributedTerminals(one[:]))) == 0 || n.ID == row.target {
				return nil
			}
			if row.useClass && rebuildProvidesName(n.ID) != name {
				return fmt.Errorf("%w: useClass stationarity at %q:%d for %q", errRebuildMainRule, g.file, g.line, row.target)
			}
			return claim(strings.Clone(n.ID))
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// prepareRebuildProvides is also the seam for direct main-page fixtures that
// do not run the original-page selector. They own any preceding page commits.
func prepareRebuildProvides(ctx context.Context, f *rebuildLinkFrontier, page []*graph.Edge) error {
	if f.providesFor != nil || !pendingNeedsProvidesIndex(page) {
		return nil
	}
	idx, err := captureRebuildProvides(ctx, f.view)
	if err != nil {
		return err
	}
	f.providesFor = idx
	return nil
}

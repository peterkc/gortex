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
// receipt selection also defers some providers. Neither difference is safe
// without this proof. Main changes no nodes and defers source repoints until
// after every page, so a normal token move can only choose an existing exact
// compatible-name candidate. A competing Provides at the same site could
// win the destination key and erase the abstract/concrete pair.
func checkRebuildProvidesStationarity(ctx context.Context, view store_pebble.RebuildView) error {
	type site struct {
		from, file string
		line       int
	}
	type pending struct {
		site
		target  string
		generic bool
	}
	sites := make(map[site]int)
	var rows []pending
	count, remaining := 0, rebuildProvidesBytes
	if err := view.VisitEdges(ctx, func(e *graph.Edge) error {
		if e.Kind != graph.EdgeProvides {
			return nil
		}
		if count == rebuildProvidesRows || len(e.From) > remaining || len(e.FilePath) > remaining-len(e.From) {
			return fmt.Errorf("%w: useClass stationarity sites", store_pebble.ErrRebuildLinkInputSize)
		}
		count++
		remaining -= len(e.From) + len(e.FilePath)
		key := site{e.From, e.FilePath, e.Line}
		if sites[key] == 0 {
			key.from, key.file = strings.Clone(key.from), strings.Clone(key.file)
		}
		sites[key]++
		if rebuildProvidesAbstract(e) == "" {
			return nil
		}
		// Native's qualified pending LIKE is ASCII-insensitive. Refuse
		// nonstandard qualified spellings if they can enter that frontier.
		if !graph.IsUnresolvedTarget(e.To) {
			lower := strings.ToLower(e.To)
			if strings.Contains(lower, "::"+graph.UnresolvedMarker) && !strings.Contains(lower, "::"+graph.FnValuePlaceholderMarker) {
				return fmt.Errorf("%w: useClass stationarity for nonstandard pending target %q", errRebuildMainRule, e.To)
			}
			return nil
		}
		if graph.IsFnValuePlaceholder(e.To) {
			return nil // not part of native main's pending frontier
		}
		if len(e.To) > remaining {
			return fmt.Errorf("%w: useClass stationarity targets", store_pebble.ErrRebuildLinkInputSize)
		}
		remaining -= len(e.To)
		generic, _ := e.Meta[graph.MetaGenericInstantiation].(bool)
		rows = append(rows, pending{key, strings.Clone(e.To), generic})
		return nil
	}); err != nil {
		return err
	}
	candidates, candidateBytes := 0, rebuildProvidesBytes
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
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
		caller, err := view.Node(ctx, row.from)
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
			if rebuildProvidesName(n.ID) != name || sites[row.site] != 1 {
				return fmt.Errorf("%w: useClass stationarity at %q:%d for %q", errRebuildMainRule, row.file, row.line, row.target)
			}
			return nil
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

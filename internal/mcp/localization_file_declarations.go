package mcp

import (
	"context"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// localizationFileDeclarations carries a bounded declaration page together with
// the count observed while scanning the file. DeclaredKnown distinguishes an
// exact count from a saturation lower bound. Truncated means Nodes is only a
// prefix and Declared is therefore an "at least" count. Unavailable reads also
// set Truncated: an empty prefix then does not prove that the file is empty.
type localizationFileDeclarations struct {
	Nodes         []*graph.Node
	Declared      int
	DeclaredKnown bool
	Truncated     bool
}

var localizationDeclarationExcludedKinds = []graph.NodeKind{
	graph.KindFile, graph.KindImport, graph.KindLocal, graph.KindParam,
	graph.KindClosure, graph.KindGenericParam, graph.KindBuiltin,
}

// localizationFileDeclarationCache keeps one request's bounded lightweight
// declaration pages. It deliberately depends on BoundedFileNodeReader: a store
// without that capability fails closed instead of falling back to full-row
// GetFileNodes decoding.
type localizationFileDeclarationCache struct {
	ctx             context.Context
	reader          graph.Reader
	bounded         graph.BoundedFileNodeReader
	scope           graph.LocalizationNodeScope
	budget          *localizationFileRequestBudget
	byFile          map[string]localizationFileDeclarations
	definitionLimit int
}

func newLocalizationFileDeclarationCache(
	ctx context.Context,
	reader graph.Reader,
	scope graph.LocalizationNodeScope,
) *localizationFileDeclarationCache {
	return newBoundedLocalizationFileDeclarationCache(ctx, reader, scope, localizationFileNodeLimit)
}

func newBoundedLocalizationFileDeclarationCache(
	ctx context.Context,
	reader graph.Reader,
	scope graph.LocalizationNodeScope,
	definitionLimit int,
) *localizationFileDeclarationCache {
	if ctx == nil {
		ctx = context.Background()
	}
	bounded, _ := reader.(graph.BoundedFileNodeReader)
	return &localizationFileDeclarationCache{
		ctx:             ctx,
		reader:          reader,
		bounded:         bounded,
		scope:           localizationDeclarationScope(scope),
		budget:          localizationFileBudgetFor(ctx),
		byFile:          make(map[string]localizationFileDeclarations),
		definitionLimit: min(max(definitionLimit, 0), localizationFileNodeLimit),
	}
}

func localizationDeclarationScope(scope graph.LocalizationNodeScope) graph.LocalizationNodeScope {
	excluded := make(map[graph.NodeKind]bool, len(scope.ExcludeKinds)+len(localizationDeclarationExcludedKinds))
	for kind, omit := range scope.ExcludeKinds {
		if omit {
			excluded[kind] = true
		}
	}
	for _, kind := range localizationDeclarationExcludedKinds {
		excluded[kind] = true
	}
	scope.ExcludeKinds = excluded
	return scope
}

func (cache *localizationFileDeclarationCache) definitions(file string) localizationFileDeclarations {
	if cache == nil {
		return localizationFileDeclarations{}
	}
	return cache.definitionsAtLimit(file, cache.definitionLimit)
}

// outlineDefinitions gives the first two distinct page files the full bounded
// declaration projection and spends only a shallow page on every later file.
// The first read wins: a lower-ranked cached page is never silently upgraded by
// a later consumer, so one file cannot be charged twice against the request.
func (cache *localizationFileDeclarationCache) outlineDefinitions(file string, rank int) localizationFileDeclarations {
	if cache == nil {
		return localizationFileDeclarations{}
	}
	limit := localizationOutlineFileFetchLimit(rank)
	if cache.definitionLimit > 0 {
		limit = min(limit, cache.definitionLimit)
	}
	return cache.definitionsAtLimit(file, limit)
}

func (cache *localizationFileDeclarationCache) definitionsAtLimit(file string, limit int) localizationFileDeclarations {
	file = strings.TrimSpace(file)
	if cache == nil || file == "" {
		return localizationFileDeclarations{}
	}
	if declarations, cached := cache.byFile[file]; cached {
		return declarations
	}
	declarations := cache.readDefinitions(file, limit)
	cache.byFile[file] = declarations
	return declarations
}

// boundedDefinitions avoids retaining a generated file's whole declaration set
// for supporting body evidence. A cached outline page can be sliced safely; an
// uncached file gets its own bounded projection and does not poison the outline
// cache with a shallower page.
func (cache *localizationFileDeclarationCache) boundedDefinitions(file string, limit int) []*graph.Node {
	file = strings.TrimSpace(file)
	if cache == nil || file == "" || limit <= 0 {
		return nil
	}
	if declarations, cached := cache.byFile[file]; cached {
		return declarations.Nodes[:min(len(declarations.Nodes), limit)]
	}
	return cache.readDefinitions(file, limit).Nodes
}

func (cache *localizationFileDeclarationCache) readDefinitions(file string, limit int) localizationFileDeclarations {
	incomplete := localizationFileDeclarations{Truncated: true}
	if cache == nil || cache.bounded == nil || cache.ctx == nil || cache.ctx.Err() != nil || file == "" {
		return incomplete
	}
	if limit <= 0 || limit > localizationFileNodeLimit {
		limit = localizationFileNodeLimit
	}
	reserved := cache.budget.reserve(limit)
	if reserved <= 0 {
		return incomplete
	}
	page, err := cache.bounded.FindFileNodesBounded(cache.ctx, file, cache.scope, reserved)
	if err != nil || cache.ctx.Err() != nil {
		// The amount inspected is uncertain, so the reservation remains charged.
		return incomplete
	}
	consumed := max(page.Total, len(page.Nodes))
	cache.budget.finish(reserved, consumed)

	nodes := page.Nodes
	truncated := page.Truncated
	if len(nodes) > reserved {
		nodes = nodes[:reserved]
		truncated = true
	}
	declared := max(page.Total, len(nodes))
	if truncated {
		declared = max(declared, len(nodes)+1)
	}
	return localizationFileDeclarations{
		Nodes:         nodes,
		Declared:      declared,
		DeclaredKnown: !truncated,
		Truncated:     truncated,
	}
}

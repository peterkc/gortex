package resolver

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

var errRebuildMainRule = errors.New("rebuild main resolution rule not implemented")

// RebuildMainInputs supplies the receipt selection and frozen import facts.
// Callbacks must not change the private node view.
type RebuildMainInputs struct {
	// SelectCallers must return an order-preserving subset of each pending
	// page. The page and its edges are borrowed: do not modify or retain them.
	SelectCallers func([]*graph.Edge) ([]*graph.Edge, error)
	// Receipts replays assembled edges in frozen order for Go receiver repair.
	Receipts iter.Seq2[store_pebble.ParseFacts, error]
	// PathAlias maps a caller path and module to a graph stem. RewriteNpm
	// maps a caller path and raw import payload to a new payload. An empty
	// result with no error means a proved miss. Both are required at use.
	PathAlias, RewriteNpm func(string, string) (string, error)
	// GoOwnership supplies ownership evidence when a Go import needs it.
	GoOwnership func(GoImportCandidate) (GoPackageOwnershipResult, error)
	// CheckGoExtern proves empty tracked-module input and a checked root
	// with no applicable local replacement. A nil callback proves neither.
	CheckGoExtern func(importPath string) error
	// WorkspaceMembership returns the package-manager member of a graph file.
	// An empty result with no error proves no membership. This is not Node.WorkspaceID.
	WorkspaceMembership func(file string) (string, error)
	// ExternalNpmDependency uses the graph caller path and effective module
	// specifier without its export suffix. False with no error is a proved negative.
	ExternalNpmDependency func(caller, specifier string) (bool, error)
	// DependencyContractsReady certifies that all dependency contracts from
	// the frozen root go.mod input were seeded before the frontier opened.
	DependencyContractsReady bool
	// SkipPlaceholderSources carries the frozen native reconciliation setting.
	SkipPlaceholderSources bool
}

// RunRebuildMain resolves selected pending edges in a private build using
// frozen import facts, then reconciles placeholder sources unless disabled
// and checks weak cross-package bindings. A nonempty original frontier also
// enables Go receiver, bare-name scope, dataflow callee, generic parameter,
// Go builtin, and Go external-call attribution. It does not publish.
// SelectCallers is required. On error the owner must discard the private build.
func RunRebuildMain(ctx context.Context, build *store_pebble.BuildStore, opts store_pebble.RebuildLinkOptions, in RebuildMainInputs) error {
	if in.SelectCallers == nil {
		return errors.New("run rebuild main: missing caller selector")
	}
	return runRebuildMain(ctx, build, opts, !in.SkipPlaceholderSources, in)
}

// runRebuildMain consumes the cold resolver frontier and applies complete old
// keys in page order. Any error invalidates the entire private build,
// including earlier pages. This path does not install an LSP helper or
// retry queue, and later attribution passes are not yet dispatched.
func runRebuildMain(ctx context.Context, build *store_pebble.BuildStore, opts store_pebble.RebuildLinkOptions,
	reconcileSources bool, in RebuildMainInputs,
) (err error) {
	imports := rebuildMainImportInputs{
		jsts:                     rebuildJSTSImportInputs{pathAlias: in.PathAlias, rewriteNpm: in.RewriteNpm},
		goOwnership:              in.GoOwnership,
		checkGoExtern:            in.CheckGoExtern,
		workspaceMembership:      in.WorkspaceMembership,
		externalNpmDependency:    in.ExternalNpmDependency,
		dependencyContractsReady: in.DependencyContractsReady,
	}
	guard := newRebuildCrossPackageGuard(opts.ScratchParent, opts.SpillBytes)
	var placeholder *store_pebble.RebuildPlaceholderSources
	defer func() {
		err = errors.Join(err, guard.Close())
		if placeholder != nil {
			err = errors.Join(err, placeholder.Close())
		}
	}()
	hadPending := false
	var providesProof error
	var providesFor map[string]map[string]struct{}
	view := build.RebuildView()
	selectCallers := func(page []*graph.Edge) ([]*graph.Edge, error) {
		// Prove the projection stable across native and private page order
		// before any main move. Only the first original wildcard needs the
		// proof or the census, including pages receipt selection empties.
		if !hadPending {
			providesProof = checkRebuildProvidesStationarity(ctx, view)
		}
		if providesFor == nil && pendingNeedsProvidesIndex(page) {
			if providesProof != nil {
				return nil, providesProof
			}
			var err error
			providesFor, err = captureRebuildProvides(ctx, view)
			if err != nil {
				return nil, err
			}
		}
		// Native's cold tail is enabled by the original pending stream,
		// even if receipt selection keeps nothing or main moves no rows.
		hadPending = true
		if in.SelectCallers == nil {
			return page, nil
		}
		return in.SelectCallers(page)
	}
	err = runRebuildLinkFrontier(ctx, build, opts, selectCallers, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
		f.imports = imports
		f.providesFor = providesFor
		moves, err := reduceRebuildMainPage(ctx, f, page)
		if err != nil || len(moves) == 0 {
			return err
		}
		if err := build.ApplyLinkReindexes(ctx, moves); err != nil {
			return err
		}
		if reconcileSources {
			// Native's lazy census sees the first successful commit, including
			// any kind promotion. It must not be refreshed between pages.
			if placeholder == nil {
				placeholder, err = build.CapturePlaceholderSources(ctx, opts.SpillBytes)
				if err != nil {
					return err
				}
			}
			if err := placeholder.Record(ctx, moves); err != nil {
				return err
			}
		}
		return guard.Append(ctx, moves)
	})
	if err != nil {
		return err
	}
	if placeholder != nil {
		if _, err := placeholder.Apply(ctx); err != nil {
			return err
		}
		if err := placeholder.Close(); err != nil {
			return err
		}
	}
	// Keep native order: the guard sees settled imports and source repoints.
	if _, err := guard.Apply(ctx, build, reconcileSources, nil); err != nil {
		return err
	}
	if !hadPending {
		return nil
	}
	if _, err := build.RebuildGoMethodReceivers(ctx, in.Receipts); err != nil {
		return err
	}
	if _, err := build.RebuildBareNameScope(ctx); err != nil {
		return err
	}
	if _, err := build.RebuildDataflowCallees(ctx); err != nil {
		return err
	}
	if _, err := build.RebuildGenericParams(ctx); err != nil {
		return err
	}
	if err := runRebuildGoBuiltins(ctx, build, reconcileSources, opts.SpillBytes); err != nil {
		return err
	}
	return runRebuildGoExternalCalls(ctx, build)
}

func rebuildMainTarget(edge *graph.Edge) string {
	target := graph.UnresolvedName(edge.To)
	if target == "" {
		target = strings.TrimPrefix(edge.To, unresolvedPrefix)
	}
	return target
}

// Only the unconditional type branches of the existing dispatch. Hierarchy
// precedes wildcard; reference/instantiation follows wildcard and Vue.
func rebuildMainTypePrimary(edge *graph.Edge, target string) (primary, allowFunction bool) {
	if resolutionExempt(edge) || strings.HasPrefix(target, "grpc::") || strings.HasPrefix(target, "pyrel::") ||
		strings.HasPrefix(target, "import::") || strings.HasPrefix(target, "extern::") {
		return false, false
	}
	switch edge.Kind {
	case graph.EdgeExtends, graph.EdgeImplements, graph.EdgeComposes, graph.EdgeReturns, graph.EdgeTypedAs:
		return true, false
	case graph.EdgeInstantiates, graph.EdgeReferences:
		if strings.HasPrefix(target, "*.") || (edge.Kind == graph.EdgeReferences && strings.HasSuffix(edge.FilePath, ".vue") && edge.Meta["template"] == true) {
			return false, false
		}
		return true, true
	}
	return false, false
}

func reduceRebuildMainPage(ctx context.Context, f *rebuildLinkFrontier, page []*graph.Edge) ([]graph.EdgeReindex, error) {
	type tokenSite struct {
		index   int
		to      string
		sameDir bool
	}
	var requests []store_pebble.LinkRequest
	var tokens []tokenSite
	var typeQueries []rebuildTypeQuery
	var batch []rebuildMainPrepared
	var carry *rebuildMainPrepared
	batchEnd := 0
	updates := make([]*graph.Edge, len(page))
	for i, edge := range page {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if resolutionExempt(edge) {
			continue
		}
		target := rebuildMainTarget(edge)
		if i >= batchEnd && rebuildMainBatchFamily(edge, target) != "" {
			var err error
			batch, batchEnd, carry, err = prepareRebuildMainBatch(ctx, f, page, i, carry)
			if err != nil {
				return nil, err
			}
		}
		if len(batch) > 0 && batch[0].index == i {
			p := batch[0]
			batch[0] = rebuildMainPrepared{}
			batch = batch[1:]
			if p.err != nil {
				return nil, p.err
			}
			if err := p.query.finishRow(); err != nil {
				return nil, err
			}
			updates[i] = p.edge
			continue
		}
		primaryType, _ := rebuildMainTypePrimary(edge, target)
		switch {
		case edge.Kind == graph.EdgeCalls && phpEdgeMetaString(edge, "eloquent_model") != "":
			// The declared-static branch precedes even the tail-owned stubs.
			return nil, fmt.Errorf("%w: PHP declared static", errRebuildMainRule)
		case strings.HasPrefix(target, "grpc::"), strings.HasPrefix(target, "pyrel::"):
			continue
		case strings.HasPrefix(target, "import::"):
			e := *edge
			if err := resolveRebuildImport(ctx, f, &e, strings.TrimPrefix(target, "import::")); err != nil {
				return nil, err
			}
			updates[i] = &e
		case strings.HasPrefix(target, "extern::"):
			e := *edge
			if err := resolveRebuildExtern(ctx, f, &e, strings.TrimPrefix(target, "extern::")); err != nil {
				return nil, err
			}
			updates[i] = &e
		case primaryType:
			// Warm at first use, as the old reducer did; all other type
			// primary requests read the same immutable pre-page snapshot.
			if typeQueries == nil {
				var err error
				typeQueries, err = resolveRebuildTypePage(ctx, f, page)
				if err != nil {
					return nil, err
				}
			}
			e := *edge
			if err := typeQueries[i].finish(&e); err != nil {
				return nil, err
			}
			updates[i] = &e
		case strings.HasPrefix(target, "*."):
			e := *edge
			if err := resolveRebuildMember(ctx, f, &e, strings.TrimPrefix(target, "*.")); err != nil {
				return nil, err
			}
			updates[i] = &e
		case edge.Kind == graph.EdgeProvides || edge.Kind == graph.EdgeConsumes:
			if target == "" {
				continue
			}
			source, err := f.view.Node(ctx, edge.From)
			if err != nil {
				return nil, err
			}
			req := store_pebble.LinkRequest{Kind: store_pebble.LinkName, Key: target}
			if source != nil {
				req.RepoPrefix = source.RepoPrefix
				_, req.Languages = resolverCompatibleLanguages(source.Language)
			}
			requests = append(requests, req)
			tokens = append(tokens, tokenSite{index: i})
		case edge.Kind == graph.EdgeRendersChild ||
			(edge.Kind == graph.EdgeReferences && strings.HasSuffix(edge.FilePath, ".vue") && edge.Meta["template"] == true):
			e := *edge
			if err := resolveRebuildVue(ctx, f, &e, target); err != nil {
				return nil, err
			}
			updates[i] = &e
		default:
			rustPath, _ := edge.Meta["rust_path"].(string)
			if strings.Contains(rustPath, "::") {
				continue // native leaves this call for the Rust scope pass
			}
			e := *edge
			if err := resolveRebuildFunction(ctx, f, &e, target); err != nil {
				return nil, err
			}
			updates[i] = &e
		}
	}
	if len(requests) > 0 {
		if _, err := f.candidates(ctx, requests, func(i int, n *graph.Node) error {
			site := &tokens[i]
			if site.sameDir {
				return nil
			}
			edge := page[site.index]
			one := [1]*graph.Node{n}
			if len(genericInstantiationOnly(edge, withoutAttributedTerminals(one[:]))) == 0 {
				return nil
			}
			if filePathDir(n.FilePath) == filePathDir(edge.FilePath) {
				site.to, site.sameDir = n.ID, true
			} else if site.to == "" {
				site.to = n.ID
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	for _, site := range tokens {
		if site.to == "" {
			continue
		}
		e := *page[site.index]
		e.To, e.Confidence = site.to, 0.7
		if site.sameDir {
			e.Confidence = 0.9
		}
		updates[site.index] = &e
	}
	var moves []graph.EdgeReindex
	for i, e := range updates {
		old := page[i]
		if e == nil || e.To == old.To {
			continue
		}
		if !graph.IsUnresolvedTarget(e.To) {
			e.Meta = maps.Clone(e.Meta)
			clearEdgeTerminal(e)
		}
		moves = append(moves, graph.EdgeReindex{Edge: e, OldTo: old.To, OldKind: old.Kind})
	}
	return moves, nil
}

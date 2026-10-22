package resolver

import (
	"context"
	"iter"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// NpmPackageLookup maps a bare npm specifier to tracked package directories.
// Each directory has graph-path entry candidates from exports, or legacy
// types/main/directory-index entry points only when exports is absent. The indexer
// owns manifest reads; the resolver probes indexed files and applies repository/workspace policy.
type NpmPackageLookup func(specifier string) map[string][]string

// SetNpmPackageLookup installs tracked-package evidence for the cross-repo pass.
// Call before resolution; nil keeps the external-package short-circuit unchanged.
func (cr *CrossRepoResolver) SetNpmPackageLookup(fn NpmPackageLookup) {
	cr.npmPackages = fn
}

type npmProbeKey struct {
	dir, spec string
}

type npmImportMemo struct {
	probes map[npmProbeKey]*graph.Node
	nodes  map[string]*graph.Node
}

func (m *npmImportMemo) getNode(cr *CrossRepoResolver, id string) *graph.Node {
	if node, seen := m.nodes[id]; seen {
		return node
	}
	node := cr.cachedGetNode(id)
	m.nodes[id] = node
	return node
}

func (cr *CrossRepoResolver) trackedNpmImport(callerRepo, callerWS, importPath string, memo *npmImportMemo) *graph.Node {
	spec, symbol := splitImportSpecSymbol(importPath)
	packages := cr.npmPackages(spec)
	// Manifest ownership, not a successful probe, blocks linking a monorepo
	// import to a clone or fork of its own workspace package.
	for dir := range packages {
		if dir == callerRepo || strings.HasPrefix(dir, callerRepo+"/") {
			return nil
		}
	}
	var local, foreign []*graph.Node
	for dir, stems := range packages {
		key := npmProbeKey{dir: dir, spec: spec}
		node, seen := memo.probes[key]
		if !seen {
			for _, stem := range stems {
				if file := probeJSTSFile(cr.cachedGetNode, stem); file != "" {
					node = cr.cachedGetNode(file)
					break
				}
			}
			memo.probes[key] = node // Cache misses too; reset on the next pass.
		}
		if node == nil {
			continue
		}
		if candidateWorkspaceID(node) == callerWS {
			local = append(local, node)
		} else if cr.crossWorkspaceEligible(callerWS, candidateWorkspaceID(node), spec) {
			foreign = append(foreign, node)
		}
	}
	candidates := local
	if len(candidates) == 0 {
		candidates = foreign
	}
	// Package names do not identify a unique tracked checkout when more
	// than one provider is eligible in the winning workspace tier.
	if len(candidates) != 1 {
		return nil
	}
	node := candidates[0]
	if symbol != "" {
		if exported := memo.getNode(cr, node.ID+"::"+symbol); exported != nil && exported.Meta["visibility"] == "public" {
			node = exported
		}
	}
	return node
}

// The per-repo pass has already made unindexed npm imports external. Retry only
// imports with positive tracked-package evidence before building call reachability.
func (cr *CrossRepoResolver) resolveTrackedNpmImports(ctx context.Context, edges iter.Seq[*graph.Edge], stats *CrossRepoStats) error {
	if cr.npmPackages == nil {
		return ctx.Err()
	}
	// Scan metadata-free projections. Hydrate only imports whose package name
	// is tracked, so ordinary node_modules dependencies stay on the cheap path.
	var eligible []*graph.Edge
	for e := range edges {
		if err := ctx.Err(); err != nil {
			return err
		}
		spec := trackedNpmImportSpec(e)
		if spec == "" || !cr.eligibleTrackedNpmImport(e, spec) {
			continue
		}
		module, _ := splitImportSpecSymbol(spec)
		if len(cr.npmPackages(module)) == 0 {
			continue
		}
		eligible = append(eligible, e)
	}
	if len(eligible) == 0 {
		return ctx.Err()
	}
	memo := &npmImportMemo{probes: map[npmProbeKey]*graph.Node{}, nodes: map[string]*graph.Node{}}
	// Close the light-edge cursor before reading or writing full edges. Hydrate
	// only one bounded page at a time; metadata never accumulates across pages.
	for start := 0; start < len(eligible); start += resolvePendingPageRows {
		if err := ctx.Err(); err != nil {
			return err
		}
		page := eligible[start:min(start+resolvePendingPageRows, len(eligible))]
		sites := make([]graph.EdgeSite, 0, len(page))
		for _, e := range page {
			sites = append(sites, graph.EdgeSite{From: e.From, Line: e.Line, Kind: graph.EdgeImports})
		}
		full := cr.graph.GetEdgeCandidates(nil, sites)
		var batch []graph.EdgeReindex
		for _, projected := range page {
			for _, e := range full.Site(projected.From, projected.Line, graph.EdgeImports) {
				if e.To != projected.To {
					continue
				}
				oldTo := e.To
				if cr.resolveTrackedNpmImport(e, trackedNpmImportSpec(e), stats, memo) {
					batch = append(batch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
					projected.To, projected.CrossRepo = e.To, e.CrossRepo
				}
			}
		}
		if len(batch) > 0 {
			cr.graph.ReindexEdges(batch)
		}
	}
	return ctx.Err()
}

func trackedNpmImportSpec(e *graph.Edge) string {
	if e == nil || e.Kind != graph.EdgeImports {
		return ""
	}
	if strings.HasPrefix(e.To, "external::") {
		return strings.TrimPrefix(e.To, "external::")
	}
	if target := graph.UnresolvedName(e.To); strings.HasPrefix(target, "import::") {
		return strings.TrimPrefix(target, "import::")
	}
	return ""
}

func (cr *CrossRepoResolver) resolveTrackedNpmImport(e *graph.Edge, importPath string, stats *CrossRepoStats, memo *npmImportMemo) bool {
	from := memo.getNode(cr, e.From)
	callerRepo := ""
	if from != nil {
		callerRepo = from.RepoPrefix
	}
	callerWS := candidateWorkspaceID(from)
	picked := cr.trackedNpmImport(callerRepo, callerWS, importPath, memo)
	if picked == nil {
		return false
	}
	e.To = picked.ID
	e.CrossRepo = true
	stats.Resolved++
	stats.CrossRepoEdges++
	stats.ByRepo[picked.RepoPrefix]++
	return true
}

// These checks use only the light projection and must precede manifest scans
// and full-edge hydration.
func (cr *CrossRepoResolver) eligibleTrackedNpmImport(e *graph.Edge, importPath string) bool {
	callerFile := jsTSImportCallerFile(e)
	if !isJSTSBareSpecifier(callerFile, importPath) || !declaresExternalNpmDep(cr.npmDep, callerFile, importPath) {
		return false
	}
	spec, _ := splitImportSpecSymbol(importPath)
	_, aliased := rewriteNpmAliasImport(cr.npmAlias, callerFile, spec)
	return !aliased
}

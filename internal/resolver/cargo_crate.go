package resolver

import (
	"context"
	"iter"
	"path"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// CargoImportStubMeta preserves the import's exact target before a tracked bind.
const CargoImportStubMeta = "cargo_import_stub"

// CargoCallStubMeta retains the extractor's function or method target before linking.
const CargoCallStubMeta = "cargo_call_stub"

// CargoCrate identifies a provider library and its exposed crate name.
type CargoCrate struct {
	Root string // graph-path library file
	Name string // exposed provider crate name, never the consumer's alias
}

// CargoCrateLookup returns providers declared by the crate owning callerFile.
// The resolver owns workspace eligibility.
type CargoCrateLookup func(callerFile, crateName string) []CargoCrate

// SetCargoCrateLookup installs manifest evidence for Rust import/file edges.
func (cr *CrossRepoResolver) SetCargoCrateLookup(lookup CargoCrateLookup) { cr.cargoCrates = lookup }

// SetCargoImportRetargetRepo refreshes marked bindings only after a provider file is added.
func (cr *CrossRepoResolver) SetCargoImportRetargetRepo(repo string) { cr.cargoRetargetRepo = repo }

func (cr *CrossRepoResolver) cargoImportTarget(e *graph.Edge, spec string) *graph.Node {
	if cr.cargoCrates == nil || e.Kind != graph.EdgeImports || !strings.HasSuffix(e.FilePath, ".rs") {
		return nil
	}
	segs := strings.Split(spec, "/")
	switch segs[0] {
	case "", "crate", "self", "super", "std", "core", "alloc":
		return nil
	}
	for _, seg := range segs {
		if seg == ".." || seg == "." || seg == "" {
			return nil
		}
	}
	crates := cr.cargoCrates(e.FilePath, segs[0])
	if len(crates) == 0 {
		return nil
	}
	callerRepo, callerWS := cr.callerRepoPrefix(e), cr.callerWorkspaceID(e)
	var root *graph.Node
	for _, crate := range crates {
		candidate := cr.cachedGetNode(crate.Root)
		if candidate == nil || candidate.Kind != graph.KindFile || candidate.Language != "rust" {
			continue
		}
		if candidate.RepoPrefix == callerRepo {
			return nil // same-repo resolution stays unchanged
		}
		if !cr.crossWorkspaceEligible(callerWS, candidateWorkspaceID(candidate), crate.Name+strings.TrimPrefix(spec, segs[0])) {
			continue
		}
		if root != nil {
			return nil // two eligible providers, even if only one has the module
		}
		root = candidate
	}
	if root == nil {
		return nil
	}
	// Same longest-prefix module-file probes as resolveRustUseFile, anchored
	// at the manifest's library root rather than a guessed src directory.
	for n := len(segs); n > 1; n-- {
		stem := path.Join(append([]string{path.Dir(root.ID)}, segs[1:n]...)...)
		var found *graph.Node
		for _, id := range []string{stem + ".rs", stem + "/mod.rs"} {
			if node := cr.cachedGetNode(id); node != nil && node.Kind == graph.KindFile && node.Language == "rust" {
				if found != nil {
					return nil
				}
				found = node
			}
		}
		if found != nil {
			return found
		}
	}
	return root // item or glob import from the crate itself
}

// Per-repo resolution converts missing imports to external stubs. Retry just
// those Rust imports before the cross-repo unresolved pass builds reachability.
func (cr *CrossRepoResolver) resolveCargoImports(ctx context.Context, edges iter.Seq[*graph.Edge], stats *CrossRepoStats) error {
	if cr.cargoCrates == nil {
		return ctx.Err()
	}
	// Keep metadata out of the scan. Only declared, tracked crates need hydration.
	var eligible []*graph.Edge
	for e := range edges {
		if err := ctx.Err(); err != nil {
			return err
		}
		spec := cr.cargoImportSpec(e)
		if spec == "" {
			continue
		}
		name, _, _ := strings.Cut(spec, "/")
		if len(cr.cargoCrates(e.FilePath, name)) > 0 {
			eligible = append(eligible, e)
		}
	}
	// Close the light cursor before hydration or writes; retain metadata for
	// only one bounded page, as in the tracked npm bridge.
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
				if node := cr.cargoImportTarget(e, cr.cargoImportSpec(e)); node != nil && node.ID != e.To {
					oldTo := e.To
					cr.bindCargoImport(e, node, stats)
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

func (cr *CrossRepoResolver) cargoImportSpec(e *graph.Edge) string {
	if e == nil || e.Kind != graph.EdgeImports || !strings.HasSuffix(e.FilePath, ".rs") {
		return ""
	}
	target := e.To
	if cr.cargoRetargetRepo != "" && e.CrossRepo && graph.RepoPrefixOfID(e.To) == cr.cargoRetargetRepo {
		target, _ = e.Meta[CargoImportStubMeta].(string)
		target = strings.Replace(target, "unresolved::import::", "external::", 1)
	}
	spec, external := strings.CutPrefix(target, "external::")
	if !external {
		return ""
	}
	name, _, _ := strings.Cut(spec, "/")
	switch name {
	case "", "crate", "self", "super", "std", "core", "alloc":
		return ""
	}
	return spec
}

func (cr *CrossRepoResolver) bindCargoImport(e *graph.Edge, node *graph.Node, stats *CrossRepoStats) {
	if e.Meta == nil {
		e.Meta = make(map[string]any)
	}
	if original, _ := e.Meta[CargoImportStubMeta].(string); original == "" {
		e.Meta[CargoImportStubMeta] = e.To
	}
	e.To, e.CrossRepo = node.ID, true
	clearEdgeTerminal(e)
	stats.Resolved++
	stats.CrossRepoEdges++
	stats.ByRepo[node.RepoPrefix]++
}

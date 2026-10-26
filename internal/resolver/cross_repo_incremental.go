package resolver

import (
	"context"
	"path"
	"strings"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// ResolveFilesAndIncoming runs one scoped cross-repository pass for a complete
// mutation batch. The frontier contains unresolved edges emitted by the changed
// files and unresolved incoming stub buckets for the symbols they define. Both
// directions share one resolver lock and one build of the directory,
// dependency, reachability, and lookup indexes.
func (cr *CrossRepoResolver) ResolveFilesAndIncoming(filePaths []string) *CrossRepoStats {
	return cr.ResolveMutationFiles(filePaths, filePaths)
}

// ResolveMutationFiles separates files that can create or bind unresolved
// edges from files whose resolved base edges only need cross-repository
// materialization. Keeping both roles under one lock still shares the expensive
// workspace indexes without feeding every semantic edge back through resolution.
func (cr *CrossRepoResolver) ResolveMutationFiles(resolutionFiles, materializationFiles []string) *CrossRepoStats {
	return cr.ResolveMutationFrontiers(resolutionFiles, materializationFiles, materializationFiles)
}

// CrossRepoIncomingAdmission is one cross-repository mutation pass's bounded
// incoming-stub and Go-import admission: the completeness fact plus the typed refusal that
// produced it (*graph.BoundedLocalizationLimitError, nil when nothing was
// refused).
//
// It is returned beside CrossRepoStats rather than folded into it because a
// refused incoming leg is not a count of resolutions — it is the statement that
// this pass never looked at Dropped parked references, and a consumer that
// cannot read it cannot tell a cross-repo pass that had no incoming work from
// one that refused to admit the incoming work it had.
type CrossRepoIncomingAdmission struct {
	graph.IncomingSourceAdmission
	Refusal         error
	IncomingRefused bool // Separates incoming proof from independent Go import recovery.
}

// ResolveMutationFrontiers preserves the distinct edge-source and changed-
// definition roles through candidate selection. It discards the incoming
// admission fact after logging it; callers that must react to a bounded pass
// use ResolveMutationFrontiersBounded.
func (cr *CrossRepoResolver) ResolveMutationFrontiers(resolutionFiles, edgeSourceFiles, definitionFiles []string) *CrossRepoStats {
	stats, _ := cr.ResolveMutationFrontiersBounded(resolutionFiles, edgeSourceFiles, definitionFiles)
	return stats
}

// ResolveMutationFrontiersBounded is ResolveMutationFrontiers plus the incoming
// leg's completeness fact. The frontier's incoming admission is charged against
// the same shared ceiling the single-repository legs use (the constant, not one
// budget object: every leg passes a nil budget and gets a fresh one), and a
// refusal empties
// the incoming half of the frontier: the pass then resolves only the changed
// files' own outgoing edges, and its CrossRepoStats — which count exactly that
// — would otherwise read as a complete pass.
func (cr *CrossRepoResolver) ResolveMutationFrontiersBounded(resolutionFiles, edgeSourceFiles, definitionFiles []string) (*CrossRepoStats, CrossRepoIncomingAdmission) {
	stats := &CrossRepoStats{ByRepo: make(map[string]int)}
	var admission CrossRepoIncomingAdmission
	if cr == nil || cr.graph == nil || len(resolutionFiles) == 0 && len(edgeSourceFiles) == 0 && len(definitionFiles) == 0 {
		return stats, admission
	}

	providerRepos := make(map[string]bool)
	goMutation := false
	goManifestMutation := false
	for _, files := range [][]string{resolutionFiles, edgeSourceFiles} {
		for _, file := range files {
			if goModuleManifestPath(file) {
				goManifestMutation = true
				goMutation = true
			}
			if strings.HasSuffix(file, ".go") {
				goMutation = true
			}
		}
	}
	for _, file := range definitionFiles {
		if goModuleManifestPath(file) {
			goManifestMutation = true
			goMutation = true
		}
		repo := graph.RepoPrefixOfID(file)
		if strings.HasSuffix(file, ".go") && !strings.HasSuffix(file, "_test.go") && cr.goModules[repo] != "" {
			providerRepos[repo] = true
			goMutation = true
		}
	}
	importBudget := newGoModuleImportRecoveryBudget()
	// Provider and build-list recovery need the seed before the pass lock.
	// Ordinary consumer frontiers keep local resolution ahead of the bridge.
	if len(providerRepos) > 0 || goManifestMutation {
		if err := cr.seedGoModuleImportRegistry(context.Background()); err != nil {
			cr.logger.Warn("cross-repo resolve: seed Go module imports deferred", zap.Error(err))
		}
	}
	cr.mu.Lock()
	defer cr.mu.Unlock()
	cr.goModuleImportsDeferred = 0

	budget := &graph.IncomingSourceBudget{}
	if len(resolutionFiles) > 0 {
		frontier := collectIncrementalFileFrontier(cr.graph, resolutionFiles, nil)
		admission.Keys += frontier.incomingAdmission.Keys
		admission.Inspected += frontier.incomingAdmission.Inspected
		admission.Dropped += frontier.incomingAdmission.Dropped
		admission.Limit = frontier.incomingAdmission.Limit
		if frontier.incomingAdmission.Refused {
			admission.Refused, admission.Refusal = true, frontier.incomingRefusal
		}
		logIncomingAdmissionRefusal(cr.logger, "cross_repo_preparation", len(frontier.stubKeys),
			frontier.incomingAdmission, frontier.incomingRefusal)
		// Per-repo resolution has already made registry imports external.
		// Keep outgoing imports for the tracked-package retry as well.
		if cr.npmPackages != nil {
			for _, edges := range frontier.outByNode {
				for _, edge := range edges {
					if trackedNpmImportSpec(edge) != "" {
						frontier.pending = append(frontier.pending, edge)
					}
				}
			}
		}
		// The per-repo pass has made missing Rust imports external already.
		// Keep outgoing imports for the tracked-crate retry too.
		if cr.cargoCrates != nil {
			for _, edges := range frontier.outByNode {
				for _, edge := range edges {
					if cr.cargoImportSpec(edge) != "" {
						frontier.pending = append(frontier.pending, edge)
					}
				}
			}
		}
		if cr.pythonPackages != nil {
			for _, edges := range frontier.outByNode {
				for _, edge := range edges {
					if trackedPythonImportSpec(edge) != "" {
						frontier.pending = append(frontier.pending, edge)
					}
				}
			}
		}
		pending := make([]*graph.Edge, 0, len(frontier.pending))
		seen := make(map[graph.EdgeIdentity]struct{}, len(frontier.pending))
		for _, edge := range frontier.pending {
			if edge == nil || !graph.IsUnresolvedTarget(edge.To) && trackedNpmImportSpec(edge) == "" && cr.cargoImportSpec(edge) == "" && trackedPythonImportSpec(edge) == "" {
				continue
			}
			identity := graph.EdgeIdentityFor(edge)
			if _, duplicate := seen[identity]; duplicate {
				continue
			}
			seen[identity] = struct{}{}
			pending = append(pending, edge)
		}
		if len(pending) > 0 {
			resolved := cr.resolveScopedLocked(pending)
			stats.Resolved += resolved.Resolved
			stats.Unresolved += resolved.Unresolved
			stats.CrossRepoEdges += resolved.CrossRepoEdges
			for repo, count := range resolved.ByRepo {
				stats.ByRepo[repo] += count
			}
		}
		// Preserve ordinary local-file resolution, but count each import only
		// once if the module bridge subsequently lifts its dependency fallback.
		counted := make(map[graph.EdgeIdentity]bool)
		for _, e := range pending {
			if e.Kind == graph.EdgeImports {
				counted[graph.EdgeIdentityFor(e)] = !graph.IsUnresolvedTarget(e.To) && !strings.HasPrefix(e.To, "external::")
			}
		}
		_ = cr.resolveGoModuleImports(context.Background(), resolutionFiles, stats, counted)
	}
	if len(definitionFiles) > 0 && len(cr.goModules) > 0 && !admission.Refused {
		if frontierInspected := admission.Inspected; frontierInspected > 0 {
			if err := budget.Charge(frontierInspected); err != nil {
				admission.Refused, admission.Refusal = true, err
			}
		}
		if !admission.Refused {
			nodes := cr.graph.GetFileNodesByPaths(definitionFiles)
			edges, fact, err := cr.goModuleIncomingEdgesBounded(nodes, definitionFiles, budget)
			admission.Keys += fact.Keys
			admission.Inspected += fact.Inspected
			admission.Limit = fact.Limit
			admission.Dropped += fact.Dropped
			admission.Refused, admission.Refusal = fact.Refused, err
			logIncomingAdmissionRefusal(cr.logger, "go_module_incoming", fact.Keys, fact, err)
			cr.resolveGoModuleEdgesLocked(edges, stats)
		}
	}
	// A newly tracked module also makes old dep/stdlib terminals eligible.
	// This indexed sweep runs at most once per module/config revision.
	if goMutation && !admission.Refused {
		if err := cr.retryGoModuleDeps(context.Background(), stats, false); err != nil {
			cr.logger.Error("cross-repo resolve: retry Go module terminals", zap.Error(err))
		}
	}
	// Import recovery has its own ceiling: a popular provider must not starve
	// incoming calls or the revision-keyed call-terminal retry above.
	for repo := range providerRepos {
		ids := cr.goModuleImportIDs(repo)
		if len(ids) > importBudget.Remaining() {
			cr.goModuleImportsDeferred += len(ids)
			cr.logger.Warn("cross-repo resolve: Go module imports deferred to full pass", zap.Int("imports", len(ids)), zap.Int("remaining", importBudget.Remaining()))
			continue
		}
		if err := importBudget.Charge(len(ids)); err != nil {
			cr.goModuleImportsDeferred += len(ids)
			cr.logger.Warn("cross-repo resolve: Go module imports deferred to full pass", zap.Error(err))
			continue
		}
		for start := 0; start < len(ids); start += externalCallMutationChunk {
			if err := cr.restoreGoModuleImportBatch(context.Background(), repo, ids[start:min(start+externalCallMutationChunk, len(ids))], stats); err != nil {
				cr.logger.Error("cross-repo resolve: restore Go module imports", zap.Error(err))
			}
		}
	}

	admission.IncomingRefused = admission.Refused
	if deferred := cr.goModuleImportsDeferred; deferred > 0 {
		admission.Refused = true
		admission.Dropped += deferred
		admission.Limit = graph.MaxIncomingSourceCandidateRows
		if admission.Refusal == nil {
			admission.Refusal = &graph.BoundedLocalizationLimitError{Resource: "Go module import recovery", Limit: graph.MaxIncomingSourceCandidateRows}
		}
		if state := cr.goModuleRetry; state != nil {
			state.mu.Lock()
			state.complete = false
			state.mu.Unlock()
		}
	}
	DetectCrossRepoEdgesForMutationFiles(cr.graph, edgeSourceFiles, definitionFiles)
	return stats, admission
}

// DetectCrossRepoEdgesForFiles materializes the cross-repo layer only for base
// edges incident to nodes in the exact changed-file frontier. Inspecting both
// incoming and outgoing edges covers unchanged callers rebound to a changed
// target as well as new calls emitted by the changed source file.
func DetectCrossRepoEdgesForFiles(g graph.Store, filePaths []string) int {
	return DetectCrossRepoEdgesForMutationFiles(g, filePaths, filePaths)
}

// DetectCrossRepoEdgesForMutationFiles materializes resolved base edges using
// the narrow query shape for each mutation role.
func DetectCrossRepoEdgesForMutationFiles(g graph.Store, edgeSourceFiles, definitionFiles []string) int {
	if g == nil || len(edgeSourceFiles) == 0 && len(definitionFiles) == 0 {
		return 0
	}
	return materializeCrossRepoCandidates(g, crossRepoCandidatesForMutationFiles(g, edgeSourceFiles, definitionFiles))
}

// Keep the Go build-list inputs aligned with the indexer's generation input
// boundary. Source-only suffix checks miss workspace and vendor changes.
func goModuleManifestPath(file string) bool {
	file = strings.ReplaceAll(file, "\\", "/")
	switch path.Base(file) {
	case "go.mod", "go.sum", "go.work", "go.work.sum":
		return true
	}
	return file == "vendor/modules.txt" || strings.HasSuffix(file, "/vendor/modules.txt")
}

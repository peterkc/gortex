package indexer

import "github.com/zzet/gortex/internal/graph"

// These test spies wrap graph.New. Preserve its backfill capability without
// exposing unrelated optional methods or bypassing the spies' write hooks.
func (s *armableBlockingAddBatchStore) BackfillWorkspaceSlugsWithImpact(slugs []graph.WorkspaceSlug) graph.WorkspaceSlugBackfillResult {
	return s.Store.(graph.WorkspaceSlugImpactBackfiller).BackfillWorkspaceSlugsWithImpact(slugs)
}

func (s *countingIndexAllStore) BackfillWorkspaceSlugsWithImpact(slugs []graph.WorkspaceSlug) graph.WorkspaceSlugBackfillResult {
	return s.Store.(graph.WorkspaceSlugImpactBackfiller).BackfillWorkspaceSlugsWithImpact(slugs)
}

func (s *recordingRepoIndexStateStore) BackfillWorkspaceSlugsWithImpact(slugs []graph.WorkspaceSlug) graph.WorkspaceSlugBackfillResult {
	return s.Store.(graph.WorkspaceSlugImpactBackfiller).BackfillWorkspaceSlugsWithImpact(slugs)
}

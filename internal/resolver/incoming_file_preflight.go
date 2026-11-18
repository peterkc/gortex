package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph"
)

// PreflightIncomingFiles checks the warmup fallback predicate without hydrating
// outgoing nodes or edges. It has no mutation evidence: warmup checks the current
// declarations only. Ordinary incremental resolves keep their existing reads.
func PreflightIncomingFiles(ctx context.Context, g graph.Store, filePaths []string) (graph.IncomingSourceAdmission, error) {
	paths := incrementalFilePaths(filePaths)
	if g == nil || len(paths) == 0 {
		return graph.IncomingSourceAdmission{}, nil
	}
	g.ResolveMutex().Lock()
	defer g.ResolveMutex().Unlock()
	keys := incomingStubKeys(graph.NodesLightInScopeSeq(g, nil, paths), nil)
	_, fact, err := admitIncomingIdentities(ctx, g, keys)
	return fact, err
}

func incrementalFilePaths(filePaths []string) []string {
	var paths []string
	seen := make(map[string]struct{}, len(filePaths))
	for _, path := range filePaths {
		if path == "" {
			continue
		}
		if _, duplicate := seen[path]; duplicate {
			continue
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	return paths
}

func incomingStubKeys(nodes iter.Seq[*graph.Node], extra []string) []string {
	var keys []string
	seen := make(map[string]struct{})
	appendKey := func(key string) {
		if key == "" {
			return
		}
		if _, duplicate := seen[key]; duplicate {
			return
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	for node := range nodes {
		if node == nil || node.Name == "" || !graph.IsReferenceableSymbol(node.Kind) {
			continue
		}
		// The four name-owned forms are one contract. This builder is
		// the batched incremental path's incoming leg, and it was the
		// last one still hand-building the bare pair: a member
		// reference parked under a wildcard form stayed pending until
		// the next whole-graph resolve.
		for _, key := range graph.UnresolvedNameCandidateIDs(node) {
			appendKey(key)
		}
	}
	for _, key := range extra {
		appendKey(key)
	}
	return keys
}

func admitIncomingIdentities(ctx context.Context, g graph.Store, keys []string) (map[string][]graph.EdgeIdentity, graph.IncomingSourceAdmission, error) {
	return graph.AdmitIncomingRowsBounded(ctx,
		func(keys []string) map[string][]graph.EdgeIdentity {
			return graph.InEdgeIdentitiesByNodeIDs(g, keys)
		}, keys, nil)
}

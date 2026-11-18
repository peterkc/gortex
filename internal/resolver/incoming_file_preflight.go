package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph"
)

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

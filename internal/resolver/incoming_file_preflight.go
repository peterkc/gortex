package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph"
	"go.uber.org/zap"
)

// PreflightIncomingFiles checks the warmup fallback predicate without hydrating
// outgoing adjacency. File-index reads stay bounded to one chunk at a time.
// It has no mutation evidence: warmup checks current declarations only.
// Ordinary incremental resolves keep their existing reads.
func PreflightIncomingFiles(ctx context.Context, g graph.Store, filePaths []string, logger *zap.Logger) (graph.IncomingSourceAdmission, error) {
	paths := incrementalFilePaths(filePaths)
	if g == nil || len(paths) == 0 {
		return graph.IncomingSourceAdmission{}, nil
	}
	g.ResolveMutex().Lock()
	defer g.ResolveMutex().Unlock()
	// Reuse the file-index reader rather than the files-only scoped summary,
	// which SQLite must serve by scanning the generation once per page.
	const fileChunkSize = 256
	nodes := func(yield func(*graph.Node) bool) {
		for start := 0; start < len(paths); start += fileChunkSize {
			if ctx.Err() != nil {
				return
			}
			chunk := paths[start:min(start+fileChunkSize, len(paths))]
			for node := range frameworkFileFrontierNodes(g, nil, chunk,
				graph.KindFunction, graph.KindMethod, graph.KindType, graph.KindInterface,
				graph.KindVariable, graph.KindConstant, graph.KindField, graph.KindEnumMember) {
				if !yield(node) {
					return
				}
			}
		}
	}
	keys := incomingStubKeys(nodes, nil)
	_, fact, err := admitIncomingIdentities(ctx, g, keys)
	if fact.Refused {
		logIncomingAdmissionRefusal(logger, "preparation", len(keys), fact, err)
	}
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

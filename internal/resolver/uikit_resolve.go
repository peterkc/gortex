package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// UIKit directory-convention name-resolution. Like the SwiftUI pass, this is a
// fallback for the references sourcekit-lsp leaves unresolved: a `*ViewController`
// binds to /ViewControllers/ or /Controllers/, a `*Cell` to /Cells/,
// /TableViewCells/ or /CollectionViewCells/, and a `*Delegate` / `*DataSource`
// to a /Delegates/ or /DataSources/ directory (or, via the same-dir tier, the
// owning controller's own directory). Swift and Objective-C references are
// considered.

var (
	uikitVCDirs       = []string{"/ViewControllers/", "/Controllers/", "/ViewController/", "/Controller/"}
	uikitCellDirs     = []string{"/Cells/", "/TableViewCells/", "/CollectionViewCells/", "/Cell/"}
	uikitDelegateDirs = []string{"/Delegates/", "/DataSources/", "/Delegate/", "/DataSource/"}
)

// ResolveUIKitRefs binds residual unresolved UIKit references to their
// directory-located definitions. Returns the count bound.
func ResolveUIKitRefs(g graph.Store) int {
	return resolveUIKitRefs(g, nil)
}

// resolveUIKitRefs is the census-multiplexed form. Calls and references use
// candidates exact-refetched immediately before this pass begins; instantiates
// and typed-as edges retain their local scans. Candidate assembly deliberately
// preserves the legacy kind order: Instantiates, References, TypedAs, Calls.
func resolveUIKitRefs(g graph.Store, cands *frameworkPassCandidates) int {
	if g == nil {
		return 0
	}

	type candidate struct {
		identity graph.EdgeIdentity
		edge     *graph.Edge
	}
	var candidates []candidate
	var localIDs []graph.EdgeIdentity
	var sourceIDs []string
	appendLocal := func(kind graph.EdgeKind) {
		for edge := range graph.EdgesLightSeq(g, kind) {
			if !isUIKitCandidateEdge(edge) {
				continue
			}
			identity := graph.EdgeIdentityFor(edge)
			candidates = append(candidates, candidate{identity: identity})
			localIDs = append(localIDs, identity)
			sourceIDs = append(sourceIDs, edge.From)
		}
	}
	appendShared := func(edges []*graph.Edge) {
		for _, edge := range edges {
			if !isUIKitCandidateEdge(edge) {
				continue
			}
			identity := graph.EdgeIdentityFor(edge)
			candidates = append(candidates, candidate{identity: identity, edge: edge})
			sourceIDs = append(sourceIDs, edge.From)
		}
	}

	appendLocal(graph.EdgeInstantiates)
	if cands == nil {
		appendLocal(graph.EdgeReferences)
	} else {
		appendShared(cands.refs)
	}
	appendLocal(graph.EdgeTypedAs)
	if cands == nil {
		appendLocal(graph.EdgeCalls)
	} else {
		appendShared(cands.calls)
	}
	if len(candidates) == 0 {
		return 0
	}

	// Shared candidates were exact-refetched by passStreams. Local candidates
	// receive the same exact current-row check here after their cursors close.
	placements := graph.NodePlacementsByIDs(g, dedupeFrameworkIDs(sourceIDs))
	currentLocal := findFrameworkEdgesByIdentities(g, localIDs)
	appleCandidates := candidates[:0]
	for _, cand := range candidates {
		edge := cand.edge
		if edge == nil {
			edge = currentLocal[cand.identity]
		}
		if !isUIKitCandidateEdge(edge) || graph.EdgeIdentityFor(edge) != cand.identity {
			continue
		}
		placement, ok := placements[edge.From]
		if !ok || !isAppleSourceFile(placement.FilePath) {
			continue
		}
		cand.edge = edge
		appleCandidates = append(appleCandidates, cand)
	}
	if len(appleCandidates) == 0 {
		return 0
	}

	resolved := 0
	var reindex []graph.EdgeReindex
	for _, cand := range appleCandidates {
		edge := cand.edge
		// Re-apply the full pass predicate to the authoritative edge. The shared
		// census predicate is intentionally only a cheap necessary condition.
		if !isUIKitCandidateEdge(edge) {
			continue
		}
		name := graph.UnresolvedName(edge.To)
		dirs, _ := uikitDirsFor(name)
		fromFile := placements[edge.From].FilePath
		targetID, conf := ResolveByConvention(g, name, "", dirs, fromFile)
		if targetID == "" {
			continue
		}
		reindex = append(reindex, uikitRefRebind(edge, targetID, conf))
		resolved++
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	return resolved
}

func uikitRefRebind(edge *graph.Edge, targetID string, conf float64) graph.EdgeReindex {
	oldTo := edge.To
	edge.To = targetID
	edge.Origin = graph.OriginASTInferred
	edge.Confidence = conf
	edge.ConfidenceLabel = graph.ConfidenceLabelFor(edge.Kind, conf)
	StampSynthesized(edge, SynthUIKitResolve)
	return graph.EdgeReindex{Edge: edge, OldTo: oldTo}
}

func isUIKitCandidateEdge(edge *graph.Edge) bool {
	return edge != nil && isUIKitCandidateTarget(edge.To)
}

func isUIKitCandidateTarget(target string) bool {
	if !graph.IsUnresolvedTarget(target) {
		return false
	}
	name := graph.UnresolvedName(target)
	if name == "" || strings.ContainsRune(name, '.') {
		return false
	}
	_, ok := uikitDirsFor(name)
	return ok
}

// uikitDirsFor classifies a UIKit reference name into its convention dirs.
func uikitDirsFor(name string) ([]string, bool) {
	switch {
	case strings.HasSuffix(name, "ViewController"):
		return uikitVCDirs, true
	case strings.HasSuffix(name, "Cell"):
		return uikitCellDirs, true
	case strings.HasSuffix(name, "Delegate"), strings.HasSuffix(name, "DataSource"):
		return uikitDelegateDirs, true
	}
	return nil, false
}

// isAppleSourceFile reports whether a path is a Swift or Objective-C source
// file — the only files the UIKit pass binds references from.
func isAppleSourceFile(p string) bool {
	switch {
	case strings.HasSuffix(p, ".swift"), strings.HasSuffix(p, ".m"),
		strings.HasSuffix(p, ".mm"), strings.HasSuffix(p, ".h"):
		return true
	}
	return false
}

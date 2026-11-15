package mcp

import (
	"context"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/rerank"
)

const exploreQualifiedLeafEndpointLimit = exploreSyntacticAnchorFetch * exploreExactNameAnchorOwnerScan

func exploreQualifiedAnchorParts(qualifiedName string) (owner, member string, ok bool) {
	dot := strings.LastIndexByte(qualifiedName, '.')
	if dot <= 0 || dot == len(qualifiedName)-1 {
		return "", "", false
	}
	owner = exploreQualifiedIdentifierLeaf(qualifiedName[:dot])
	member = exploreQualifiedIdentifierLeaf(qualifiedName[dot+1:])
	return owner, member, owner != "" && member != ""
}

func exploreQualifiedLeafMatchesNode(node *graph.Node, leaf string) bool {
	if node == nil || leaf == "" {
		return false
	}
	for _, identifier := range []string{node.Name, node.QualName, node.ID} {
		if strings.EqualFold(exploreQualifiedIdentifierLeaf(identifier), leaf) {
			return true
		}
	}
	return false
}

type exploreQualifiedOwnerFile struct {
	path     string
	ownerIDs map[string]struct{}
}

// exploreRankedQualifiedOwnerFiles intersects exact owner declarations with
// the ordinary result order. Thus a common owner name cannot authorize a new
// file: independent retrieval must already have ranked that file.
func (s *Server) exploreRankedQualifiedOwnerFiles(
	ctx context.Context,
	reader graph.Reader,
	owner string,
	ordinary []*rerank.Candidate,
	scope query.QueryOptions,
) []exploreQualifiedOwnerFile {
	page, ok := boundedLocalizationExactName(
		ctx,
		reader,
		owner,
		s.localizationNodeScope(ctx, scope, graph.KindType, graph.KindInterface),
		exploreExactNameAnchorOwnerRawScan,
	)
	if !ok {
		return nil
	}
	ownersByFile := make(map[string]map[string]struct{})
	rawScanned, eligibleScanned := 0, 0
	for _, node := range page.Nodes {
		if rawScanned == exploreExactNameAnchorOwnerRawScan {
			break
		}
		rawScanned++
		if node == nil || node.Name != owner || node.FilePath == "" ||
			(node.Kind != graph.KindType && node.Kind != graph.KindInterface) ||
			!scope.ScopeAllows(node) || !s.nodeInSessionScope(ctx, node) {
			continue
		}
		if eligibleScanned == exploreExactNameAnchorOwnerScan {
			break
		}
		eligibleScanned++
		ownerIDs := ownersByFile[node.FilePath]
		if ownerIDs == nil {
			ownerIDs = make(map[string]struct{})
			ownersByFile[node.FilePath] = ownerIDs
		}
		ownerIDs[node.ID] = struct{}{}
	}
	if len(ownersByFile) == 0 {
		return nil
	}
	files := make([]exploreQualifiedOwnerFile, 0, exploreQualifiedLeafMaxFiles)
	seen := make(map[string]struct{}, exploreQualifiedLeafMaxFiles)
	for _, candidate := range ordinary {
		if candidate == nil || candidate.Node == nil {
			continue
		}
		path := candidate.Node.FilePath
		ownerIDs, ownerFile := ownersByFile[path]
		if !ownerFile {
			continue
		}
		if _, duplicate := seen[path]; duplicate {
			continue
		}
		seen[path] = struct{}{}
		files = append(files, exploreQualifiedOwnerFile{path: path, ownerIDs: ownerIDs})
		if len(files) == exploreQualifiedLeafMaxFiles {
			break
		}
	}
	return files
}

type exploreQualifiedLeafOwnerStatus uint8

const (
	exploreQualifiedLeafOwnerUnknown exploreQualifiedLeafOwnerStatus = iota
	exploreQualifiedLeafOwnerMatch
	exploreQualifiedLeafOwnerMismatch
)

type exploreQualifiedLeafMatch struct {
	node     *graph.Node
	ownerIDs map[string]struct{}
	proven   bool
}

func (s *Server) exploreQualifiedLeafCandidate(
	ctx context.Context,
	reader graph.Reader,
	anchor exploreSyntacticAnchor,
	ordinary []*rerank.Candidate,
	scope query.QueryOptions,
	usedIDs, usedFiles map[string]struct{},
) *rerank.Candidate {
	owner, member, ok := exploreQualifiedAnchorParts(anchor.qualifiedName)
	if !ok || ctx.Err() != nil {
		return nil
	}
	files := s.exploreRankedQualifiedOwnerFiles(ctx, reader, owner, ordinary, scope)
	if len(files) == 0 {
		return nil
	}
	nodeScope := s.localizationNodeScope(
		ctx, scope, graph.KindFunction, graph.KindMethod, graph.KindType, graph.KindMacro,
	)
	budget := localizationFileBudgetFor(ctx)
	matches := make([]exploreQualifiedLeafMatch, 0, exploreSyntacticAnchorFetch)
	seen := make(map[string]struct{}, exploreSyntacticAnchorFetch)
	for _, file := range files {
		if ctx.Err() != nil {
			return nil
		}
		if len(matches) == exploreSyntacticAnchorFetch {
			break
		}
		page, complete := boundedLocalizationFileNodes(
			ctx, reader, budget, file.path, nodeScope, localizationFileNodeLimit,
		)
		if !complete {
			return nil
		}
		for _, node := range page.Nodes {
			if node == nil || !exploreQualifiedLeafMatchesNode(node, member) ||
				!exploreSyntacticAnchorEligibleNode(node) || !scope.ScopeAllows(node) ||
				!s.nodeInSessionScope(ctx, node) {
				continue
			}
			if _, duplicate := seen[node.ID]; duplicate {
				continue
			}
			seen[node.ID] = struct{}{}
			ownerStatus := exploreQualifiedLeafIdentityOwnerStatus(node, owner, member)
			if ownerStatus == exploreQualifiedLeafOwnerMismatch {
				continue
			}
			matches = append(matches, exploreQualifiedLeafMatch{
				node: node, ownerIDs: file.ownerIDs,
				proven: ownerStatus == exploreQualifiedLeafOwnerMatch,
			})
			if len(matches) == exploreSyntacticAnchorFetch {
				break
			}
		}
	}
	if len(matches) == 0 {
		return nil
	}

	endpoints := make([]graph.TypedEdgeEndpoint, 0, len(matches)*exploreExactNameAnchorOwnerScan)
	for _, match := range matches {
		if match.proven {
			continue
		}
		for ownerID := range match.ownerIDs {
			endpoints = append(endpoints, graph.TypedEdgeEndpoint{
				From: match.node.ID, To: ownerID, Kind: graph.EdgeMemberOf,
			})
			if len(endpoints) > exploreQualifiedLeafEndpointLimit {
				return nil
			}
		}
	}
	if len(endpoints) > 0 {
		// Ownership promotion is optional. Unavailable proof means unknown,
		// not an absent owner; do not widen to legacy adjacency.
		bounded, ok := reader.(graph.BoundedEdgeExistenceReader)
		if !ok {
			return nil
		}
		existing, err := bounded.FindExistingEdgeEndpoints(
			ctx, endpoints, exploreQualifiedLeafEndpointLimit,
		)
		if err != nil || ctx.Err() != nil {
			return nil
		}
		for index := range matches {
			if matches[index].proven {
				continue
			}
			for ownerID := range matches[index].ownerIDs {
				key := graph.TypedEdgeEndpoint{
					From: matches[index].node.ID, To: ownerID, Kind: graph.EdgeMemberOf,
				}
				if _, exactOwner := existing[key]; exactOwner {
					matches[index].proven = true
					break
				}
			}
		}
	}

	var provenFallback *graph.Node
	for _, match := range matches {
		if !match.proven {
			continue
		}
		if _, used := usedIDs[match.node.ID]; used {
			continue
		}
		if provenFallback == nil {
			provenFallback = match.node
		}
		if _, repeatedFile := usedFiles[match.node.FilePath]; !repeatedFile {
			return &rerank.Candidate{Node: match.node, VectorRank: -1}
		}
	}
	if provenFallback != nil {
		return &rerank.Candidate{Node: provenFallback, VectorRank: -1}
	}
	// Parsers that do not retain enclosing-owner metadata still recover one
	// globally unique leaf. Any second leaf makes ownership ambiguous and the
	// qualified fallback deliberately declines.
	if len(matches) != 1 {
		return nil
	}
	if _, used := usedIDs[matches[0].node.ID]; used {
		return nil
	}
	return &rerank.Candidate{Node: matches[0].node, VectorRank: -1}
}

func exploreQualifiedLeafIdentityOwnerStatus(node *graph.Node, owner, member string) exploreQualifiedLeafOwnerStatus {
	if node == nil || owner == "" || member == "" {
		return exploreQualifiedLeafOwnerUnknown
	}
	status := exploreQualifiedLeafOwnerUnknown
	identities := []string{node.QualName, node.Name, node.ID}
	for index, identity := range identities {
		identity = strings.TrimSpace(identity)
		if index == len(identities)-1 {
			// Graph IDs carry a file/repo prefix before the final ::. Only the
			// symbol suffix can describe an enclosing declaration.
			if cut := strings.LastIndex(identity, "::"); cut >= 0 {
				identity = identity[cut+2:]
			}
		}
		identity = strings.ReplaceAll(identity, "::", ".")
		separator := strings.LastIndexByte(identity, '.')
		if separator <= 0 || !strings.EqualFold(exploreQualifiedIdentifierLeaf(identity[separator+1:]), member) {
			continue
		}
		ownerPart := exploreQualifiedIdentifierLeaf(identity[:separator])
		if ownerPart == "" {
			continue
		}
		if strings.EqualFold(ownerPart, owner) {
			return exploreQualifiedLeafOwnerMatch
		}
		status = exploreQualifiedLeafOwnerMismatch
	}
	return status
}

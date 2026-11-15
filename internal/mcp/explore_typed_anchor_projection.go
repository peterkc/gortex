package mcp

import (
	"context"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/rerank"
)

const (
	exploreTypedAnchorFieldLimit         = 2
	exploreTypedAnchorFieldRelationLimit = 8
	exploreTypedAnchorConsumerLimit      = 12
	exploreTypedAnchorOwnerMemberLimit   = 48
	exploreTypedAnchorCallLimit          = 64
	exploreTypedAnchorMemberOwnerLimit   = 4
	exploreTypedAnchorProjectionSignal   = "typed_anchor_projection"
)

// exploreTypedAnchorBatchReader deliberately exposes only exact batched node
// hydration. Every adjacency stage below requires the optional metadata-free
// bounded capabilities; unsupported readers fail the whole projection closed
// rather than falling back to legacy full-row edge reads. This discards only
// extra typed proof, not ordinary ranked candidates. False does not prove absence.
type exploreTypedAnchorBatchReader interface {
	GetNodesByIDs([]string) map[string]*graph.Node
}

func exploreTypedAnchorNodesByIDsBounded(
	ctx context.Context,
	reader exploreTypedAnchorBatchReader,
	ids []string,
	limit int,
) (map[string]*graph.Node, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	boundedIDs, complete := exploreBoundedNodeIDs(ids, limit)
	if reader == nil || !complete || ctx.Err() != nil {
		return nil, false
	}
	if len(boundedIDs) == 0 {
		return map[string]*graph.Node{}, true
	}
	var (
		nodes map[string]*graph.Node
		err   error
	)
	if contextual, supported := reader.(exploreContextNodesReader); supported {
		nodes, err = contextual.GetNodesByIDsContext(ctx, boundedIDs)
	} else {
		nodes = reader.GetNodesByIDs(boundedIDs)
	}
	if err != nil || ctx.Err() != nil || len(nodes) != len(boundedIDs) {
		return nil, false
	}
	for _, id := range boundedIDs {
		if nodes[id] == nil {
			return nil, false
		}
	}
	if ctx.Err() != nil {
		return nil, false
	}
	return nodes, true
}

func exploreTypedAnchorOutgoingIdentitiesBounded(
	ctx context.Context,
	reader exploreTypedAnchorBatchReader,
	ids []string,
	kinds []graph.EdgeKind,
	limit int,
) (map[string][]graph.EdgeIdentity, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	bounded, supported := reader.(graph.BoundedOutgoingEdgeIdentityReader)
	if !supported || ctx.Err() != nil {
		return nil, false
	}
	ids = exploreTypedAnchorSortedIDs(ids)
	projection, err := bounded.FindOutgoingEdgeIdentitiesBounded(ctx, ids, kinds, limit)
	if err != nil || ctx.Err() != nil {
		return nil, false
	}
	if !exploreTypedAnchorValidIdentityProjection(projection, ids, kinds, limit, true) || ctx.Err() != nil {
		return nil, false
	}
	return projection.ByEndpoint, true
}

func exploreTypedAnchorIncomingIdentitiesBounded(
	ctx context.Context,
	reader exploreTypedAnchorBatchReader,
	ids []string,
	kinds []graph.EdgeKind,
	limit int,
) (map[string][]graph.EdgeIdentity, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	bounded, supported := reader.(graph.BoundedIncomingEdgeIdentityReader)
	if !supported || ctx.Err() != nil {
		return nil, false
	}
	ids = exploreTypedAnchorSortedIDs(ids)
	projection, err := bounded.FindIncomingEdgeIdentitiesBounded(ctx, ids, kinds, limit)
	if err != nil || ctx.Err() != nil {
		return nil, false
	}
	if !exploreTypedAnchorValidIdentityProjection(projection, ids, kinds, limit, false) || ctx.Err() != nil {
		return nil, false
	}
	return projection.ByEndpoint, true
}

func exploreTypedAnchorValidIdentityProjection(
	projection graph.BoundedEdgeIdentityProjection,
	ids []string,
	kinds []graph.EdgeKind,
	limit int,
	outgoing bool,
) bool {
	if limit < 1 || len(kinds) == 0 || len(projection.ByEndpoint) > len(ids) || len(projection.Truncated) > len(ids) {
		return false
	}
	requested := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		requested[id] = struct{}{}
	}
	allowed := make(map[graph.EdgeKind]struct{}, len(kinds))
	for _, kind := range kinds {
		allowed[kind] = struct{}{}
	}
	for id, truncated := range projection.Truncated {
		if _, ok := requested[id]; !ok || truncated {
			return false
		}
	}
	for id, identities := range projection.ByEndpoint {
		if _, ok := requested[id]; !ok || len(identities) > limit || projection.Truncated[id] {
			return false
		}
		seen := make(map[graph.EdgeIdentity]struct{}, len(identities))
		for _, identity := range identities {
			if _, ok := allowed[identity.Kind]; !ok || identity.From == "" || identity.To == "" {
				return false
			}
			if outgoing && identity.From != id || !outgoing && identity.To != id {
				return false
			}
			if _, duplicate := seen[identity]; duplicate {
				return false
			}
			seen[identity] = struct{}{}
		}
	}
	return true
}

func exploreTypedAnchorSortIdentities(identities []graph.EdgeIdentity) {
	sort.SliceStable(identities, func(i, j int) bool {
		left, right := identities[i], identities[j]
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		if left.From != right.From {
			return left.From < right.From
		}
		if left.To != right.To {
			return left.To < right.To
		}
		if left.FilePath != right.FilePath {
			return left.FilePath < right.FilePath
		}
		return left.Line < right.Line
	})
}

type exploreTypedAnchorField struct {
	candidate     *rerank.Candidate
	anchorIndexes []int
	field         *graph.Node
	owner         *graph.Node
	typedIDs      map[string]struct{}
	canonicalType string
}

type exploreTypedAnchorConsumer struct {
	field         *exploreTypedAnchorField
	node          *graph.Node
	direct        bool
	anchorMatches int
}

type exploreTypedAnchorCall struct {
	consumer *exploreTypedAnchorConsumer
	memberID string
}

type exploreTypedAnchorProjection struct {
	field           *exploreTypedAnchorField
	consumer        *graph.Node
	member          *graph.Node
	memberMatches   int
	consumerMatches int
	directFieldUse  bool
	exactType       bool
}

// projectExploreTypedAnchorCandidates repairs a narrow blind spot in the
// ordinary localization window. A distinctive syntactic anchor can retain a
// typed field without the behavior that consumes it. One consumer/member pair
// is admitted only when the graph proves all of:
//
//	field --member_of--> owner <--member_of-- consumer --calls--> member
//	member --member_of--> the field's declared type
//
// A direct field-use edge proves the consumer. Without one, the owner member
// itself must match the same active task anchor. Every graph stage is batched,
// canonically ordered, and structurally capped. Cap overflow, incomplete
// hydration, ambiguous strongest proofs, and request cancellation are all
// conservative no-ops; wall-clock scheduling never changes correctness.
func projectExploreTypedAnchorCandidates(
	ctx context.Context,
	task string,
	candidates []*rerank.Candidate,
	reader exploreTypedAnchorBatchReader,
	scope query.QueryOptions,
	maxSymbols int,
	protectedAnchors map[int]string,
	protectedImplementationID string,
) []*rerank.Candidate {
	if ctx == nil {
		ctx = context.Background()
	}
	if reader == nil || ctx.Err() != nil || len(candidates) == 0 || maxSymbols < 3 || len(protectedAnchors) == 0 {
		return candidates
	}
	projection, ok := findExploreTypedAnchorProjection(ctx, task, candidates, reader, scope, protectedAnchors)
	if !ok || ctx.Err() != nil {
		return candidates
	}
	reserved := exploreTypedAnchorReservedCandidateIDs(candidates, protectedAnchors, protectedImplementationID)
	return reserveExploreTypedAnchorProjection(candidates, projection, reserved, maxSymbols)
}

func findExploreTypedAnchorProjection(
	ctx context.Context,
	task string,
	candidates []*rerank.Candidate,
	reader exploreTypedAnchorBatchReader,
	scope query.QueryOptions,
	protected map[int]string,
) (exploreTypedAnchorProjection, bool) {
	anchors := exploreSyntacticAnchors(task)
	active := exploreTypedAnchorActiveIndexes(anchors, protected)
	if len(active) == 0 || ctx.Err() != nil {
		return exploreTypedAnchorProjection{}, false
	}

	fields := make([]*exploreTypedAnchorField, 0, exploreTypedAnchorFieldLimit)
	for _, candidate := range candidates {
		if candidate == nil || candidate.Node == nil || candidate.Node.Kind != graph.KindField {
			continue
		}
		indexes := exploreTypedAnchorMatchingIndexes(
			anchors, active, candidate.Node, exploreTypedAnchorFieldType(candidate.Node),
		)
		if len(indexes) == 0 {
			continue
		}
		if len(fields) == exploreTypedAnchorFieldLimit {
			return exploreTypedAnchorProjection{}, false
		}
		fields = append(fields, &exploreTypedAnchorField{
			candidate: candidate, anchorIndexes: indexes, field: candidate.Node,
			canonicalType: exploreTypedAnchorCanonicalType(exploreTypedAnchorFieldType(candidate.Node)),
		})
	}
	if len(fields) == 0 || !hydrateExploreTypedAnchorFields(ctx, fields, reader, scope) {
		return exploreTypedAnchorProjection{}, false
	}

	consumers, ok := hydrateExploreTypedAnchorConsumers(ctx, fields, reader, scope, anchors)
	if !ok || len(consumers) == 0 {
		return exploreTypedAnchorProjection{}, false
	}
	projections, ok := hydrateExploreTypedAnchorCalls(ctx, consumers, reader, scope, anchors)
	if !ok || len(projections) == 0 {
		return exploreTypedAnchorProjection{}, false
	}
	return uniqueStrongestExploreTypedAnchorProjection(projections)
}

func exploreTypedAnchorActiveIndexes(anchors []exploreSyntacticAnchor, protected map[int]string) []int {
	indexes := make([]int, 0, len(protected))
	for index, id := range protected {
		if id != "" && index >= 0 && index < len(anchors) {
			indexes = append(indexes, index)
		}
	}
	sort.Ints(indexes)
	return indexes
}

func hydrateExploreTypedAnchorFields(
	ctx context.Context,
	fields []*exploreTypedAnchorField,
	reader exploreTypedAnchorBatchReader,
	scope query.QueryOptions,
) bool {
	fieldIDs := make([]string, 0, len(fields))
	for _, field := range fields {
		fieldIDs = append(fieldIDs, field.field.ID)
	}
	out, complete := exploreTypedAnchorOutgoingIdentitiesBounded(
		ctx,
		reader,
		fieldIDs,
		[]graph.EdgeKind{graph.EdgeMemberOf, graph.EdgeTypedAs},
		exploreTypedAnchorFieldRelationLimit,
	)
	if !complete {
		return false
	}

	relations := make(map[string][]graph.EdgeIdentity, len(fields))
	endpointIDs := make([]string, 0, len(fields)*exploreTypedAnchorFieldRelationLimit)
	for _, field := range fields {
		edges := append([]graph.EdgeIdentity(nil), out[field.field.ID]...)
		exploreTypedAnchorSortIdentities(edges)
		relations[field.field.ID] = edges
		for _, edge := range edges {
			if edge.From != field.field.ID || edge.To == "" {
				return false
			}
			endpointIDs = append(endpointIDs, edge.To)
		}
	}
	nodes, complete := exploreTypedAnchorNodesByIDsBounded(
		ctx,
		reader,
		endpointIDs,
		exploreTypedAnchorFieldLimit*exploreTypedAnchorFieldRelationLimit,
	)
	if !complete {
		return false
	}

	kept := fields[:0]
	for _, field := range fields {
		owners := make(map[string]*graph.Node)
		field.typedIDs = make(map[string]struct{})
		for _, edge := range relations[field.field.ID] {
			node := nodes[edge.To]
			if !exploreNodeWithinQueryScope(node, scope) {
				continue
			}
			switch edge.Kind {
			case graph.EdgeMemberOf:
				if exploreTypedAnchorTypeNode(node) {
					owners[node.ID] = node
				}
			case graph.EdgeTypedAs:
				if exploreTypedAnchorTypeNode(node) {
					field.typedIDs[node.ID] = struct{}{}
				}
			}
		}
		if len(owners) != 1 || (len(field.typedIDs) == 0 && field.canonicalType == "") {
			continue
		}
		for _, owner := range owners {
			field.owner = owner
		}
		kept = append(kept, field)
	}
	for index := len(kept); index < len(fields); index++ {
		fields[index] = nil
	}
	return len(kept) > 0
}

func hydrateExploreTypedAnchorConsumers(
	ctx context.Context,
	fields []*exploreTypedAnchorField,
	reader exploreTypedAnchorBatchReader,
	scope query.QueryOptions,
	anchors []exploreSyntacticAnchor,
) ([]*exploreTypedAnchorConsumer, bool) {
	// hydrateExploreTypedAnchorFields filters in place but cannot resize its
	// caller's slice. Ignore any cleared or unhydrated seed here.
	fieldIDs := make([]string, 0, len(fields))
	ownerIDs := make([]string, 0, len(fields))
	for _, field := range fields {
		if field == nil || field.owner == nil {
			continue
		}
		fieldIDs = append(fieldIDs, field.field.ID)
		ownerIDs = append(ownerIDs, field.owner.ID)
	}
	if len(fieldIDs) == 0 {
		return nil, false
	}
	fieldIn, complete := exploreTypedAnchorIncomingIdentitiesBounded(
		ctx,
		reader,
		fieldIDs,
		[]graph.EdgeKind{graph.EdgeReads, graph.EdgeWrites, graph.EdgeAccessesField},
		exploreTypedAnchorConsumerLimit,
	)
	if !complete {
		return nil, false
	}
	ownerIn, complete := exploreTypedAnchorIncomingIdentitiesBounded(
		ctx,
		reader,
		ownerIDs,
		[]graph.EdgeKind{graph.EdgeMemberOf},
		exploreTypedAnchorOwnerMemberLimit,
	)
	if !complete {
		return nil, false
	}

	type rawConsumer struct {
		id     string
		direct bool
	}
	rawByField := make(map[string]map[string]rawConsumer, len(fields))
	consumerIDs := make([]string, 0, len(fields)*exploreTypedAnchorOwnerMemberLimit)
	for _, field := range fields {
		if field == nil || field.owner == nil {
			continue
		}
		direct := append([]graph.EdgeIdentity(nil), fieldIn[field.field.ID]...)
		members := append([]graph.EdgeIdentity(nil), ownerIn[field.owner.ID]...)
		exploreTypedAnchorSortIdentities(direct)
		exploreTypedAnchorSortIdentities(members)
		byID := make(map[string]rawConsumer, len(direct)+len(members))
		for _, edge := range members {
			if edge.From == "" || edge.To != field.owner.ID {
				return nil, false
			}
			byID[edge.From] = rawConsumer{id: edge.From}
		}
		for _, edge := range direct {
			if edge.From == "" || edge.To != field.field.ID {
				return nil, false
			}
			consumer, member := byID[edge.From]
			if !member {
				// A field read outside the owning type does not prove the
				// field→owner←consumer leg required by this projection.
				continue
			}
			consumer.direct = true
			byID[edge.From] = consumer
		}
		rawByField[field.field.ID] = byID
		for id := range byID {
			consumerIDs = append(consumerIDs, id)
		}
	}
	nodes, complete := exploreTypedAnchorNodesByIDsBounded(
		ctx,
		reader,
		consumerIDs,
		exploreTypedAnchorFieldLimit*exploreTypedAnchorOwnerMemberLimit,
	)
	if !complete {
		return nil, false
	}

	consumers := make([]*exploreTypedAnchorConsumer, 0, len(consumerIDs))
	for _, field := range fields {
		if field == nil || field.owner == nil {
			continue
		}
		local := make([]*exploreTypedAnchorConsumer, 0, len(rawByField[field.field.ID]))
		for _, raw := range rawByField[field.field.ID] {
			node := nodes[raw.id]
			if node == nil {
				return nil, false
			}
			if !exploreTypedAnchorCallable(node, scope) {
				continue
			}
			matches := exploreTypedAnchorMatchCount(anchors, field.anchorIndexes, node, "")
			if !raw.direct && matches == 0 {
				continue
			}
			local = append(local, &exploreTypedAnchorConsumer{
				field: field, node: node, direct: raw.direct, anchorMatches: matches,
			})
		}
		sort.SliceStable(local, func(i, j int) bool {
			if local[i].direct != local[j].direct {
				return local[i].direct
			}
			if local[i].anchorMatches != local[j].anchorMatches {
				return local[i].anchorMatches > local[j].anchorMatches
			}
			return local[i].node.ID < local[j].node.ID
		})
		if len(local) > exploreTypedAnchorConsumerLimit {
			return nil, false
		}
		consumers = append(consumers, local...)
	}
	return consumers, true
}

func hydrateExploreTypedAnchorCalls(
	ctx context.Context,
	consumers []*exploreTypedAnchorConsumer,
	reader exploreTypedAnchorBatchReader,
	scope query.QueryOptions,
	anchors []exploreSyntacticAnchor,
) ([]exploreTypedAnchorProjection, bool) {
	consumerIDs := make([]string, 0, len(consumers))
	consumerByID := make(map[string][]*exploreTypedAnchorConsumer, len(consumers))
	for _, consumer := range consumers {
		consumerIDs = append(consumerIDs, consumer.node.ID)
		consumerByID[consumer.node.ID] = append(consumerByID[consumer.node.ID], consumer)
	}
	out, complete := exploreTypedAnchorOutgoingIdentitiesBounded(
		ctx,
		reader,
		consumerIDs,
		[]graph.EdgeKind{graph.EdgeCalls},
		exploreTypedAnchorCallLimit,
	)
	if !complete {
		return nil, false
	}

	calls := make([]exploreTypedAnchorCall, 0, exploreTypedAnchorCallLimit)
	seenCalls := make(map[string]struct{})
	for _, consumerID := range exploreTypedAnchorSortedIDs(consumerIDs) {
		edges := append([]graph.EdgeIdentity(nil), out[consumerID]...)
		exploreTypedAnchorSortIdentities(edges)
		for _, edge := range edges {
			if edge.From != consumerID || edge.To == "" {
				return nil, false
			}
		}
		if len(calls)+len(edges)*len(consumerByID[consumerID]) > exploreTypedAnchorCallLimit {
			return nil, false
		}
		for _, consumer := range consumerByID[consumerID] {
			for _, edge := range edges {
				key := consumer.field.field.ID + "\x00" + consumer.node.ID + "\x00" + edge.To
				if _, duplicate := seenCalls[key]; duplicate {
					continue
				}
				seenCalls[key] = struct{}{}
				calls = append(calls, exploreTypedAnchorCall{consumer: consumer, memberID: edge.To})
			}
		}
	}
	if len(calls) == 0 {
		return nil, true
	}

	memberIDs := make([]string, 0, len(calls))
	for _, call := range calls {
		memberIDs = append(memberIDs, call.memberID)
	}
	memberOut, complete := exploreTypedAnchorOutgoingIdentitiesBounded(
		ctx,
		reader,
		memberIDs,
		[]graph.EdgeKind{graph.EdgeMemberOf},
		exploreTypedAnchorMemberOwnerLimit,
	)
	if !complete {
		return nil, false
	}
	memberRelations := make(map[string][]graph.EdgeIdentity)
	ownerIDs := make([]string, 0, len(memberIDs)*exploreTypedAnchorMemberOwnerLimit)
	for _, memberID := range exploreTypedAnchorSortedIDs(memberIDs) {
		edges := append([]graph.EdgeIdentity(nil), memberOut[memberID]...)
		exploreTypedAnchorSortIdentities(edges)
		memberRelations[memberID] = edges
		for _, edge := range edges {
			if edge.From != memberID || edge.To == "" {
				return nil, false
			}
			ownerIDs = append(ownerIDs, edge.To)
		}
	}
	hydrationIDs := append(exploreTypedAnchorSortedIDs(memberIDs), ownerIDs...)
	nodes, complete := exploreTypedAnchorNodesByIDsBounded(
		ctx,
		reader,
		hydrationIDs,
		exploreTypedAnchorCallLimit*(exploreTypedAnchorMemberOwnerLimit+1),
	)
	if !complete {
		return nil, false
	}

	projections := make([]exploreTypedAnchorProjection, 0, len(calls))
	for _, call := range calls {
		member := nodes[call.memberID]
		if member == nil {
			return nil, false
		}
		if !exploreTypedAnchorCallable(member, scope) {
			continue
		}
		memberMatches := exploreTypedAnchorMatchCount(
			anchors, call.consumer.field.anchorIndexes, member, "",
		)
		if memberMatches == 0 {
			continue
		}
		exactType, typeMatches, complete := exploreTypedAnchorMemberTypeProof(
			call.consumer.field, memberRelations[member.ID], nodes, scope,
		)
		if !complete {
			return nil, false
		}
		if !typeMatches {
			continue
		}
		projections = append(projections, exploreTypedAnchorProjection{
			field: call.consumer.field, consumer: call.consumer.node, member: member,
			memberMatches: memberMatches, consumerMatches: call.consumer.anchorMatches,
			directFieldUse: call.consumer.direct, exactType: exactType,
		})
	}
	return projections, true
}

func exploreTypedAnchorMemberTypeProof(
	field *exploreTypedAnchorField,
	edges []graph.EdgeIdentity,
	nodes map[string]*graph.Node,
	scope query.QueryOptions,
) (exact, matches, complete bool) {
	complete = true
	for _, edge := range edges {
		owner := nodes[edge.To]
		if owner == nil {
			return false, false, false
		}
		if !exploreTypedAnchorTypeNode(owner) || !exploreNodeWithinQueryScope(owner, scope) {
			continue
		}
		if len(field.typedIDs) > 0 {
			if _, ok := field.typedIDs[owner.ID]; ok {
				return true, true, true
			}
			continue
		}
		if field.canonicalType != "" && exploreTypedAnchorCanonicalTypeMatches(field.canonicalType, owner) {
			return false, true, true
		}
	}
	return false, false, complete
}

func uniqueStrongestExploreTypedAnchorProjection(projections []exploreTypedAnchorProjection) (exploreTypedAnchorProjection, bool) {
	if len(projections) == 0 {
		return exploreTypedAnchorProjection{}, false
	}
	best := projections[0]
	ambiguous := false
	for _, projection := range projections[1:] {
		switch exploreTypedAnchorProjectionCompare(projection, best) {
		case 1:
			best = projection
			ambiguous = false
		case 0:
			if projection.consumer.ID != best.consumer.ID || projection.member.ID != best.member.ID {
				ambiguous = true
			}
		}
	}
	return best, !ambiguous
}

func exploreTypedAnchorProjectionCompare(left, right exploreTypedAnchorProjection) int {
	leftStrength := []int{
		boolInt(left.exactType), boolInt(left.directFieldUse), left.memberMatches, left.consumerMatches,
	}
	rightStrength := []int{
		boolInt(right.exactType), boolInt(right.directFieldUse), right.memberMatches, right.consumerMatches,
	}
	for index := range leftStrength {
		if leftStrength[index] > rightStrength[index] {
			return 1
		}
		if leftStrength[index] < rightStrength[index] {
			return -1
		}
	}
	return 0
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func exploreTypedAnchorMatchingIndexes(
	anchors []exploreSyntacticAnchor,
	indexes []int,
	node *graph.Node,
	extra string,
) []int {
	matched := make([]int, 0, len(indexes))
	for _, index := range indexes {
		if index < 0 || index >= len(anchors) {
			continue
		}
		anchor := anchors[index]
		if exploreSyntacticAnchorMatchesNode(anchor, node) ||
			(extra != "" && exploreSyntacticAnchorMatchesIdentifier(anchor, extra)) {
			matched = append(matched, index)
		}
	}
	return matched
}

func exploreTypedAnchorMatchCount(anchors []exploreSyntacticAnchor, indexes []int, node *graph.Node, extra string) int {
	return len(exploreTypedAnchorMatchingIndexes(anchors, indexes, node, extra))
}

func exploreTypedAnchorCallable(node *graph.Node, scope query.QueryOptions) bool {
	if node == nil || (node.Kind != graph.KindFunction && node.Kind != graph.KindMethod) || !exploreNodeWithinQueryScope(node, scope) {
		return false
	}
	isTest, _ := node.Meta["is_test"].(bool)
	return !isTest
}

func exploreTypedAnchorTypeNode(node *graph.Node) bool {
	return node != nil && (node.Kind == graph.KindType || node.Kind == graph.KindInterface)
}

func exploreTypedAnchorFieldType(field *graph.Node) string {
	if field == nil || field.Meta == nil {
		return ""
	}
	value, _ := field.Meta["field_type"].(string)
	return strings.TrimSpace(value)
}

// exploreTypedAnchorCanonicalType extracts one exact outer type identity. It
// strips references, array/nullable suffixes and generic arguments, retaining
// at most the final namespace/module qualifier. It deliberately does not accept
// shared component words or peer wrapper contents. If the graph cannot provide
// typed_as, false positives are more damaging than a conservative no-op.
func exploreTypedAnchorCanonicalType(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if index := strings.IndexByte(value, '<'); index >= 0 {
		value = value[:index]
	}
	value = strings.TrimSpace(strings.TrimRight(value, "?[]*& "))
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return ""
	}
	value = fields[len(fields)-1]
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ':' || r == '.' || r == '/' || r == '\\'
	})
	identities := make([]string, 0, 2)
	for _, part := range parts {
		tokens := rerank.Tokenize(part)
		if len(tokens) == 0 {
			continue
		}
		identities = append(identities, strings.ToLower(strings.TrimSpace(tokens[len(tokens)-1])))
	}
	if len(identities) > 2 {
		identities = identities[len(identities)-2:]
	}
	return strings.Join(identities, ".")
}

func exploreTypedAnchorCanonicalTypeMatches(fieldIdentity string, owner *graph.Node) bool {
	if fieldIdentity == "" || owner == nil {
		return false
	}
	if !strings.Contains(fieldIdentity, ".") {
		return fieldIdentity == exploreTypedAnchorCanonicalType(owner.Name)
	}
	return fieldIdentity == exploreTypedAnchorCanonicalType(owner.QualName)
}

func exploreTypedAnchorSortedIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	unique := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id != "" {
			unique[id] = struct{}{}
		}
	}
	result := make([]string, 0, len(unique))
	for id := range unique {
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}

func exploreTypedAnchorReservedCandidateIDs(
	candidates []*rerank.Candidate,
	protectedAnchors map[int]string,
	protectedImplementationID string,
) map[string]struct{} {
	reserved := make(map[string]struct{}, len(protectedAnchors)+4)
	if len(candidates) > 0 && candidates[0] != nil && candidates[0].Node != nil {
		reserved[candidates[0].Node.ID] = struct{}{}
	}
	for _, id := range protectedAnchors {
		if id != "" {
			reserved[id] = struct{}{}
		}
	}
	if protectedImplementationID != "" {
		reserved[protectedImplementationID] = struct{}{}
	}
	for _, candidate := range candidates {
		if candidate == nil || candidate.Node == nil || candidate.Signals == nil {
			continue
		}
		if candidate.Signals[exploreContentRecallExactSignal] > 0 ||
			candidate.Signals[exploreSourceLiteralSignal] > 0 ||
			candidate.Signals[exploreSourceLiteralCalleeSignal] > 0 ||
			candidate.Signals[exploreTypedAnchorProjectionSignal] > 0 {
			reserved[candidate.Node.ID] = struct{}{}
		}
	}
	return reserved
}

// exploreFinalReservedCandidateIDs extends the pre-projection reservation set
// only after typed admission has completed. This keeps a weak marginal concept
// complement from blocking stronger typed proof while still protecting the
// admitted complement from owner-folding eviction.
func exploreFinalReservedCandidateIDs(
	candidates []*rerank.Candidate,
	protectedAnchors map[int]string,
	protectedImplementationID string,
) map[string]struct{} {
	reserved := exploreTypedAnchorReservedCandidateIDs(
		candidates, protectedAnchors, protectedImplementationID,
	)
	for _, candidate := range candidates {
		if candidate == nil || candidate.Node == nil || candidate.Signals == nil {
			continue
		}
		if candidate.Signals[exploreConceptComplementSignal] > 0 {
			reserved[candidate.Node.ID] = struct{}{}
		}
	}
	return reserved
}

func reserveExploreTypedAnchorProjection(
	candidates []*rerank.Candidate,
	projection exploreTypedAnchorProjection,
	reserved map[string]struct{},
	maxSymbols int,
) []*rerank.Candidate {
	if projection.field == nil || projection.field.candidate == nil || projection.field.field == nil ||
		projection.consumer == nil || projection.member == nil {
		return candidates
	}
	mustKeep := make(map[string]struct{}, len(reserved)+3)
	for id := range reserved {
		mustKeep[id] = struct{}{}
	}
	mustKeep[projection.field.field.ID] = struct{}{}
	mustKeep[projection.consumer.ID] = struct{}{}
	mustKeep[projection.member.ID] = struct{}{}
	if len(mustKeep) > maxSymbols {
		return candidates
	}

	existing := make(map[string]struct{}, len(candidates))
	marked := make([]*rerank.Candidate, 0, len(candidates)+2)
	for _, candidate := range candidates {
		if candidate == nil || candidate.Node == nil {
			marked = append(marked, candidate)
			continue
		}
		id := candidate.Node.ID
		if _, duplicate := existing[id]; duplicate {
			continue
		}
		existing[id] = struct{}{}
		if id == projection.consumer.ID || id == projection.member.ID {
			candidate = exploreTypedAnchorMarkedCandidate(candidate)
		}
		marked = append(marked, candidate)
	}

	additions := make([]*rerank.Candidate, 0, 2)
	for _, node := range []*graph.Node{projection.consumer, projection.member} {
		if _, present := existing[node.ID]; present {
			continue
		}
		existing[node.ID] = struct{}{}
		additions = append(additions, exploreTypedAnchorProjectedCandidate(node, projection.field.candidate.Score))
	}
	expanded := make([]*rerank.Candidate, 0, len(marked)+len(additions))
	inserted := false
	for _, candidate := range marked {
		expanded = append(expanded, candidate)
		if candidate != nil && candidate.Node != nil && candidate.Node.ID == projection.field.field.ID {
			expanded = append(expanded, additions...)
			inserted = true
		}
	}
	if !inserted {
		return candidates
	}
	for len(expanded) > maxSymbols {
		remove := -1
		for index := len(expanded) - 1; index >= 0; index-- {
			candidate := expanded[index]
			if candidate == nil || candidate.Node == nil {
				remove = index
				break
			}
			if _, keep := mustKeep[candidate.Node.ID]; !keep {
				remove = index
				break
			}
		}
		if remove < 0 {
			return candidates
		}
		expanded = append(expanded[:remove], expanded[remove+1:]...)
	}
	return expanded
}

func exploreTypedAnchorMarkedCandidate(candidate *rerank.Candidate) *rerank.Candidate {
	clone := *candidate
	clone.Signals = make(map[string]float64, len(candidate.Signals)+1)
	for key, value := range candidate.Signals {
		clone.Signals[key] = value
	}
	clone.Signals[exploreTypedAnchorProjectionSignal] = 1
	return &clone
}

func exploreTypedAnchorProjectedCandidate(node *graph.Node, score float64) *rerank.Candidate {
	return &rerank.Candidate{
		Node: node, TextRank: -1, VectorRank: -1, Score: score,
		Signals: map[string]float64{exploreTypedAnchorProjectionSignal: 1},
	}
}

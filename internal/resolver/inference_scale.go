package resolver

import (
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// Mutation batches bound the adjacency maps used for warm no-op checks.
// The censuses themselves are collected in ONE projection walk over the full
// repo list: the store filters scoped projections only after reading each
// row, so paging repos N at a time multiplied the workspace-wide edge-kind
// walks — the dominant cost — by ceil(repos/N) while bounding only compact
// method-name maps that never needed bounding.
const inferenceMutationBatchSize = 512

type implementationInterface struct {
	id      string
	methods []string
}

type implementationType struct {
	node    *graph.Node
	methods map[string]struct{}
}

type overrideCandidate struct {
	from   *graph.Node
	to     *graph.Node
	origin string
}

func inferencePairKey(from, to string) string { return from + "\x00" + to }

func allInferenceRepos(store graph.Store) []string {
	seen := map[string]struct{}{"": {}}
	for _, repo := range store.RepoPrefixes() {
		seen[repo] = struct{}{}
	}
	return sortedInferenceRepos(seen)
}

func sortedInferenceRepos(seen map[string]struct{}) []string {
	repos := make([]string, 0, len(seen))
	for repo := range seen {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	return repos
}

func implementationInferenceRepos(store graph.Store, scopeTypes, scopeIfaces map[string]bool) []string {
	if scopeTypes == nil {
		return allInferenceRepos(store)
	}
	ids := make([]string, 0, len(scopeTypes)+len(scopeIfaces))
	for id, affected := range scopeTypes {
		if affected {
			ids = append(ids, id)
		}
	}
	for id, affected := range scopeIfaces {
		if affected {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	nodes := store.GetNodesByIDs(ids)
	repos := make(map[string]struct{})
	for _, node := range nodes {
		if node != nil {
			repos[node.RepoPrefix] = struct{}{}
		}
	}
	return sortedInferenceRepos(repos)
}

func overrideInferenceRepos(store graph.Store, scope map[string]bool) []string {
	if scope == nil {
		return allInferenceRepos(store)
	}
	ids := make([]string, 0, len(scope))
	for id, affected := range scope {
		if affected {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}

	// A scoped parent can be the target of a structural edge owned by a child
	// in another repository. One batched incoming lookup discovers those source
	// repositories without falling back to a workspace scan.
	repos := make(map[string]struct{})
	for _, node := range store.GetNodesByIDs(ids) {
		if node != nil {
			repos[node.RepoPrefix] = struct{}{}
		}
	}
	incoming := store.GetInEdgesByNodeIDs(ids)
	childIDs := make([]string, 0)
	for _, id := range ids {
		for _, edge := range incoming[id] {
			if edge != nil && isOverrideParentKind(edge.Kind) {
				childIDs = append(childIDs, edge.From)
			}
		}
	}
	for _, node := range store.GetNodesByIDs(childIDs) {
		if node != nil {
			repos[node.RepoPrefix] = struct{}{}
		}
	}
	return sortedInferenceRepos(repos)
}

func interfaceMethodNames(meta map[string]any) []string {
	if meta == nil {
		return nil
	}
	raw, ok := meta["methods"]
	if !ok {
		return nil
	}
	set := make(map[string]struct{})
	switch methods := raw.(type) {
	case []string:
		for _, method := range methods {
			set[method] = struct{}{}
		}
	case []any:
		for _, method := range methods {
			if name, ok := method.(string); ok {
				set[name] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(set))
	for method := range set {
		out = append(out, method)
	}
	sort.Strings(out)
	return out
}

func collectImplementationInterfaces(store graph.Store, repos []string) map[string][]implementationInterface {
	byRepo := make(map[string][]implementationInterface)
	for node := range graph.NodesInScopeSeq(store, repos, nil, graph.KindInterface) {
		if node == nil {
			continue
		}
		methods := interfaceMethodNames(node.Meta)
		if len(methods) == 0 {
			continue
		}
		byRepo[node.RepoPrefix] = append(byRepo[node.RepoPrefix], implementationInterface{
			id: node.ID, methods: methods,
		})
	}
	for repo := range byRepo {
		sort.Slice(byRepo[repo], func(i, j int) bool { return byRepo[repo][i].id < byRepo[repo][j].id })
	}
	return byRepo
}

func collectImplementationTypes(store graph.Store, repos []string, ifacesByRepo map[string][]implementationInterface) map[string]map[string]*implementationType {
	byRepo := make(map[string]map[string]*implementationType)
	for row := range graph.EdgesInScopeSeq(store, repos, nil, graph.EdgeMemberOf) {
		method, owner := row.Source, row.Target
		if method == nil || owner == nil || method.Kind != graph.KindMethod {
			continue
		}
		if owner.Kind != graph.KindType && owner.Kind != graph.KindInterface {
			continue
		}
		if len(ifacesByRepo[owner.RepoPrefix]) == 0 {
			continue
		}
		types := byRepo[owner.RepoPrefix]
		if types == nil {
			types = make(map[string]*implementationType)
			byRepo[owner.RepoPrefix] = types
		}
		typeInfo := types[owner.ID]
		if typeInfo == nil {
			typeInfo = &implementationType{node: owner, methods: make(map[string]struct{})}
			types[owner.ID] = typeInfo
		}
		typeInfo.methods[method.Name] = struct{}{}
	}
	return byRepo
}

// collectImplementationTypesProjected uses the compact member-method join on
// whole-workspace runs. Only owner type nodes are hydrated, once in an ID batch;
// method edge payloads and method Meta never cross the storage boundary.
func collectImplementationTypesProjected(
	store graph.Store,
	ifacesByRepo map[string][]implementationInterface,
) (map[string]map[string]*implementationType, bool) {
	projector, ok := store.(graph.MemberMethodsByType)
	if !ok {
		return nil, false
	}
	rowsByType := projector.MemberMethodsByType()
	typeIDs := make([]string, 0, len(rowsByType))
	for typeID := range rowsByType {
		typeIDs = append(typeIDs, typeID)
	}
	sort.Strings(typeIDs)
	owners := store.GetNodesByIDs(typeIDs)
	byRepo := make(map[string]map[string]*implementationType)
	for _, typeID := range typeIDs {
		owner := owners[typeID]
		if owner == nil || (owner.Kind != graph.KindType && owner.Kind != graph.KindInterface) {
			continue
		}
		if len(ifacesByRepo[owner.RepoPrefix]) == 0 {
			continue
		}
		types := byRepo[owner.RepoPrefix]
		if types == nil {
			types = make(map[string]*implementationType)
			byRepo[owner.RepoPrefix] = types
		}
		typeInfo := &implementationType{node: owner, methods: make(map[string]struct{})}
		for _, method := range rowsByType[typeID] {
			typeInfo.methods[method.Name] = struct{}{}
		}
		types[typeID] = typeInfo
	}
	return byRepo, true
}

func collectExistingImplementationPairs(store graph.Store, repos []string) map[string]struct{} {
	existing := make(map[string]struct{})
	for row := range graph.EdgesInScopeSeq(store, repos, nil, graph.EdgeImplements) {
		if row.Edge != nil {
			existing[inferencePairKey(row.Edge.From, row.Edge.To)] = struct{}{}
		}
	}
	return existing
}

// visitImplementationMatches uses one rarest-required-method posting per
// interface. Every satisfying type necessarily has that anchor, so this is
// complete while avoiding the old types×interfaces Cartesian loop. It returns
// the number of actual method-set comparisons for scale regression tests.
func visitImplementationMatches(
	typesByRepo map[string]map[string]*implementationType,
	ifacesByRepo map[string][]implementationInterface,
	scopeTypes, scopeIfaces map[string]bool,
	visit func(*implementationType, implementationInterface),
) int {
	repos := make([]string, 0, len(ifacesByRepo))
	for repo := range ifacesByRepo {
		repos = append(repos, repo)
	}
	sort.Strings(repos)

	comparisons := 0
	for _, repo := range repos {
		types := typesByRepo[repo]
		if len(types) == 0 {
			continue
		}
		frequency := make(map[string]int)
		for _, typeInfo := range types {
			for method := range typeInfo.methods {
				frequency[method]++
			}
		}
		postings := make(map[string][]implementationInterface)
		for _, iface := range ifacesByRepo[repo] {
			anchor := iface.methods[0]
			anchorFrequency := frequency[anchor]
			for _, method := range iface.methods[1:] {
				if count := frequency[method]; count < anchorFrequency {
					anchor, anchorFrequency = method, count
				}
			}
			if anchorFrequency > 0 {
				postings[anchor] = append(postings[anchor], iface)
			}
		}

		typeIDs := make([]string, 0, len(types))
		for typeID := range types {
			typeIDs = append(typeIDs, typeID)
		}
		sort.Strings(typeIDs)
		for _, typeID := range typeIDs {
			typeInfo := types[typeID]
			methodNames := make([]string, 0, len(typeInfo.methods))
			for method := range typeInfo.methods {
				if len(postings[method]) > 0 {
					methodNames = append(methodNames, method)
				}
			}
			sort.Strings(methodNames)
			for _, method := range methodNames {
				for _, iface := range postings[method] {
					if iface.id == typeID {
						continue
					}
					if scopeTypes != nil && !scopeTypes[typeID] && !scopeIfaces[iface.id] {
						continue
					}
					comparisons++
					satisfies := true
					for _, required := range iface.methods {
						if _, ok := typeInfo.methods[required]; !ok {
							satisfies = false
							break
						}
					}
					if satisfies {
						visit(typeInfo, iface)
					}
				}
			}
		}
	}
	return comparisons
}

func methodSetImplementationEdge(owner *graph.Node, iface string) *graph.Edge {
	return &graph.Edge{
		From: owner.ID, To: iface, Kind: graph.EdgeImplements,
		FilePath: owner.FilePath, Line: owner.StartLine,
		Meta: map[string]any{"via": MetaViaMethodSetInference},
	}
}

func (r *Resolver) inferImplements(scopeTypes, scopeIfaces map[string]bool) int {
	return r.inferImplementationsByRepo(scopeTypes, scopeIfaces)
}

func (r *Resolver) inferImplementationsByRepo(scopeTypes, scopeIfaces map[string]bool) int {
	repos := implementationInferenceRepos(r.graph, scopeTypes, scopeIfaces)
	if len(repos) == 0 {
		return 0
	}
	ifacesByRepo := collectImplementationInterfaces(r.graph, repos)
	if len(ifacesByRepo) == 0 {
		return 0
	}
	var typesByRepo map[string]map[string]*implementationType
	if scopeTypes == nil && scopeIfaces == nil {
		if projected, ok := collectImplementationTypesProjected(r.graph, ifacesByRepo); ok {
			typesByRepo = projected
		} else {
			typesByRepo = collectImplementationTypes(r.graph, repos, ifacesByRepo)
		}
	} else {
		typesByRepo = collectImplementationTypes(r.graph, repos, ifacesByRepo)
	}
	if len(typesByRepo) == 0 {
		return 0
	}
	existing := collectExistingImplementationPairs(r.graph, repos)
	added := 0
	batch := make([]*graph.Edge, 0, inferenceMutationBatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		r.graph.AddBatch(nil, batch)
		batch = make([]*graph.Edge, 0, inferenceMutationBatchSize)
	}
	visitImplementationMatches(typesByRepo, ifacesByRepo, scopeTypes, scopeIfaces,
		func(typeInfo *implementationType, iface implementationInterface) {
			key := inferencePairKey(typeInfo.node.ID, iface.id)
			if _, found := existing[key]; found {
				return
			}
			existing[key] = struct{}{}
			batch = append(batch, methodSetImplementationEdge(typeInfo.node, iface.id))
			added++
			if len(batch) == inferenceMutationBatchSize {
				flush()
			}
		})
	flush()
	return added
}

func isOverrideParentKind(kind graph.EdgeKind) bool {
	return kind == graph.EdgeExtends || kind == graph.EdgeImplements || kind == graph.EdgeComposes
}

func relevantOverrideParent(row graph.ScopedEdgeRow, scope map[string]bool) bool {
	if row.Edge == nil || row.Source == nil || row.Target == nil || row.Edge.From == row.Edge.To {
		return false
	}
	if !isOverrideParentKind(row.Edge.Kind) {
		return false
	}
	if row.Source.Kind != graph.KindType && row.Source.Kind != graph.KindInterface {
		return false
	}
	if row.Target.Kind != graph.KindType && row.Target.Kind != graph.KindInterface {
		return false
	}
	return scope == nil || scope[row.Edge.From] || scope[row.Edge.To]
}

func overrideOrigin(parentOrigin string) string {
	if parentOrigin == graph.OriginASTResolved {
		return graph.OriginASTResolved
	}
	if graph.OriginRank(parentOrigin) >= graph.OriginRank(graph.OriginLSPDispatch) {
		return parentOrigin
	}
	return graph.OriginASTInferred
}

func overrideMethodRepos(store graph.Store, sourceRepos []string, scope map[string]bool) []string {
	repos := make(map[string]struct{}, len(sourceRepos))
	for _, repo := range sourceRepos {
		repos[repo] = struct{}{}
	}
	for row := range graph.EdgesInScopeSeq(store, sourceRepos, nil,
		graph.EdgeExtends, graph.EdgeImplements, graph.EdgeComposes) {
		if relevantOverrideParent(row, scope) {
			repos[row.Target.RepoPrefix] = struct{}{}
		}
	}
	return sortedInferenceRepos(repos)
}

func collectOverrideMethods(store graph.Store, repos []string) map[string]map[string]*graph.Node {
	byType := make(map[string]map[string]*graph.Node)
	for row := range graph.EdgesInScopeSeq(store, repos, nil, graph.EdgeMemberOf) {
		method, owner := row.Source, row.Target
		if method == nil || owner == nil || method.Kind != graph.KindMethod {
			continue
		}
		methods := byType[owner.ID]
		if methods == nil {
			methods = make(map[string]*graph.Node)
			byType[owner.ID] = methods
		}
		// Projection order is edge-ID order, matching MemberMethodsByType's
		// stable last-row winner for duplicate method names.
		methods[method.Name] = method
	}
	return byType
}

// collectOverrideMethodsProjected builds the same stable last-name-winner map
// from the compact member-method projection. The lightweight nodes carry every
// field override emission consumes and omit the unused declaration payload.
func collectOverrideMethodsProjected(store graph.Store) (map[string]map[string]*graph.Node, bool) {
	projector, ok := store.(graph.MemberMethodsByType)
	if !ok {
		return nil, false
	}
	rowsByType := projector.MemberMethodsByType()
	byType := make(map[string]map[string]*graph.Node, len(rowsByType))
	for typeID, rows := range rowsByType {
		methods := make(map[string]*graph.Node)
		for _, row := range rows {
			if row.MethodID == "" {
				continue
			}
			methods[row.Name] = &graph.Node{
				ID: row.MethodID, Kind: graph.KindMethod, Name: row.Name,
				FilePath: row.FilePath, StartLine: row.StartLine, RepoPrefix: row.RepoPrefix,
			}
		}
		if len(methods) > 0 {
			byType[typeID] = methods
		}
	}
	return byType, true
}

func (r *Resolver) flushOverrideCandidates(candidates []overrideCandidate) int {
	if len(candidates) == 0 {
		return 0
	}
	fromSet := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		fromSet[candidate.from.ID] = struct{}{}
	}
	fromIDs := make([]string, 0, len(fromSet))
	for id := range fromSet {
		fromIDs = append(fromIDs, id)
	}
	sort.Strings(fromIDs)
	outByFrom := r.graph.GetOutEdgesByNodeIDs(fromIDs)
	existing := make(map[string]*graph.Edge, len(candidates))
	for _, id := range fromIDs {
		for _, edge := range outByFrom[id] {
			if edge == nil || edge.Kind != graph.EdgeOverrides {
				continue
			}
			key := inferencePairKey(edge.From, edge.To)
			if existing[key] == nil {
				existing[key] = edge
			}
		}
	}

	staged := make(map[string]*graph.Edge, len(candidates))
	edges := make([]*graph.Edge, 0, len(candidates))
	provenance := make(map[string]graph.EdgeProvenanceUpdate)
	added := 0
	for _, candidate := range candidates {
		key := inferencePairKey(candidate.from.ID, candidate.to.ID)
		if edge := existing[key]; edge != nil {
			if graph.OriginRank(edge.Origin) < graph.OriginRank(candidate.origin) {
				update := provenance[key]
				if update.Edge == nil || graph.OriginRank(update.NewOrigin) < graph.OriginRank(candidate.origin) {
					provenance[key] = graph.EdgeProvenanceUpdate{Edge: edge, NewOrigin: candidate.origin}
				}
			}
			continue
		}
		if edge := staged[key]; edge != nil {
			if graph.OriginRank(edge.Origin) < graph.OriginRank(candidate.origin) {
				edge.Origin = candidate.origin
			}
			continue
		}
		edge := &graph.Edge{
			From:            candidate.from.ID,
			To:              candidate.to.ID,
			Kind:            graph.EdgeOverrides,
			FilePath:        candidate.from.FilePath,
			Line:            candidate.from.StartLine,
			Confidence:      1.0,
			ConfidenceLabel: "EXTRACTED",
			Origin:          candidate.origin,
		}
		staged[key] = edge
		edges = append(edges, edge)
		added++
	}
	if len(edges) > 0 {
		r.graph.AddBatch(nil, edges)
	}
	if len(provenance) > 0 {
		keys := make([]string, 0, len(provenance))
		for key := range provenance {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		updates := make([]graph.EdgeProvenanceUpdate, 0, len(keys))
		for _, key := range keys {
			updates = append(updates, provenance[key])
		}
		r.graph.SetEdgeProvenanceBatch(updates)
	}
	return added
}

func (r *Resolver) inferOverrides(scope map[string]bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inferOverridesByRepo(scope)
}

func (r *Resolver) inferOverridesByRepo(scope map[string]bool) int {
	repos := overrideInferenceRepos(r.graph, scope)
	if len(repos) == 0 {
		return 0
	}
	// Unscoped inference already spans every repository (including ""), so a
	// parent's repo is always in the source list and the discovery walk over
	// the structural kinds would be a pure duplicate of the candidate walk
	// below. Only a scoped pass can name a parent repo outside its sources.
	methodRepos := repos
	if scope != nil {
		methodRepos = overrideMethodRepos(r.graph, repos, scope)
	}
	var methodsByType map[string]map[string]*graph.Node
	if scope == nil {
		if projected, ok := collectOverrideMethodsProjected(r.graph); ok {
			methodsByType = projected
		} else {
			methodsByType = collectOverrideMethods(r.graph, methodRepos)
		}
	} else {
		methodsByType = collectOverrideMethods(r.graph, methodRepos)
	}
	if len(methodsByType) == 0 {
		return 0
	}
	added := 0
	candidates := make([]overrideCandidate, 0, inferenceMutationBatchSize)
	flush := func() {
		added += r.flushOverrideCandidates(candidates)
		candidates = make([]overrideCandidate, 0, inferenceMutationBatchSize)
	}
	visitParent := func(fromID, toID, parentOrigin string) {
		if fromID == toID {
			return
		}
		childMethods := methodsByType[fromID]
		parentMethods := methodsByType[toID]
		if len(childMethods) == 0 || len(parentMethods) == 0 {
			return
		}
		names := make([]string, 0, len(childMethods))
		for name := range childMethods {
			names = append(names, name)
		}
		sort.Strings(names)
		origin := overrideOrigin(parentOrigin)
		for _, name := range names {
			child, parent := childMethods[name], parentMethods[name]
			if child == nil || parent == nil || parent.ID == child.ID {
				continue
			}
			candidates = append(candidates, overrideCandidate{from: child, to: parent, origin: origin})
			if len(candidates) == inferenceMutationBatchSize {
				flush()
			}
		}
	}
	if scope == nil {
		if projector, ok := r.graph.(graph.StructuralParentEdges); ok {
			for _, row := range projector.StructuralParentEdges() {
				if (row.FromKind != graph.KindType && row.FromKind != graph.KindInterface) ||
					(row.ToKind != graph.KindType && row.ToKind != graph.KindInterface) {
					continue
				}
				visitParent(row.FromID, row.ToID, row.Origin)
			}
			flush()
			return added
		}
	}
	for row := range graph.EdgesInScopeSeq(r.graph, repos, nil,
		graph.EdgeExtends, graph.EdgeImplements, graph.EdgeComposes) {
		if !relevantOverrideParent(row, scope) {
			continue
		}
		visitParent(row.Edge.From, row.Edge.To, row.Edge.Origin)
	}
	flush()
	return added
}

package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// grpcStubPrefix is the placeholder namespace the Go extractor emits
// for a gRPC client-stub call it can't land locally
// (`unresolved::grpc::<Service>::<Method>`).
const grpcStubPrefix = unresolvedPrefix + "grpc::"

// ResolveGRPCStubCalls is the graph-wide materialisation pass for the
// gRPC stub-call layer (M4). It lands every gRPC client-stub method
// call — emitted by the Go extractor as an EdgeCalls edge to the
// `unresolved::grpc::<Service>::<Method>` placeholder, carrying
// `Meta["via"]="grpc.stub"` plus `grpc_service` / `grpc_method` — on
// the server-side handler method that implements that RPC.
//
// Handler discovery uses two signals, in priority order:
//
//  1. Registration. The generated gRPC code's `Register<Service>Server`
//     helper is called by the server with the concrete implementation
//     as its second argument (`pb.RegisterUserServiceServer(s, &userServer{})`).
//     The Go extractor stamps `grpc_register_service` / `grpc_register_impl`
//     meta on that call edge; this pass joins the impl type's methods
//     by name. Most precise — independent of InferImplements and of the
//     forward-compat `Unimplemented<Service>Server` embedding pattern.
//     Resolved edges ride at ast_resolved.
//
//  2. Interface satisfaction. When no registration is found, the pass
//     falls back to the `<Service>Server` interface and the concrete
//     types that EdgeImplements it (materialised by InferImplements,
//     skipping the generated `Unimplemented*` stub type). Resolved
//     edges ride at ast_inferred.
//
// The pass is a full recompute and idempotent: every grpc.stub edge's
// target is recomputed from its own `grpc_service` / `grpc_method`
// meta, so it is incremental-safe — a reindex of either the client or
// the server file leaves the meta intact and the next pass re-lands
// (or un-lands) the edge. graph.ReindexEdge keeps the out/in buckets
// consistent. An edge whose handler is no longer in the graph is reset
// back to the placeholder and loses its resolution-tier metadata.
//
// Runs at every resolver settle point that already runs InferImplements
// (so signal 2 has its EdgeImplements edges) and before
// DetectCrossRepoEdges (so a cross-repo gRPC call gets its parallel
// cross_repo_calls edge).
//
// Returns the number of grpc.stub edges pointing at a resolved handler
// after the pass.
func ResolveGRPCStubCalls(g graph.Store) int {
	return resolveGRPCStubCalls(g, nil)
}

// resolveGRPCStubCalls is the census-multiplexed form: cands, when non-nil,
// carries the pass's pre-matched EdgeCalls candidates (stubs AND
// registration carriers) from the shared full-census walk, replacing both
// of this pass's own whole-stream decodes.
func resolveGRPCStubCalls(g graph.Store, cands *frameworkPassCandidates) int {
	if g == nil {
		return 0
	}

	callsStream := g.EdgesByKind(graph.EdgeCalls)
	var sharedCalls []*graph.Edge
	if cands != nil {
		sharedCalls = cands.calls
		callsStream = frameworkEdgeSeq(sharedCalls)
	}

	resolved := 0
	var reindexBatch []graph.EdgeReindex
	// First pass: collect every grpc.stub edge plus the From IDs we'll
	// need to read RepoPrefix off, so the per-edge GetNode below
	// collapses to a single GetNodesByIDs batch on disk backends.
	// Collected BEFORE the handler index is built: a workspace with no
	// grpc.stub edges (the common case) pays only this scan — the
	// whole-graph handler index (measured 58s on a 28-repo workspace,
	// yielding zero) is skipped, and with zero stubs the resolve loop
	// below is vacuously identical.
	type stubEdge struct {
		edge            *graph.Edge
		service, method string
	}
	var stubs []stubEdge
	fromIDs := make(map[string]struct{})
	for e := range callsStream {
		if e == nil || e.Meta == nil {
			continue
		}
		if v, _ := e.Meta["via"].(string); v != "grpc.stub" {
			continue
		}
		service, _ := e.Meta["grpc_service"].(string)
		method, _ := e.Meta["grpc_method"].(string)
		if service == "" || method == "" {
			continue
		}
		stubs = append(stubs, stubEdge{edge: e, service: service, method: method})
		if e.From != "" {
			fromIDs[e.From] = struct{}{}
		}
	}
	if len(stubs) == 0 {
		return 0
	}
	fromList := make([]string, 0, len(fromIDs))
	for id := range fromIDs {
		fromList = append(fromList, id)
	}
	callerNodes := g.GetNodesByIDs(fromList)
	idx := buildGRPCHandlerIndex(g, cands != nil, sharedCalls)

	for _, s := range stubs {
		e := s.edge
		callerRepo := ""
		if from := callerNodes[e.From]; from != nil {
			callerRepo = from.RepoPrefix
		}
		handlerID, origin, conf := idx.lookup(s.service, s.method, callerRepo)

		want := handlerID
		if want == "" {
			want = grpcStubPlaceholder(s.service, s.method)
		}
		if e.To == want {
			if handlerID != "" {
				resolved++
			}
			continue
		}

		oldTo := e.To
		e.To = want
		if handlerID != "" {
			e.Origin = origin
			e.Confidence = conf
			e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, conf)
			e.Meta["grpc_resolution"] = origin
			StampSynthesized(e, SynthGRPCStub)
			resolved++
		} else {
			// Re-orphaned (handler removed since the last pass): drop the
			// resolution-tier metadata so the edge reads as a plain
			// unresolved placeholder again.
			e.Origin = ""
			e.Confidence = 0
			e.ConfidenceLabel = ""
			delete(e.Meta, "grpc_resolution")
			UnstampSynthesized(e)
		}
		reindexBatch = append(reindexBatch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
	}
	if len(reindexBatch) > 0 {
		g.ReindexEdges(reindexBatch)
	}
	return resolved
}

// grpcStubPlaceholder is the canonical placeholder target for an
// unresolved gRPC stub call.
func grpcStubPlaceholder(service, method string) string {
	return grpcStubPrefix + service + "::" + method
}

// grpcHandlerIndex maps a gRPC service name to candidate handler method
// nodes, discovered via the registration and interface signals.
type grpcHandlerIndex struct {
	registration map[string][]*graph.Node // service → impl method nodes (ast_resolved)
	iface        map[string][]*graph.Node // service → impl method nodes (ast_inferred)
}

// lookup returns the handler node ID for (service, method), preferring
// the registration signal over the interface signal and a same-repo
// candidate over a cross-repo one. Returns ("", "", 0) when no unique
// handler is found.
func (idx *grpcHandlerIndex) lookup(service, method, callerRepo string) (id, origin string, confidence float64) {
	if n := pickGRPCHandler(idx.registration[service], method, callerRepo); n != nil {
		return n.ID, graph.OriginASTResolved, 0.9
	}
	if n := pickGRPCHandler(idx.iface[service], method, callerRepo); n != nil {
		return n.ID, graph.OriginASTInferred, 0.7
	}
	return "", "", 0
}

// buildGRPCHandlerIndex walks the graph once and indexes server-side
// gRPC handler methods by service, via both discovery signals. When shared
// is set, sharedCalls carries the registration-carrier candidates from the
// census dispatcher and the index's own EdgeCalls decode is skipped.
func buildGRPCHandlerIndex(g graph.Store, shared bool, sharedCalls []*graph.Edge) *grpcHandlerIndex {
	typesByName := map[string][]*graph.Node{}
	ifacesByName := map[string][]*graph.Node{}
	typeAndIfaceNodes := nodesByKindsOrAll(g, graph.KindType, graph.KindInterface)
	for _, n := range typeAndIfaceNodes {
		switch n.Kind {
		case graph.KindType:
			typesByName[n.Name] = append(typesByName[n.Name], n)
		case graph.KindInterface:
			ifacesByName[n.Name] = append(ifacesByName[n.Name], n)
		}
	}

	// methodsByType: type node ID → its method nodes (via EdgeMemberOf).
	// Use the MemberMethodsByType capability — projects only the four
	// columns we read (id/name/file/line) per row, no per-edge GetNode.
	rawMembers := memberMethodInfosByType(g)
	methodsByType := map[string][]*graph.Node{}
	for typeID, infos := range rawMembers {
		nodes := make([]*graph.Node, 0, len(infos))
		for _, m := range infos {
			nodes = append(nodes, &graph.Node{
				ID:         m.MethodID,
				Kind:       graph.KindMethod,
				Name:       m.Name,
				FilePath:   m.FilePath,
				StartLine:  m.StartLine,
				RepoPrefix: m.RepoPrefix,
			})
		}
		methodsByType[typeID] = nodes
	}

	// implementorsByIface: interface node ID → implementing type node
	// IDs. Pull only EdgeImplements; the From IDs are kept as-is for the
	// later impl filter (Unimplemented*).
	implementorsByIface := map[string][]string{}
	var registrations []*graph.Edge
	for e := range graph.EdgesLightSeq(g, graph.EdgeImplements) {
		if e == nil {
			continue
		}
		implementorsByIface[e.To] = append(implementorsByIface[e.To], e.From)
	}
	registrationStream := g.EdgesByKind(graph.EdgeCalls)
	if shared {
		registrationStream = frameworkEdgeSeq(sharedCalls)
	}
	for e := range registrationStream {
		if e == nil || e.Meta == nil {
			continue
		}
		if svc, _ := e.Meta["grpc_register_service"].(string); svc != "" {
			registrations = append(registrations, e)
		}
	}

	idx := &grpcHandlerIndex{
		registration: map[string][]*graph.Node{},
		iface:        map[string][]*graph.Node{},
	}

	// Prefetch the From nodes for every registration call so the
	// per-registration repo / dir lookup collapses to a single batch
	// GetNodesByIDs on disk backends.
	regFromIDs := make([]string, 0, len(registrations))
	for _, e := range registrations {
		if e.From != "" {
			regFromIDs = append(regFromIDs, e.From)
		}
	}
	regFromNodes := g.GetNodesByIDs(regFromIDs)

	// Signal 1: registration calls. Resolve the impl type named by the
	// registration's second argument, then index its methods.
	for _, e := range registrations {
		service, _ := e.Meta["grpc_register_service"].(string)
		implType, _ := e.Meta["grpc_register_impl"].(string)
		if service == "" || implType == "" {
			continue
		}
		regRepo, regDir := "", ""
		if from := regFromNodes[e.From]; from != nil {
			regRepo = from.RepoPrefix
			regDir = grpcParentDir(from.FilePath)
		}
		typeNode := pickGRPCType(typesByName[implType], regRepo, regDir)
		if typeNode == nil {
			continue
		}
		idx.registration[service] = append(idx.registration[service], methodsByType[typeNode.ID]...)
	}

	// Prefetch every implementor type referenced by a `<Service>Server`
	// interface so the per-implementor GetNode in Signal 2 collapses to
	// a batch.
	implTypeIDs := make(map[string]struct{})
	for name, ifaceNodes := range ifacesByName {
		const sfx = "Server"
		if len(name) <= len(sfx) || !strings.HasSuffix(name, sfx) {
			continue
		}
		for _, ifn := range ifaceNodes {
			for _, typeID := range implementorsByIface[ifn.ID] {
				if typeID != "" {
					implTypeIDs[typeID] = struct{}{}
				}
			}
		}
	}
	implTypeList := make([]string, 0, len(implTypeIDs))
	for id := range implTypeIDs {
		implTypeList = append(implTypeList, id)
	}
	implTypeNodes := g.GetNodesByIDs(implTypeList)

	// Signal 2: the `<Service>Server` interface and the concrete types
	// that implement it. The generated `Unimplemented<Service>Server`
	// stub also implements the interface — skip it so the fallback
	// lands on a real handler, not a "not implemented" stub.
	for name, ifaceNodes := range ifacesByName {
		const sfx = "Server"
		if len(name) <= len(sfx) || !strings.HasSuffix(name, sfx) {
			continue
		}
		service := name[:len(name)-len(sfx)]
		for _, ifn := range ifaceNodes {
			for _, typeID := range implementorsByIface[ifn.ID] {
				tn := implTypeNodes[typeID]
				if tn == nil || strings.HasPrefix(tn.Name, "Unimplemented") {
					continue
				}
				idx.iface[service] = append(idx.iface[service], methodsByType[typeID]...)
			}
		}
	}

	return idx
}

// pickGRPCType selects the impl type node for a registration call from
// same-name candidates: an exact same-directory match wins outright,
// then a unique same-repo match. Returns nil when ambiguous.
func pickGRPCType(candidates []*graph.Node, repo, dir string) *graph.Node {
	switch len(candidates) {
	case 0:
		return nil
	case 1:
		return candidates[0]
	}
	var sameRepo []*graph.Node
	for _, n := range candidates {
		if dir != "" && grpcParentDir(n.FilePath) == dir {
			return n
		}
		if repo != "" && n.RepoPrefix == repo {
			sameRepo = append(sameRepo, n)
		}
	}
	if len(sameRepo) == 1 {
		return sameRepo[0]
	}
	return nil
}

// pickGRPCHandler selects the handler method named `name` from a
// service's candidate methods, preferring a unique same-repo match,
// then a unique match overall. Returns nil when no candidate matches
// or the choice is ambiguous.
func pickGRPCHandler(methods []*graph.Node, name, callerRepo string) *graph.Node {
	var all, sameRepo []*graph.Node
	seen := map[string]bool{}
	for _, m := range methods {
		if m == nil || m.Name != name || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		all = append(all, m)
		if callerRepo != "" && m.RepoPrefix == callerRepo {
			sameRepo = append(sameRepo, m)
		}
	}
	if len(sameRepo) == 1 {
		return sameRepo[0]
	}
	if len(sameRepo) == 0 && len(all) == 1 {
		return all[0]
	}
	return nil
}

// grpcParentDir returns the slash-separated parent directory of a graph
// file path. Graph paths are slash-normalised, so a plain byte scan is
// correct on every OS.
func grpcParentDir(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return ""
}

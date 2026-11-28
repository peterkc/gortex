package indexer

import (
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

// extractDIContracts walks the graph for DI-tagged EdgeProvides and
// EdgeConsumes edges (emitted by the TypeScript extractor for @Module
// providers and @Inject consumers) and materialises them as Contract
// records in reg. The contract ID shape `di::<token>` is the same on
// both sides so the standard matcher reports orphans (tokens provided
// but not consumed, or consumed but never provided).
//
// This runs as a post-pass after per-file contract extraction because
// DI edges already live in the graph at that point — no source re-parse
// required. Safe to call repeatedly; AddAll de-duplicates by contract
// ID + symbol ID.
//
// In multi-repo mode the walk is scoped to this repo's nodes' out-edges
// instead of the global edge slice. The previous full walk produced
// per-repo work proportional to the entire shared graph, which became
// the dominant per-repo cost at large repo counts (and incorrectly
// re-attributed contracts from other repos to this one via
// AddAllScoped's RepoPrefix overwrite).
func (idx *Indexer) extractDIContracts(reg *contracts.Registry) {
	if reg == nil {
		return
	}
	// Spring @Bean linkage runs first and produces new EdgeCalls edges
	// that the later Contract-emission pass needs to consider. Ordering
	// also keeps bean extraction independent of the contract-side —
	// a repo that only uses Spring still gets usable bean links even
	// if no @Inject / useClass contracts exist anywhere.
	rows := graph.ReadRepoEdgesByKinds(idx.graph, []string{idx.repoPrefix}, []graph.EdgeKind{
		graph.EdgeProvides,
		graph.EdgeConsumes,
	})
	edges := make([]*graph.Edge, 0, len(rows))
	for _, row := range rows {
		if row.Edge != nil {
			edges = append(edges, row.Edge)
		}
	}
	idx.linkSpringBeansFromEdges(edges)

	var discovered []contracts.Contract
	for _, e := range edges {
		c, ok := diContractFromEdge(e)
		if !ok {
			continue
		}
		discovered = append(discovered, c)
	}
	if len(discovered) == 0 {
		return
	}
	reg.AddAllScoped(discovered, idx.repoPrefix, idx.workspaceID, idx.projectID)
}

type springBeanRef struct {
	methodID string
	typeName string
	filePath string
	line     int
}

func springBeanFromEdge(e *graph.Edge) (springBeanRef, bool) {
	if e.Kind != graph.EdgeProvides || e.Meta == nil {
		return springBeanRef{}, false
	}
	if b, _ := e.Meta[graph.MetaDIBinding].(string); b != graph.DIBindingBean {
		return springBeanRef{}, false
	}
	rt, _ := e.Meta[graph.MetaDIProvidesFor].(string)
	if rt == "" {
		return springBeanRef{}, false
	}
	return springBeanRef{methodID: e.To, typeName: rt, filePath: e.FilePath, line: e.Line}, true
}

func (idx *Indexer) linkSpringBeansFromEdges(repoEdges []*graph.Edge) {
	var beans []springBeanRef
	for _, e := range repoEdges {
		if bean, ok := springBeanFromEdge(e); ok {
			beans = append(beans, bean)
		}
	}
	if len(beans) == 0 {
		return
	}

	// Java method-node candidates considered for bean injection. Scoping
	// by repo here avoids a per-repo O(global_nodes) walk for every
	// bean — the dominant cost on workspaces that mix one Java repo
	// with hundreds of non-Java siblings.
	var candidates []*graph.Node
	for _, n := range idx.graph.GetRepoNodesByLanguage(idx.repoPrefix, "java") {
		if n.Kind == graph.KindMethod {
			candidates = append(candidates, n)
		}
	}

	// For each bean, walk Java constructors and @Bean factory methods
	// whose params_src (captured at extraction time) mentions the return
	// type. Dedupe by (consumer_class, bean_method) so an overloaded
	// constructor or factory method only links once.
	// Index candidates by identifier token once. The previous bean×method
	// nested scan repeated signature parsing for every bean; preserving the
	// candidate append order keeps emission deterministic while reducing the
	// work to signature bytes plus actual matches.
	candidatesByType := make(map[string][]*graph.Node)
	for _, n := range candidates {
		params, _ := n.Meta["params_src"].(string)
		for token := range javaIdentifierSet(params) {
			candidatesByType[token] = append(candidatesByType[token], n)
		}
	}
	linked := make(map[string]struct{})
	var links []*graph.Edge
	for _, b := range beans {
		for _, n := range candidatesByType[b.typeName] {
			if n.ID == b.methodID {
				continue
			}
			cls := enclosingClassID(n)
			if cls == "" || cls == b.methodID {
				continue
			}
			key := cls + "->" + b.methodID
			if _, dup := linked[key]; dup {
				continue
			}
			linked[key] = struct{}{}
			links = append(links, springBeanCall(cls, b))
		}
	}
	if len(links) > 0 {
		idx.graph.AddBatch(nil, links)
	}
}

func springBeanCall(cls string, bean springBeanRef) *graph.Edge {
	return &graph.Edge{
		From:     cls,
		To:       bean.methodID,
		Kind:     graph.EdgeCalls,
		FilePath: bean.filePath,
		Line:     bean.line,
		Meta: map[string]any{
			"via":     "spring.Bean",
			"bean_of": bean.typeName,
		},
	}
}

func javaIdentifierSet(src string) map[string]struct{} {
	out := make(map[string]struct{})
	for i := 0; i < len(src); {
		if !isJavaIdentChar(src[i]) || (src[i] >= '0' && src[i] <= '9') {
			i++
			continue
		}
		start := i
		i++
		for i < len(src) && isJavaIdentChar(src[i]) {
			i++
		}
		out[src[start:i]] = struct{}{}
	}
	return out
}

func isJavaIdentChar(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_' || b == '$'
}

// enclosingClassID derives the class-level node ID from a method node
// using its Meta["receiver"] (what the Java extractor stores). Returns
// "" if we can't derive one (free functions, static methods on pkg
// objects, etc. — not relevant for Spring anyway).
func enclosingClassID(n *graph.Node) string {
	recv, _ := n.Meta["receiver"].(string)
	if recv == "" {
		return ""
	}
	return n.FilePath + "::" + recv
}

// diContractFromEdge maps one EdgeProvides / EdgeConsumes edge to a
// Contract when its Meta identifies it as a DI binding. Returns
// (Contract, false) for non-DI edges (HTTP/gRPC contracts already use
// these same edge kinds, so we must not treat every Provides edge as
// a DI record).
func diContractFromEdge(e *graph.Edge) (contracts.Contract, bool) {
	var zero contracts.Contract
	if e == nil || e.Meta == nil {
		return zero, false
	}
	var token string
	var role contracts.Role
	var meta map[string]any

	switch e.Kind {
	case graph.EdgeProvides:
		// useClass and Spring's bean both name their abstract via
		// provides_for; the token forms use the token name itself.
		binding, _ := e.Meta[graph.MetaDIBinding].(string)
		switch binding {
		case graph.DIBindingUseClass, graph.DIBindingBean:
			if s, _ := e.Meta[graph.MetaDIProvidesFor].(string); s != "" {
				token = s
			}
		case graph.DIBindingUseValue, graph.DIBindingUseFactory, graph.DIBindingUseExisting:
			if s, _ := e.Meta[graph.MetaDIToken].(string); s != "" {
				token = s
			}
		default:
			return zero, false
		}
		role = contracts.RoleProvider
		meta = map[string]any{graph.MetaDIBinding: binding}
		if target := e.To; target != "" {
			// For useClass / bean, record the concrete target so the
			// orphan list in the contracts tool links straight to
			// either the concrete class (useClass) or the factory
			// method (bean). Token-form providers point at the token
			// directly, no extra info needed.
			if binding == graph.DIBindingUseClass || binding == graph.DIBindingBean {
				meta[binding] = target
			}
		}
	case graph.EdgeConsumes:
		if v, _ := e.Meta["via"].(string); v != "@Inject" {
			return zero, false
		}
		token, _ = e.Meta[graph.MetaDIToken].(string)
		role = contracts.RoleConsumer
		meta = map[string]any{"via": "@Inject"}
	default:
		return zero, false
	}

	if token == "" {
		return zero, false
	}
	return contracts.Contract{
		ID:       "di::" + token,
		Type:     contracts.ContractDI,
		Role:     role,
		SymbolID: e.From,
		FilePath: e.FilePath,
		Line:     e.Line,
		Meta:     meta,
		// Confidence mirrors the edge's originating extractor — these
		// are static `@Module` / `@Inject` decorators, high-confidence
		// by construction. Lower values would belong to future
		// inferred DI (e.g. if we ever infer bindings from tsconfig
		// paths).
		Confidence: 0.9,
	}, true
}

package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// myBatisStubPrefix is the placeholder namespace the MyBatis mapper-XML
// extractor emits for the call edge that runs from a SQL statement node
// back to the Java DAO method that executes it
// (`unresolved::mybatis::<namespace>::<id>`).
const myBatisStubPrefix = unresolvedPrefix + "mybatis::"

// ResolveMyBatisCalls is the graph-wide materialisation pass for the
// MyBatis mapper layer. The mapper-XML extractor emits one node per
// `<select>/<insert>/<update>/<delete>` statement (ID `<namespace>::<id>`)
// plus an EdgeCalls edge from that statement node to a
// `unresolved::mybatis::<namespace>::<id>` placeholder, carrying
// `Meta["via"]="mybatis.mapper"` plus `mybatis_namespace` /
// `mybatis_statement`. This pass joins each such edge onto the Java DAO /
// Mapper interface method the statement implements.
//
// The `<mapper namespace>` is the Java interface FQCN
// (`com.app.UserMapper`); the statement `id` is the method name
// (`findUser`). Java method keys come from the simple name in
// `Meta["receiver"]` and the node's `Name` when both are present. Otherwise,
// the join uses the trailing `::`-delimited component of the node ID if it
// contains a dot. It prefers a same-repo candidate and falls back to a
// unique workspace-wide match.
//
// The pass is a full recompute and idempotent: every mybatis.mapper
// edge's target is recomputed from its own `mybatis_namespace` /
// `mybatis_statement` meta on each call, so a reindex of either the mapper
// XML or the Java interface leaves the meta intact and the next pass
// re-lands (or un-lands) the edge. graph.ReindexEdges keeps the out/in
// buckets consistent. An edge whose method is no longer in the graph is
// reset back to the placeholder and loses its resolution-tier metadata.
//
// Resolved edges are stamped with provenance Meta
// (`synthesized_by="mybatis"`, `provenance="heuristic"`) so downstream
// consumers can tell a framework-synthesized binding from a directly
// extracted call.
//
// Returns the number of mybatis.mapper edges pointing at a resolved Java
// method after the pass.
func ResolveMyBatisCalls(g graph.Store) int {
	if g == nil {
		return 0
	}

	idx := buildMyBatisMethodIndex(g)
	resolved := 0
	var reindexBatch []graph.EdgeReindex

	// First sweep: collect every mybatis.mapper edge plus the From IDs we
	// need to read RepoPrefix off, so the per-edge GetNode below collapses
	// to a single GetNodesByIDs batch on disk backends.
	type stubEdge struct {
		edge                 *graph.Edge
		namespace, statement string
	}
	var stubs []stubEdge
	fromIDs := map[string]struct{}{}
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		ns, stmt := myBatisStub(e)
		if ns == "" || stmt == "" {
			continue
		}
		stubs = append(stubs, stubEdge{edge: e, namespace: ns, statement: stmt})
		if e.From != "" {
			fromIDs[e.From] = struct{}{}
		}
	}
	fromList := make([]string, 0, len(fromIDs))
	for id := range fromIDs {
		fromList = append(fromList, id)
	}
	fromNodes := g.GetNodesByIDs(fromList)

	for _, s := range stubs {
		e := s.edge
		callerRepo := ""
		if from := fromNodes[e.From]; from != nil {
			callerRepo = from.RepoPrefix
		}
		methodID := idx.lookup(s.namespace, s.statement, callerRepo)
		if methodID != "" {
			resolved++
		}
		oldTo := e.To
		if rebindMyBatisCall(e, s.namespace, s.statement, methodID) {
			reindexBatch = append(reindexBatch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
		}
	}
	if len(reindexBatch) > 0 {
		g.ReindexEdges(reindexBatch)
	}
	return resolved
}

func myBatisStub(e *graph.Edge) (namespace, statement string) {
	if e != nil {
		if via, _ := e.Meta["via"].(string); via == "mybatis.mapper" {
			namespace, _ = e.Meta["mybatis_namespace"].(string)
			statement, _ = e.Meta["mybatis_statement"].(string)
		}
	}
	return namespace, statement
}

// rebindMyBatisCall returns true only when the target changes and needs reindexing.
// Moving rows share the same resolution and re-orphaning stamps on both write paths.
func rebindMyBatisCall(e *graph.Edge, namespace, statement, method string) bool {
	want := method
	if want == "" {
		want = myBatisStubPlaceholder(namespace, statement)
	}
	if e.To == want {
		return false
	}
	e.To = want
	if method != "" {
		const confidence = 0.7
		e.Origin = graph.OriginASTInferred
		e.Confidence = confidence
		e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, confidence)
		e.Meta["synthesized_by"] = "mybatis"
		e.Meta["provenance"] = "heuristic"
		e.Meta["mybatis_resolution"] = e.Origin
	} else {
		// A removed or ambiguous method leaves a plain placeholder again.
		e.Origin = ""
		e.Confidence = 0
		e.ConfidenceLabel = ""
		delete(e.Meta, "synthesized_by")
		delete(e.Meta, "provenance")
		delete(e.Meta, "mybatis_resolution")
	}
	return true
}

// myBatisStubPlaceholder is the canonical placeholder target for an
// unresolved MyBatis mapper-statement call.
func myBatisStubPlaceholder(namespace, statement string) string {
	return myBatisStubPrefix + namespace + "::" + statement
}

// myBatisMethodIndex maps a "<SimpleClassName>.<method>" key to the Java
// method nodes that match it.
type myBatisMethodIndex struct {
	byKey map[string][]*graph.Node
}

// lookup returns the Java DAO method node for (namespace, statement),
// preferring a same-repo candidate over a cross-repo one. namespace is
// the interface FQCN (`com.app.UserMapper`); its simple class name plus
// the statement id form the join key. Returns "" when no unique
// method is found.
func (idx *myBatisMethodIndex) lookup(namespace, statement, callerRepo string) string {
	key := myBatisJoinKey(namespace, statement)
	if key == "" {
		return ""
	}
	cands := idx.byKey[key]
	if len(cands) == 0 {
		return ""
	}
	var sameRepo []*graph.Node
	for _, n := range cands {
		if callerRepo != "" && n.RepoPrefix == callerRepo {
			sameRepo = append(sameRepo, n)
		}
	}
	if len(sameRepo) == 1 {
		return sameRepo[0].ID
	}
	if len(sameRepo) == 0 && len(cands) == 1 {
		return cands[0].ID
	}
	return ""
}

// buildMyBatisMethodIndex walks every Java method node once and indexes
// it by the key returned by javaMethodJoinKey. One pass replaces an
// AllNodes scan per edge.
func buildMyBatisMethodIndex(g graph.Store) *myBatisMethodIndex {
	idx := &myBatisMethodIndex{byKey: map[string][]*graph.Node{}}
	for n := range g.NodesByKind(graph.KindMethod) {
		if n == nil || n.Language != "java" {
			continue
		}
		key := javaMethodJoinKey(n)
		if key == "" {
			continue
		}
		idx.byKey[key] = append(idx.byKey[key], n)
	}
	return idx
}

// myBatisJoinKey builds the "<SimpleClassName>.<statement>" join key from
// a mapper namespace (interface FQCN) and a statement id. The simple
// class name is the last dotted component of the FQCN
// (`com.app.UserMapper` → `UserMapper`).
func myBatisJoinKey(namespace, statement string) string {
	if namespace == "" || statement == "" {
		return ""
	}
	simple := namespace
	if i := strings.LastIndex(simple, "."); i >= 0 {
		simple = simple[i+1:]
	}
	if simple == "" {
		return ""
	}
	return simple + "." + statement
}

// javaMethodJoinKey returns the "<SimpleClassName>.<method>" key for a
// Java method node. It uses the simple name from `Meta["receiver"]` and
// the node's `Name` when both are present. Otherwise it uses the last
// `::`-delimited component of the node ID if that component contains a dot.
func javaMethodJoinKey(n *graph.Node) string {
	if n == nil {
		return ""
	}
	tail := n.ID
	if i := strings.LastIndex(tail, "::"); i >= 0 {
		tail = tail[i+2:]
	}
	// The tail is normally "Class.method"; honour a one-off
	// line-disambiguated suffix the extractor appends on collisions
	// ("Class.method_L42") by reconstructing from receiver + Name.
	if recv, _ := n.Meta["receiver"].(string); recv != "" && n.Name != "" {
		simple := recv
		if i := strings.LastIndex(simple, "."); i >= 0 {
			simple = simple[i+1:]
		}
		return simple + "." + n.Name
	}
	if tail == "" || !strings.Contains(tail, ".") {
		return ""
	}
	return tail
}

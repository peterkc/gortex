package resolver

import "github.com/zzet/gortex/internal/graph"

// sqlCallsiteVia marks the placeholder call edge a JS/TS/Python SQL
// function call site emits (mirrors languages.sqlCallsiteVia).
const sqlCallsiteVia = "sql.callsite"

// sqlCallPrefix is the placeholder namespace for an unresolved SQL
// function call (`unresolved::sqlfn::<name>`).
const sqlCallPrefix = unresolvedPrefix + "sqlfn::"

// ResolveSQLCallsites is the framework-dispatch synthesizer that links a
// cross-language SQL function call site (Supabase/PostgREST `.rpc('fn')`,
// SQLAlchemy `func.fn()`) to the SQL `CREATE FUNCTION` node it invokes.
// The language extractors emit a placeholder EdgeCalls to
// `unresolved::sqlfn::<name>` carrying Meta["via"]="sql.callsite"; this
// pass lands it on the unique SQL KindFunction node of that name.
//
// Full recompute and idempotent: the target is recomputed from the edge's
// sql_function meta, so a reindex of either side re-lands or re-orphans
// the edge. graph.ReindexEdges keeps the adjacency buckets consistent.
// An ambiguous name (two SQL functions) is left on the placeholder.
//
// Returns the number of call edges landed on a SQL function.
func ResolveSQLCallsites(g graph.Store) int {
	if g == nil {
		return 0
	}
	sqlFns := map[string][]*graph.Node{}
	for _, n := range nodesByKindsOrAll(g, graph.KindFunction) {
		if n != nil && n.Language == "sql" && n.Name != "" {
			sqlFns[n.Name] = append(sqlFns[n.Name], n)
		}
	}
	if len(sqlFns) == 0 {
		return 0
	}

	resolved := 0
	var reindex []graph.EdgeReindex
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		fn := sqlCallsiteFunction(e)
		if fn == "" {
			continue
		}
		cands := sqlFns[fn]
		matched := len(cands) == 1
		target := ""
		if matched {
			target = cands[0].ID
			resolved++
		}
		oldTo := e.To
		if rebindSQLCallsite(e, fn, target, matched) {
			reindex = append(reindex, graph.EdgeReindex{Edge: e, OldTo: oldTo})
		}
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	return resolved
}

func sqlCallsiteFunction(e *graph.Edge) string {
	if e != nil {
		if via, _ := e.Meta["via"].(string); via == sqlCallsiteVia {
			fn, _ := e.Meta["sql_function"].(string)
			return fn
		}
	}
	return ""
}

// The caller elects from the live function census. Do not restamp a
// stationary Call, even if its saved payload differs from a new move.
func rebindSQLCallsite(e *graph.Edge, fn, target string, matched bool) bool {
	want := target
	if !matched {
		want = sqlCallPrefix + fn
	}
	if e.To == want {
		return false
	}
	e.To = want
	e.Origin = graph.OriginASTInferred
	if matched {
		e.Confidence = 0.6
		e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, 0.6)
		StampSynthesized(e, SynthSQLCallsite)
	} else {
		// Re-orphaned (the SQL function disappeared / became ambiguous).
		e.Confidence = 0
		e.ConfidenceLabel = ""
		UnstampSynthesized(e)
	}
	return true
}

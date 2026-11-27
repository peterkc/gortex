package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// sidekiqVia is the Meta["via"] tag the Ruby extractor stamps on a Sidekiq
// dispatch placeholder — a `Worker.perform_async(...)` the static graph
// cannot connect to the worker's `perform` because the job runs async.
const sidekiqVia = "sidekiq-dispatch"

// ResolveSidekiqCalls binds Sidekiq job dispatches to the worker's perform
// method: `EmailJob.perform_async(id)` → `EmailJob#perform`. Matching is
// namespace-aware — a `Workers::EmailJob.perform_async` reaches the
// `EmailJob` worker by its simple constant name. The include gate makes
// this precise, so edges land at the typed framework tier.
//
// Returns the number of placeholders landed on a worker perform.
func ResolveSidekiqCalls(g graph.Store) int {
	if g == nil {
		return 0
	}
	byFull := map[string][]*graph.Node{}
	bySimple := map[string][]*graph.Node{}
	for _, n := range nodesByKindsOrAll(g, graph.KindMethod, graph.KindFunction) {
		w := sidekiqWorkerName(n)
		if w == "" {
			continue
		}
		byFull[w] = append(byFull[w], n)
		bySimple[sidekiqSimpleName(w)] = append(bySimple[sidekiqSimpleName(w)], n)
	}
	if len(byFull) == 0 {
		return 0
	}

	resolved := 0
	var reindex []graph.EdgeReindex
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		worker := sidekiqCallName(e)
		if worker == "" {
			continue
		}
		cands := byFull[worker]
		if len(cands) == 0 {
			cands = bySimple[sidekiqSimpleName(worker)]
		}
		target := pickStoreAction(g, e, sameBoundaryCandidates(g, e.From, cands))

		var id string
		if target != nil {
			id = target.ID
			resolved++
		}
		oldTo := e.To
		if rebindSidekiqCall(e, id, target != nil) {
			reindex = append(reindex, graph.EdgeReindex{Edge: e, OldTo: oldTo})
		}
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	return resolved
}

func sidekiqWorkerName(n *graph.Node) string {
	if n == nil {
		return ""
	}
	worker, _ := n.Meta["sidekiq_worker"].(string)
	return worker
}

func sidekiqCallName(e *graph.Edge) string {
	if e == nil {
		return ""
	}
	if via, _ := e.Meta["via"].(string); via != sidekiqVia {
		return ""
	}
	worker, _ := e.Meta["sidekiq_worker"].(string)
	return worker
}

func rebindSidekiqCall(e *graph.Edge, target string, matched bool) bool {
	if !matched {
		target = "unresolved::*.perform"
	}
	if e.To == target {
		return false
	}
	e.To = target
	e.Origin = graph.OriginASTInferred
	if matched {
		e.Confidence = ConfidenceTyped
		e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, ConfidenceTyped)
		StampSynthesizedTyped(e, SynthSidekiq)
	} else {
		e.Confidence = 0
		e.ConfidenceLabel = ""
		UnstampSynthesized(e)
	}
	return true
}

// sidekiqSimpleName returns the last `::` segment of a Ruby constant path.
func sidekiqSimpleName(s string) string {
	if i := strings.LastIndex(s, "::"); i >= 0 {
		return s[i+2:]
	}
	return s
}

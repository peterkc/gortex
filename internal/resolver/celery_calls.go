package resolver

import (
	"github.com/zzet/gortex/internal/graph"
)

// celeryVia is the Meta["via"] tag the Python extractor stamps on a Celery
// dispatch placeholder — a `task.delay(...)` / `.apply_async(...)` / `.s()`
// or a `send_task("name")` the static graph cannot resolve because the task
// runs out of process.
const celeryVia = "celery-dispatch"

// celeryFanoutCap bounds the candidate set a single task name may resolve
// against before the placeholder is left unbound, matching the framework's
// precision-first posture.
const celeryFanoutCap = 80

// ResolveCeleryCalls binds Celery task dispatches to the decorated task
// function: `send_email.delay(...)` → `send_email`, and
// `send_task("emails.send")` → the `@task(name="emails.send")` function.
// The decorator gate makes this precise, so edges land at the typed
// framework tier (ConfidenceTyped / ProvenanceFramework).
//
// Returns the number of placeholders landed on a real task.
func ResolveCeleryCalls(g graph.Store) int {
	if g == nil {
		return 0
	}
	byName := map[string][]*graph.Node{}
	byRegistered := map[string][]*graph.Node{}
	for _, n := range nodesByKindsOrAll(g, graph.KindFunction, graph.KindMethod) {
		task, registered := celeryTaskNode(n)
		if task != "" {
			byName[task] = append(byName[task], n)
		}
		if registered != "" {
			byRegistered[registered] = append(byRegistered[registered], n)
		}
	}
	if len(byName) == 0 {
		return 0
	}

	resolved := 0
	var reindex []graph.EdgeReindex
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		task, registered := celeryTaskCall(e)
		if task == "" {
			continue
		}
		cands := byName[task]
		if registered != "" {
			cands = byRegistered[registered]
		}
		var target *graph.Node
		if len(cands) <= celeryFanoutCap {
			target = pickStoreAction(g, e, sameBoundaryCandidates(g, e.From, cands))
		}
		oldTo := e.To
		var id string
		if target != nil {
			id = target.ID
			resolved++
		}
		if rebindCeleryCall(e, task, id, target != nil) {
			reindex = append(reindex, graph.EdgeReindex{Edge: e, OldTo: oldTo})
		}
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	return resolved
}

func celeryTaskNode(n *graph.Node) (task, registered string) {
	if n == nil {
		return "", ""
	}
	task, _ = n.Meta["celery_task"].(string)
	registered, _ = n.Meta["celery_registered_name"].(string)
	return task, registered
}

func celeryTaskCall(e *graph.Edge) (task, registered string) {
	if e == nil {
		return "", ""
	}
	if via, _ := e.Meta["via"].(string); via != celeryVia {
		return "", ""
	}
	task, _ = e.Meta["celery_task"].(string)
	registered, _ = e.Meta["celery_registered_name"].(string)
	return task, registered
}

// rebindCeleryCall returns true only when the target changes and needs reindexing.
func rebindCeleryCall(e *graph.Edge, task, target string, matched bool) bool {
	if !matched {
		target = "unresolved::*." + task
	}
	if e.To == target {
		return false
	}
	e.To = target
	e.Origin = graph.OriginASTInferred
	if matched {
		e.Confidence = ConfidenceTyped
		e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, ConfidenceTyped)
		StampSynthesizedTyped(e, SynthCelery)
	} else {
		e.Confidence = 0
		e.ConfidenceLabel = ""
		UnstampSynthesized(e)
	}
	return true
}

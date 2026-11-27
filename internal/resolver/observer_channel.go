package resolver

import (
	"regexp"
	"slices"
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// observerChannelVia marks a synthesized field-backed observer-channel call
// edge.
const observerChannelVia = "observer.channel"

// maxObserverCallbacksPerChannel caps how many callbacks one channel field may
// fan a dispatcher out to. A field registered with dozens of callbacks is
// almost always a generic bus where a dispatcher→callback edge per pair is
// noise; real domain observers wire a handful.
const maxObserverCallbacksPerChannel = 40

var (
	// observerRegistrarRe matches a method that registers an observer into a
	// channel field: onFoo / subscribe / addListener / register / watch / …
	observerRegistrarRe = regexp.MustCompile(`(?i)^(on[a-z]\w*|subscribe|addlistener|addeventlistener|register|watch|listen|addcallback)$`)
	// observerDispatcherRe matches a method that fires a channel field:
	// emit / trigger / notify / dispatch / fire / publish / flush.
	observerDispatcherRe = regexp.MustCompile(`(?i)(emit|trigger|notify|dispatch|fire|publish|flush)`)
)

// observerChannel pairs the registrar and dispatcher methods that share one
// field-backed channel.
type observerChannel struct {
	registrars  []*graph.Node
	dispatchers []*graph.Node
}

// ResolveObserverChannelCalls is the framework-dispatch synthesizer for
// field-backed observer channels — the dynamic-dispatch hole where a method
// stores callbacks in a field and another method invokes them. A field is a
// channel when a registrar-named method (onFoo / subscribe / register / …) and
// a dispatcher-named method (emit / trigger / notify / …) both access it. For
// each such channel the pass finds the functions registered at the registrar's
// call sites — a function referenced on the same line as the registrar call —
// and synthesizes a calls edge from every dispatcher to every registered
// callback, the runtime dispatch the static call graph cannot see.
//
// High-precision, low-recall by design: named registrar/dispatcher methods
// paired by a shared field, callbacks taken from the registrar's call sites,
// and the fan-out capped. Full recompute and idempotent: edges are re-derived
// from the field-access and call metadata, graph.AddEdge dedupes by key, and
// graph.EvictFile drops the edge when either endpoint's file is reindexed.
// Edges ride at ast_inferred and carry synthesizer provenance.
//
// Returns the number of observer-channel call edges synthesized.
func ResolveObserverChannelCalls(g graph.Store) int {
	if g == nil {
		return 0
	}

	channels := map[string]*observerChannel{}
	for e := range g.EdgesByKind(graph.EdgeAccessesField) {
		if e == nil || e.To == "" || e.From == "" {
			continue
		}
		m := g.GetNode(e.From)
		if !graph.ObserverCallable(m) {
			continue
		}
		isReg := observerRegistrarRe.MatchString(m.Name)
		isDisp := observerDispatcherRe.MatchString(m.Name)
		if !isReg && !isDisp {
			continue
		}
		c := channels[e.To]
		if c == nil {
			c = &observerChannel{}
			channels[e.To] = c
		}
		if isReg {
			c.registrars = append(c.registrars, m)
		}
		if isDisp {
			c.dispatchers = append(c.dispatchers, m)
		}
	}
	if len(channels) == 0 {
		return 0
	}

	// Stable iteration so a re-run produces edges in the same order.
	fieldIDs := make([]string, 0, len(channels))
	for id := range channels {
		fieldIDs = append(fieldIDs, id)
	}
	sort.Strings(fieldIDs)

	var batch []*graph.Edge
	synthesized := 0
	for _, field := range fieldIDs {
		c := channels[field]
		if len(c.registrars) == 0 || len(c.dispatchers) == 0 {
			continue
		}
		callbacks := observerCallbacks(g, c.registrars)
		if !graph.ObserverCallbackCountAllowed(len(callbacks), maxObserverCallbacksPerChannel) {
			continue
		}
		for _, disp := range c.dispatchers {
			for _, cb := range callbacks {
				if !graph.ObserverCallbackIsDistinct(disp.ID, cb) {
					continue
				}
				if !sameDispatchBoundary(disp, g.GetNode(cb)) {
					continue
				}
				batch = append(batch, observerChannelEdge(disp, cb, field))
				synthesized++
			}
		}
	}

	for _, e := range batch {
		g.AddEdge(e)
	}
	return synthesized
}

// observerCallbacks collects the functions registered through a channel's
// registrar methods: a function/method referenced on the same file+line as a
// call to the registrar (the callback argument). Returns a sorted, de-duped ID
// list for deterministic edge synthesis.
func observerCallbacks(g graph.Store, registrars []*graph.Node) []string {
	seen := map[string]bool{}
	kinds := graph.ObserverCallbackKinds()
	for _, reg := range registrars {
		for _, in := range g.GetInEdges(reg.ID) {
			if in == nil || in.Kind != graph.EdgeCalls || in.From == "" {
				continue
			}
			for _, ref := range g.GetOutEdges(in.From) {
				if !graph.ObserverCallbackAtSite(in, ref) || !slices.Contains(kinds, ref.Kind) {
					continue
				}
				if !graph.ObserverCallable(g.GetNode(ref.To)) {
					continue
				}
				seen[ref.To] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// observerChannelEdge builds one dispatcher→callback synthesized call edge.
func observerChannelEdge(from *graph.Node, toID, field string) *graph.Edge {
	return &graph.Edge{
		From:            from.ID,
		To:              toID,
		Kind:            graph.EdgeCalls,
		FilePath:        from.FilePath,
		Line:            from.StartLine,
		Confidence:      0.5,
		ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeCalls, 0.5),
		Origin:          graph.OriginASTInferred,
		Meta: map[string]any{
			"via":             observerChannelVia,
			"channel_field":   field,
			MetaSynthesizedBy: SynthObserverChannel,
			MetaProvenance:    ProvenanceHeuristic,
		},
	}
}

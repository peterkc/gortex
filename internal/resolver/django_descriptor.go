package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// DjangoDescriptorResolver is a claiming resolver for Django's named
// descriptor dispatch — an attribute reference the static graph cannot
// resolve because it names a runtime descriptor, not a declared method.
// The flagship case is `self._iterable_class(self)` inside a QuerySet:
// `_iterable_class` is a class attribute (default `ModelIterable`), and
// iterating its instance runs `ModelIterable.__iter__`. This resolver
// claims those residual `_iterable_class` references and binds them to the
// iterable class's `__iter__`, keyed by class names present in the graph.
type DjangoDescriptorResolver struct{}

// djangoDescriptorVocab is the set of Django descriptor attribute names this
// resolver claims. Kept tight so the pre-filter only sees its own framework
// vocabulary.
var djangoDescriptorVocab = map[string]bool{
	"_iterable_class": true,
}

// djangoDefaultIterableClass is Django's default QuerySet._iterable_class.
const djangoDefaultIterableClass = "ModelIterable"

func (DjangoDescriptorResolver) Name() string { return SynthDjangoDescriptor }

// Claims reports whether the edge references a Django descriptor name.
func (DjangoDescriptorResolver) Claims(e *graph.Edge) bool {
	if e == nil {
		return false
	}
	return djangoDescriptorVocab[djangoRefName(e.To)]
}

// Resolve rebinds a claimed `_iterable_class` reference to the iterable
// class's `__iter__` method — the class named by the QuerySet's
// django_iterable_class hint, else Django's default ModelIterable.
func (DjangoDescriptorResolver) Resolve(g graph.Store, e *graph.Edge) bool {
	return DjangoDescriptorResolver{}.ResolveBatch(g, []*graph.Edge{e})[e]
}

// ResolveBatch resolves every claimed descriptor edge from one compact source
// read and one name-index query, then persists all target rewrites together.
// It preserves Resolve's first-match semantics for receiver classes and
// __iter__ methods while avoiding three point/name queries per edge.
func (DjangoDescriptorResolver) ResolveBatch(g graph.Store, edges []*graph.Edge) map[*graph.Edge]bool {
	resolved := make(map[*graph.Edge]bool)
	if g == nil || len(edges) == 0 {
		return resolved
	}
	claimed := make([]*graph.Edge, 0, len(edges))
	sourceIDs := make([]string, 0, len(edges))
	for _, edge := range edges {
		if edge == nil || djangoRefName(edge.To) != "_iterable_class" {
			continue
		}
		claimed = append(claimed, edge)
		sourceIDs = append(sourceIDs, edge.From)
	}
	if len(claimed) == 0 {
		return resolved
	}
	sources := g.GetNodesByIDs(sourceIDs)
	names := []string{"__iter__"}
	seenNames := map[string]bool{"__iter__": true}
	for _, edge := range claimed {
		source := sources[edge.From]
		if source == nil || source.Meta == nil {
			continue
		}
		receiver, _ := source.Meta["receiver"].(string)
		if receiver != "" && !seenNames[receiver] {
			seenNames[receiver] = true
			names = append(names, receiver)
		}
	}
	byName := g.FindNodesByNames(names)
	iterMethods := make(map[string]*graph.Node)
	for _, node := range byName["__iter__"] {
		if node == nil || node.Kind != graph.KindMethod || node.Meta == nil {
			continue
		}
		receiver, _ := node.Meta["receiver"].(string)
		if receiver != "" && iterMethods[receiver] == nil {
			iterMethods[receiver] = node
		}
	}

	reindex := make([]graph.EdgeReindex, 0, len(claimed))
	for _, edge := range claimed {
		iterableClass := ""
		if source := sources[edge.From]; source != nil && source.Meta != nil {
			receiver, _ := source.Meta["receiver"].(string)
			for _, class := range byName[receiver] {
				if class == nil || class.Kind != graph.KindType || class.Meta == nil {
					continue
				}
				if hint, _ := class.Meta["django_iterable_class"].(string); hint != "" {
					iterableClass = hint
					break
				}
			}
		}
		if iterableClass == "" {
			iterableClass = djangoDefaultIterableClass
		}
		target := iterMethods[iterableClass]
		if target == nil {
			continue
		}
		reindex = append(reindex, rebindDjangoDescriptor(edge, target.ID))
		resolved[edge] = true
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	return resolved
}

func rebindDjangoDescriptor(edge *graph.Edge, target string) graph.EdgeReindex {
	oldTo := edge.To
	edge.To = target
	edge.Origin = graph.OriginASTInferred
	edge.Confidence = 0.7
	edge.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, 0.7)
	StampSynthesized(edge, SynthDjangoDescriptor)
	return graph.EdgeReindex{Edge: edge, OldTo: oldTo}
}

// djangoRefName extracts the bare attribute name from an unresolved target
// id, stripping the `unresolved::` prefix and any `*.` method marker.
func djangoRefName(to string) string {
	if !graph.IsUnresolvedTarget(to) {
		return ""
	}
	return strings.TrimPrefix(graph.UnresolvedName(to), "*.")
}

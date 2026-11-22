package resolver

import (
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func rebuildFunctionCandidateEligible(e *graph.Edge, n *graph.Node) bool {
	one := [1]*graph.Node{n}
	return len(genericInstantiationOnly(e, withoutAttributedTerminals(one[:]))) != 0 && !graph.IsReExportNode(n)
}

// rebuildCppOverload consumes the eligible rows of the bare call's initial
// immutable name page. It keeps no candidate list or decoded node.
type rebuildCppOverload struct {
	argHints, normArgs    []string
	bestRanks, ranks      []int
	arity, viable, bestAt int
	arityHit, best        rebuildFunctionPick
}

func (s *rebuildCppOverload) consider(n *graph.Node) {
	sig, ok := rebuildCppArityMatch(n, len(s.argHints))
	if !ok {
		return
	}
	s.arity++
	pick := rebuildFunctionPick{n.ID, n.Kind}
	if s.arity == 1 {
		s.arityHit = pick
	}
	if len(s.argHints) == 0 {
		return
	}
	if s.normArgs == nil {
		s.normArgs = make([]string, len(s.argHints))
		for i, hint := range s.argHints {
			s.normArgs[i] = graph.NormalizeCppType(hint)
		}
		s.bestRanks = make([]int, len(s.argHints))
		s.ranks = make([]int, len(s.argHints))
	}
	if !rebuildCppRanks(s.ranks, s.normArgs, sig) {
		return
	}
	s.viable++
	if s.viable == 1 || cppDominates(s.ranks, s.bestRanks) {
		s.best, s.bestAt = pick, s.arity
		s.bestRanks, s.ranks = s.ranks, s.bestRanks
	}
}

// A unique nondominated vector dominates every other vector, so it cannot
// lose the first scan. Certify it against every other viable row; compare by
// arity ordinal to skip only the winning occurrence, not other equal vectors.
func (s *rebuildCppOverload) pick(ctx context.Context, f *rebuildLinkFrontier, req store_pebble.LinkRequest, e *graph.Edge) (rebuildFunctionPick, error) {
	switch {
	case s.arity == 1:
		return s.arityHit, nil // native takes the sole arity match before conversions
	case s.viable == 0:
		return rebuildFunctionPick{}, nil // includes multiple matches without hints
	case s.viable == 1:
		return s.best, nil
	}
	if req.Kind != store_pebble.LinkName || req.AllRepos {
		return rebuildFunctionPick{}, fmt.Errorf("%w: C++ ranked overload requires the original immutable scoped name page", errRebuildMainRule)
	}
	arity := 0
	dominant := true
	_, err := f.candidates(ctx, []store_pebble.LinkRequest{req}, func(_ int, n *graph.Node) error {
		if !dominant || !rebuildFunctionCandidateEligible(e, n) {
			return nil // even a decided miss must not hide a page error
		}
		sig, ok := rebuildCppArityMatch(n, len(s.normArgs))
		if !ok {
			return nil
		}
		arity++
		if arity == s.bestAt {
			return nil
		}
		if rebuildCppRanks(s.ranks, s.normArgs, sig) && !cppDominates(s.bestRanks, s.ranks) {
			dominant = false
		}
		return nil
	})
	if err != nil || !dominant {
		return rebuildFunctionPick{}, err
	}
	return s.best, nil
}

func rebuildCppArityMatch(n *graph.Node, argc int) (cppCandSig, bool) {
	if n == nil || (n.Kind != graph.KindFunction && n.Kind != graph.KindMethod) {
		return cppCandSig{}, false
	}
	sig, ok := parseCppCandidate(n)
	return sig, ok && cppArityCompatible(sig, argc)
}

func rebuildCppRanks(ranks []int, normArgs []string, sig cppCandSig) bool {
	for i, arg := range normArgs {
		if arg == "" {
			ranks[i] = 0 // native treats unknown positions as neutral
			continue
		}
		param, shape := cppParamAt(sig, i)
		rank := cppConversionRank(arg, param, cppShape{}, shape) // native value argument shapes
		if rank >= cppRankInf {
			return false
		}
		ranks[i] = rank
	}
	return true
}

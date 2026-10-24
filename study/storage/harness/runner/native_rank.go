package runner

import (
	"fmt"
	"gxstudy/adapter"
	"gxstudy/workload"
	"slices"
)

func traceNativeFanout(m adapter.MultiRepo, q workload.Query) (adapter.MultiRepo, []adapter.SearchInput) {
	if !q.NativeScoring || q.Op != "multi_symbols" {
		return m, nil
	}
	if f, ok := m.(adapter.Fanout); ok {
		f.SymbolInputs = make([]adapter.SearchInput, len(f.Segments))
		return f, f.SymbolInputs
	}
	return m, nil
}
func compareRankedAnswer(q workload.Query, w workload.Expected, got workload.Answer, inputs []adapter.SearchInput) workload.Check {
	c := compareExpected(q, w, got)
	if c.Mismatch || !q.NativeScoring || q.Op != "multi_symbols" {
		return c
	}
	fail := func(reason string) workload.Check {
		c.Mismatch = true
		c.Status = "failed"
		c.Detail = reason
		return c
	}
	if inputs == nil {
		return fail("native fan-out has no own-score input trace")
	}
	var all []adapter.Hit
	for _, input := range inputs {
		local := q
		local.Repo = input.Repo
		local.MatchRepos = w.MatchRepos
		if err := workload.Validate(local, workload.Answer{Hits: input.Hits}); err != nil {
			return fail(fmt.Sprintf("native fan-out input: %v", err))
		}
		all = append(all, input.Hits...)
	}
	if !slices.Equal(got.Hits, adapter.Top(all, q.K)) {
		return fail("native fan-out is not the top-k of its own input scores")
	}
	return c
}

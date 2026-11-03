package oracle

import (
	"context"
	"errors"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"gxstudy/workload"
	"slices"
	"strings"
)

// DiskReference computes just this query's output and the membership/tie
// evidence needed for the actual returned IDs. It never stores all matches.
func DiskReference(ctx context.Context, ds []dataset.Dataset, q workload.Query, ids []string) (workload.Expected, error) {
	segments, global := DiskView(ds)
	return diskReference(ctx, ds, segments, global, q, ids)
}

// Reference reuses one validated checker view. Native measurements have already
// stopped before this entry point, including the first index construction.
func (v *CheckerView) Reference(ctx context.Context, q workload.Query, ids []string) (workload.Expected, error) {
	if err := v.Validate(ctx); err != nil {
		return workload.Expected{}, err
	}
	out, err := diskReference(ctx, v.data, v.segments, v.global, q, ids)
	return out, errors.Join(err, v.Validate(ctx))
}

func diskReference(ctx context.Context, ds []dataset.Dataset, segments map[string]adapter.Segment, global *DiskMulti, q workload.Query, ids []string) (workload.Expected, error) {
	original := q.Op
	q.Op = strings.TrimPrefix(q.Op, "native_")
	answer, err := workload.Evaluate(ctx, segments, global, q)
	if err != nil {
		return workload.Expected{}, err
	}
	out := workload.Expected{QueryID: q.ID, Answer: answer}
	if !workload.Ranked(q.Op) {
		return out, nil
	}
	if q.Op == "multi_symbols" {
		g := answer
		out.Global = &g
		answer, err = workload.Evaluate(ctx, segments, adapter.Fanout{Segments: segments}, q)
		if err != nil {
			return out, err
		}
		out.Answer = answer
		if strings.HasPrefix(original, "native_") {
			out.Answer = g
			local := answer
			out.Local = &local
		}
	}
	if out.Local == nil {
		local := out.Answer
		out.Local = &local
	}
	out.MatchRepos = map[string]string{}
	scope := ds
	if q.Repo != "" {
		scope = nil
		for _, d := range ds {
			if d.Repo == q.Repo {
				scope = append(scope, d)
			}
		}
	}
	visit := func(hit adapter.Hit, repo string) {
		if !slices.Contains(ids, hit.ID) {
			return
		}
		out.MatchRepos[hit.ID] = repo
		out.Ties = append(out.Ties, hit)
	}
	if workload.Lexical(q.Op) {
		if q.Op == "multi_symbols" && !strings.HasPrefix(original, "native_") {
			for _, d := range scope {
				if _, err = diskSearch(ctx, []dataset.Dataset{d}, false, q.Key, 0, visit); err != nil {
					return out, err
				}
			}
		} else {
			if _, err = diskSearch(ctx, scope, q.Op == "content", q.Key, 0, visit); err != nil {
				return out, err
			}
		}
	} else {
		for _, d := range scope {
			for v, e := range d.VectorRows() {
				if e != nil {
					return out, e
				}
				if slices.Contains(ids, v.NodeID) {
					out.Ties = append(out.Ties, adapter.Hit{ID: v.NodeID, Score: cosine(q.Vec, v.Values)})
				}
			}
		}
	}
	return out, nil
}

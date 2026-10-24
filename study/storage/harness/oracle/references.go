package oracle

import (
	"fmt"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"gxstudy/workload"
	"math"
	"slices"
)

type textPair struct{ symbols, content textIndex }

func ReferenceAnswers(ds []dataset.Dataset, qs []workload.Query, global []workload.Expected) ([]workload.Expected, error) {
	if len(qs) != len(global) {
		return nil, fmt.Errorf("reference count mismatch")
	}
	pairs := map[string]textPair{}
	var allIDs, allBodies []string
	for _, d := range ds {
		var ids, bodies []string
		for _, doc := range d.Symbols {
			ids = append(ids, doc.NodeID)
			bodies = append(bodies, doc.Tokens)
		}
		pair := textPair{symbols: newText(ids, bodies)}
		allIDs = append(allIDs, ids...)
		allBodies = append(allBodies, bodies...)
		ids = nil
		bodies = nil
		for _, doc := range d.Content {
			ids = append(ids, doc.ChunkID)
			bodies = append(bodies, doc.Body)
		}
		pair.content = newText(ids, bodies)
		pairs[d.Repo] = pair
	}
	var globalText textIndex
	out := slices.Clone(global)
	for i, q := range qs {
		if !workload.Ranked(q.Op) {
			continue
		}
		if workload.Lexical(q.Op) {
			out[i].MatchRepos = map[string]string{}
		}
		var full []adapter.Hit
		switch q.Op {
		case "symbols":
			full = pairs[q.Repo].symbols.search(q.Key, -1)
		case "content":
			full = pairs[q.Repo].content.search(q.Key, -1)
		case "multi_symbols":
			if globalText.postings == nil {
				globalText = newText(allIDs, allBodies)
			}
			var merged []adapter.Hit
			for _, d := range ds {
				hits := pairs[d.Repo].symbols.search(q.Key, -1)
				for _, h := range hits {
					out[i].MatchRepos[h.ID] = d.Repo
				}
				merged = append(merged, hits...)
			}
			full = adapter.Top(merged, -1)
			g := global[i].Answer
			out[i].Global = &g
			out[i].GlobalTies = tieBand(globalText.search(q.Key, -1), g.Hits)
			out[i].Answer = workload.Answer{Hits: adapter.Top(full, q.K)}
		case "vector", "multi_vector":
			for _, d := range ds {
				if q.Op == "vector" && d.Repo != q.Repo {
					continue
				}
				for _, v := range d.Vectors {
					full = append(full, adapter.Hit{ID: v.NodeID, Score: cosine(q.Vec, v.Values)})
				}
			}
			full = adapter.Top(full, -1)
		}
		if q.Op == "symbols" || q.Op == "content" {
			for _, h := range full {
				out[i].MatchRepos[h.ID] = q.Repo
			}
		}
		if workload.Ranked(q.Op) {
			local := out[i].Answer
			out[i].Local = &local
		}
		out[i].Ties = tieBand(full, out[i].Answer.Hits)
	}
	return out, nil
}
func tieBand(full, top []adapter.Hit) []adapter.Hit {
	if len(top) == 0 {
		return nil
	}
	cutoff := top[len(top)-1].Score
	var ties []adapter.Hit
	for _, h := range full {
		if workload.NearCutoff(h.Score, cutoff) {
			ties = append(ties, h)
		}
	}
	return ties
}
func cosine(a, b []float32) float64 {
	dot, an, bn := 0., 0., 0.
	for i, x := range a {
		dot += float64(x) * float64(b[i])
		an += float64(x) * float64(x)
		bn += float64(b[i]) * float64(b[i])
	}
	return dot / math.Sqrt(an*bn)
}

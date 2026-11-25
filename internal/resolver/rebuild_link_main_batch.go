package resolver

import (
	"context"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// The same primary request/reduction is used for a scalar caller and for
// each position in the original main page. Finish can make conditional
// requests, but only after the primary stream succeeds and at the row's turn.
type rebuildMainQuery struct {
	request  store_pebble.LinkRequest
	consider func(*graph.Node) error
	finish   func() error
	charge   int // retained caller facts, in addition to request and result bytes
}

func (q rebuildMainQuery) run(ctx context.Context, f *rebuildLinkFrontier) error {
	if q.request.Key != "" {
		if _, err := f.candidates(ctx, []store_pebble.LinkRequest{q.request}, func(_ int, n *graph.Node) error { return q.consider(n) }); err != nil {
			return err
		}
	}
	return q.finishRow()
}

func (q rebuildMainQuery) finishRow() error {
	if q.finish != nil {
		return q.finish()
	}
	return nil
}

func rebuildMainCallerCharge(n *graph.Node) int {
	if n == nil {
		return 0
	}
	return len(n.ID) + len(n.FilePath) + len(n.Language) + len(n.RepoPrefix) + len(n.WorkspaceID)
}

// Preserve main's dispatch precedence. Type batches retain H96's original
// cap/refusal. Vue and dotted Python externs have dependent point/name tiers;
// leave them scalar, since no primary request is independent of those tiers.
func rebuildMainBatchFamily(edge *graph.Edge, target string) string {
	if resolutionExempt(edge) || edge.Kind == graph.EdgeCalls && phpEdgeMetaString(edge, "eloquent_model") != "" ||
		strings.HasPrefix(target, "grpc::") || strings.HasPrefix(target, "pyrel::") {
		return ""
	}
	if strings.HasPrefix(target, "import::") {
		return "import"
	}
	if spec, ok := strings.CutPrefix(target, "extern::"); ok {
		if sep := strings.LastIndex(spec, "::"); sep >= 0 && isPythonSourcePath(edge.FilePath) && isDottedPythonModule(spec[:sep]) {
			return ""
		}
		return "extern"
	}
	if primary, _ := rebuildMainTypePrimary(edge, target); primary {
		return ""
	}
	if strings.HasPrefix(target, "*.") {
		return "member"
	}
	if edge.Kind == graph.EdgeProvides || edge.Kind == graph.EdgeConsumes || edge.Kind == graph.EdgeRendersChild ||
		edge.Kind == graph.EdgeReferences && strings.HasSuffix(edge.FilePath, ".vue") && edge.Meta["template"] == true {
		return ""
	}
	if rustPath, _ := edge.Meta["rust_path"].(string); strings.Contains(rustPath, "::") {
		return ""
	}
	return "function"
}

type rebuildMainPrepared struct {
	index  int
	family string
	edge   *graph.Edge
	query  rebuildMainQuery
	charge store_pebble.CandidateBatchCharge
	err    error
}

func prepareRebuildMainQuery(ctx context.Context, f *rebuildLinkFrontier, edge *graph.Edge, target, family string, index int) rebuildMainPrepared {
	e := *edge
	p := rebuildMainPrepared{index: index, family: family, edge: &e}
	switch family {
	case "import":
		p.query, p.err = prepareRebuildImport(ctx, f, &e, strings.TrimPrefix(target, "import::"))
	case "extern":
		p.query, p.err = prepareRebuildExtern(ctx, f, &e, strings.TrimPrefix(target, "extern::"))
	case "member":
		p.query, p.err = prepareRebuildMember(ctx, f, &e, strings.TrimPrefix(target, "*."))
	case "function":
		p.query, p.err = prepareRebuildFunction(ctx, f, &e, target)
	}
	return p
}

// Prepare an ordered sub-batch from the original page, with holes for scalar
// branches. Bound retained winner facts using the entire serialized candidate
// stream, not a guess at the number of matches. A charge that cannot be
// grouped gets the unchanged singleton path; no failed request is retried.
// Retain at most one not-yet-admitted preparation for the next sub-batch.
func prepareRebuildMainBatch(ctx context.Context, f *rebuildLinkFrontier, page []*graph.Edge, start int, pending *rebuildMainPrepared) (batch []rebuildMainPrepared, next int, carry *rebuildMainPrepared, err error) {
	var limit store_pebble.CandidateBatchLimit
	var requests []store_pebble.LinkRequest
	var positions []int
	retained := 0
	next = len(page)
	for i := start; i < len(page); i++ {
		if err := ctx.Err(); err != nil {
			return nil, 0, nil, err
		}
		target := rebuildMainTarget(page[i])
		family := rebuildMainBatchFamily(page[i], target)
		if family == "" {
			continue
		}
		var p rebuildMainPrepared
		if pending != nil && pending.index == i {
			p = *pending
		} else {
			p = prepareRebuildMainQuery(ctx, f, page[i], target, family, i)
			if p.err == nil && p.query.request.Key != "" {
				p.charge, err = f.link.ChargeCandidateRequest(ctx, f.view, p.query.request)
				if err != nil {
					return nil, 0, nil, err
				}
			}
		}
		if p.err != nil {
			// Return row-local semantic/preparation failure at original order.
			batch = append(batch, p)
			next = i + 1
			break
		}
		cost := p.query.charge + p.charge.Bytes + 48*p.charge.Rows + len(p.query.request.Key) + len(p.query.request.RepoPrefix)
		for _, lang := range p.query.request.Languages {
			cost += len(lang)
		}
		fits := cost <= resolvePendingPageBytes-retained
		if fits && p.query.request.Key != "" {
			fits = limit.Add(p.query.request, p.charge, f.opts.SpillBytes)
		}
		if !fits && len(batch) > 0 {
			next, carry = i, &p
			break
		}
		if p.query.request.Key != "" {
			requests = append(requests, p.query.request)
			positions = append(positions, len(batch))
		}
		batch = append(batch, p)
		retained += cost
		if !fits {
			next = i + 1
			break
		} // scalar-admissible or genuine scalar refusal
	}
	if len(requests) > 0 {
		if _, err := f.candidates(ctx, requests, func(pos int, n *graph.Node) error {
			return batch[positions[pos]].query.consider(n)
		}); err != nil {
			return nil, 0, nil, err
		}
	}
	return batch, next, carry, nil
}

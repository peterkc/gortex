package resolver

import (
	"context"
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// rebuildTypeQuery retains only request scope and the winning-tier summary.
// The edge is borrowed from the original precommit page; no candidate node
// or source node is retained.
type rebuildTypeQuery struct {
	request                                          store_pebble.LinkRequest
	edge                                             *graph.Edge
	name, callerDir, phpNS, short, qual              string
	allowFunction, php, alias, cs, hasQual, ownScope bool
	choice                                           rebuildTypeChoice
	tier                                             int
}

func prepareRebuildType(ctx context.Context, f *rebuildLinkFrontier, edge *graph.Edge, name string, allowFunction bool) (rebuildTypeQuery, error) {
	if !allowFunction {
		name = strings.TrimPrefix(name, "*.")
	}
	source, err := f.view.Node(ctx, edge.From)
	if err != nil {
		return rebuildTypeQuery{}, err
	}
	var repo, language string
	if source != nil {
		repo, language = source.RepoPrefix, source.Language
	}
	_, langs := resolverCompatibleLanguages(language)
	q := rebuildTypeQuery{
		request: store_pebble.LinkRequest{Kind: store_pebble.LinkName, Key: name, RepoPrefix: repo, Languages: langs},
		edge:    edge, name: name, allowFunction: allowFunction, callerDir: filePathDir(edge.FilePath), cs: strings.HasSuffix(edge.FilePath, ".cs"),
		ownScope: (source != nil || graph.RepoPrefixOfID(edge.From) == repo) && len(langs) == 0,
	}
	fqn := phpEdgeMetaString(edge, "target_fqn")
	q.php = fqn != "" && (!strings.Contains(fqn, ".") || strings.Contains(fqn, `\`))
	q.short = fqn
	if i := strings.LastIndex(fqn, `\`); i >= 0 {
		q.phpNS, q.short = fqn[:i], fqn[i+1:]
	}
	q.alias = q.php && q.short != name
	if q.alias {
		// phpFindByFQN is a repo-only lookup, not the caller language scope.
		q.request.Key, q.request.Languages = q.short, []string{"php"}
	}
	if i := strings.LastIndexByte(fqn, '.'); i >= 0 {
		q.qual, q.hasQual = fqn[:i], true
	}
	return q, nil
}

func (q *rebuildTypeQuery) consider(n *graph.Node) {
	one := []*graph.Node{n}
	if len(withoutAttributedTerminals(one)) == 0 {
		return
	}
	phpMatch := q.php && n.Language == "php" && phpNodeNamespace(n) == q.phpNS
	if q.alias {
		if !phpMatch || (n.Kind != graph.KindType && n.Kind != graph.KindInterface) {
			return
		}
	} else if len(genericInstantiationOnly(q.edge, one)) == 0 {
		return
	}

	// Both narrowers keep the original set on a miss. PHP runs first
	// and may match a non-type (including one with no usable target).
	ns := ""
	csEligible := q.cs && csharpNarrowEligible(n)
	if csEligible {
		ns, _ = n.Meta["scope_ns"].(string)
	}
	candidateTier := 0
	switch {
	case phpMatch:
		candidateTier = 2
	case csEligible && q.hasQual && (ns == q.qual || strings.HasSuffix(ns, "."+q.qual)):
		candidateTier = 1
	}
	if candidateTier < q.tier {
		return
	}
	if candidateTier > q.tier {
		q.tier, q.choice = candidateTier, rebuildTypeChoice{}
	}
	q.choice.consider(n, ns, q.callerDir, q.allowFunction)
}

// finish changes only the borrowed row's tentative copy, after the entire
// candidate stream succeeded. Case-specific refusals retain original order.
func (q *rebuildTypeQuery) finish(edge *graph.Edge) error {
	if q.choice.id == "" {
		return nil
	}
	if q.alias {
		// A matching alias is certain when this edge itself warmed the
		// short name for this repo and PHP. Otherwise a different edge in
		// the frozen page can install an authoritative non-PHP-only group.
		target := graph.UnresolvedName(edge.To)
		if !q.ownScope || strings.HasPrefix(target, "extern::") ||
			(identifierFromTarget(target) != q.short && edgeReceiverType(edge) != q.short) {
			return fmt.Errorf("%w: PHP alias %q in %q needs page name scopes", store_pebble.ErrRebuildLink, q.short, q.request.RepoPrefix)
		}
	}
	if q.choice.severalNS || (q.choice.oneNS != "" && q.choice.bestNS != q.choice.oneNS) {
		return fmt.Errorf("%w: type %q in %q needs C# namespace visibility", store_pebble.ErrRebuildLink, q.name, edge.FilePath)
	}
	edge.To = q.choice.id
	return nil
}

// The Vue fallback remains conditional on its import lookup. It uses the
// exact same preparation, candidate reduction and finish as batched types.
func resolveRebuildType(ctx context.Context, f *rebuildLinkFrontier, edge *graph.Edge, name string, allowFunction bool) error {
	q, err := prepareRebuildType(ctx, f, edge, name, allowFunction)
	if err != nil || q.request.Key == "" {
		return err
	}
	if _, err := f.candidates(ctx, []store_pebble.LinkRequest{q.request}, func(_ int, n *graph.Node) error {
		q.consider(n)
		return nil
	}); err != nil {
		return err
	}
	return q.finish(edge)
}

// resolveRebuildTypePage collects only unconditional primary type requests
// from the selected original page. It shares the same dispatcher predicate
// as the reducer. A single bounded stream delivers global request positions,
// preserving native candidate order per position; nothing is applied here.
func resolveRebuildTypePage(ctx context.Context, f *rebuildLinkFrontier, page []*graph.Edge) ([]rebuildTypeQuery, error) {
	if len(page) > resolvePendingPageRows {
		return nil, fmt.Errorf("%w: primary type page rows", store_pebble.ErrRebuildLinkInputSize)
	}
	queries := make([]rebuildTypeQuery, len(page))
	var requests []store_pebble.LinkRequest
	var positions []int
	requestBytes := 0
	for i, edge := range page {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		target := rebuildMainTarget(edge)
		primary, allowFunction := rebuildMainTypePrimary(edge, target)
		if !primary {
			continue
		}
		q, err := prepareRebuildType(ctx, f, edge, target, allowFunction)
		if err != nil {
			return nil, err
		}
		if q.request.Key == "" {
			continue
		}
		// Bound retained caller scope during preparation, before the existing
		// stricter 2 MiB wire check runs in SelectCandidatePage.
		charge := len(q.request.Key) + len(q.request.RepoPrefix)
		for _, lang := range q.request.Languages {
			charge += len(lang)
		}
		if charge > resolvePendingPageBytes-requestBytes {
			return nil, fmt.Errorf("%w: primary type request bytes", store_pebble.ErrRebuildLinkInputSize)
		}
		requestBytes += charge
		queries[i] = q
		positions = append(positions, i)
		requests = append(requests, q.request)
	}
	if len(requests) == 0 {
		return queries, nil
	}
	summaryBytes := 0
	_, err := f.candidates(ctx, requests, func(pos int, n *graph.Node) error {
		q := &queries[positions[pos]]
		before := q.choice.bytes()
		q.consider(n)
		summaryBytes += q.choice.bytes() - before
		if summaryBytes > resolvePendingPageBytes {
			return fmt.Errorf("%w: primary type summary bytes", store_pebble.ErrRebuildLinkInputSize)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return queries, nil
}

// Only the highest nonempty narrowing tier survives. Within it, visibility
// cannot change the result if every eligible namespace selects the same best
// type as the bare lookup. No list grows with the number of candidates.
type rebuildTypeChoice struct {
	id        string
	rank      int
	sameDir   bool
	bestNS    string
	oneNS     string
	severalNS bool
}

func (c rebuildTypeChoice) bytes() int {
	return len(c.id) + len(c.bestNS) + len(c.oneNS)
}

func (c *rebuildTypeChoice) consider(n *graph.Node, ns, callerDir string, allowFunction bool) {
	if ns != "" {
		if c.oneNS == "" {
			c.oneNS = ns
		} else if c.oneNS != ns {
			c.severalNS = true
		}
	}
	sameDir := filePathDir(n.FilePath) == callerDir
	rank := -1
	switch n.Kind {
	case graph.KindType, graph.KindInterface:
		rank = typeCandidateRank(n)
		if c.id != "" && !candidateBeats(rank, sameDir, n.ID, c.rank, c.sameDir, c.id) {
			return
		}
	case graph.KindFunction, graph.KindMethod:
		// First in native stream order within the preferred directory tier.
		if !allowFunction || (c.id != "" && (c.rank >= 0 || c.sameDir || !sameDir)) {
			return
		}
	default:
		return
	}
	c.id, c.rank, c.sameDir, c.bestNS = n.ID, rank, sameDir, ns
}

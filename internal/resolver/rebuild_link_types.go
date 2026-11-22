package resolver

import (
	"context"
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// resolveRebuildType applies only the winning target, after the complete name
// stream succeeds. A hierarchy edge never falls through to a function.
func resolveRebuildType(ctx context.Context, f *rebuildLinkFrontier, edge *graph.Edge, name string, allowFunction bool) error {
	if !allowFunction {
		name = strings.TrimPrefix(name, "*.")
	}
	source, err := f.view.Node(ctx, edge.From)
	if err != nil {
		return err
	}
	var repo, language string
	if source != nil {
		repo, language = source.RepoPrefix, source.Language
	}
	_, langs := resolverCompatibleLanguages(language)
	req := store_pebble.LinkRequest{Kind: store_pebble.LinkName, Key: name, RepoPrefix: repo, Languages: langs}

	fqn := phpEdgeMetaString(edge, "target_fqn")
	php := fqn != "" && (!strings.Contains(fqn, ".") || strings.Contains(fqn, `\`))
	phpNS, short := "", fqn
	if i := strings.LastIndex(fqn, `\`); i >= 0 {
		phpNS, short = fqn[:i], fqn[i+1:]
	}
	alias := php && short != name
	if alias {
		// phpFindByFQN is a repo-only lookup, not the caller language scope.
		req.Key, req.Languages = short, []string{"php"}
	}
	if req.Key == "" {
		return nil
	}
	callerDir := filePathDir(edge.FilePath)
	cs := strings.HasSuffix(edge.FilePath, ".cs")
	q := strings.LastIndexByte(fqn, '.')
	var qual string
	if q >= 0 {
		qual = fqn[:q]
	}

	var choice rebuildTypeChoice
	tier := 0
	_, err = f.candidates(ctx, []store_pebble.LinkRequest{req}, func(_ int, n *graph.Node) error {
		one := []*graph.Node{n}
		if len(withoutAttributedTerminals(one)) == 0 {
			return nil
		}
		phpMatch := php && n.Language == "php" && phpNodeNamespace(n) == phpNS
		if alias {
			if !phpMatch || (n.Kind != graph.KindType && n.Kind != graph.KindInterface) {
				return nil
			}
		} else if len(genericInstantiationOnly(edge, one)) == 0 {
			return nil
		}

		// Both narrowers keep the original set on a miss. PHP runs first
		// and may match a non-type (including one with no usable target).
		ns := ""
		csEligible := cs && csharpNarrowEligible(n)
		if csEligible {
			ns, _ = n.Meta["scope_ns"].(string)
		}
		candidateTier := 0
		switch {
		case phpMatch:
			candidateTier = 2
		case csEligible && q >= 0 && (ns == qual || strings.HasSuffix(ns, "."+qual)):
			candidateTier = 1
		}
		if candidateTier < tier {
			return nil
		}
		if candidateTier > tier {
			tier, choice = candidateTier, rebuildTypeChoice{}
		}
		choice.consider(n, ns, callerDir, allowFunction)
		return nil
	})
	if err != nil || choice.id == "" {
		return err
	}
	if alias {
		// A matching alias is certain when this edge itself warmed the
		// short name for this repo and PHP. Otherwise a different edge in
		// the frozen page can install an authoritative non-PHP-only group.
		ownRepo := source != nil || graph.RepoPrefixOfID(edge.From) == repo
		target := graph.UnresolvedName(edge.To)
		if !ownRepo || len(langs) != 0 || strings.HasPrefix(target, "extern::") ||
			(identifierFromTarget(target) != short && edgeReceiverType(edge) != short) {
			return fmt.Errorf("%w: PHP alias %q in %q needs page name scopes", store_pebble.ErrRebuildLink, short, repo)
		}
	}
	if choice.severalNS || (choice.oneNS != "" && choice.bestNS != choice.oneNS) {
		return fmt.Errorf("%w: type %q in %q needs C# namespace visibility", store_pebble.ErrRebuildLink, name, edge.FilePath)
	}
	edge.To = choice.id
	return nil
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

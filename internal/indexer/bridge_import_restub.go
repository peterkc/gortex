package indexer

import "github.com/zzet/gortex/internal/graph"

// Repository parking can only change foreign imports/calls. Read their compact
// keys first, then hydrate those sites after the identity cursor has closed.
func foreignBridgeIncoming(g graph.Store, repo string, ids []string) map[string][]*graph.Edge {
	identities := graph.InEdgeIdentitiesByNodeIDs(g, ids)
	selected := map[graph.EdgeIdentity]bool{}
	seen := map[graph.EdgeSite]bool{}
	var sites []graph.EdgeSite
	for _, rows := range identities {
		for _, row := range rows {
			if graph.RepoPrefixOfID(row.From) == repo || row.Kind != graph.EdgeImports && row.Kind != graph.EdgeCalls {
				continue
			}
			selected[row] = true
			site := graph.EdgeSite{From: row.From, Line: row.Line, Kind: row.Kind}
			if !seen[site] {
				seen[site] = true
				sites = append(sites, site)
			}
		}
	}
	incoming := map[string][]*graph.Edge{}
	for start := 0; start < len(sites); start += 512 {
		page := sites[start:min(start+512, len(sites))]
		rows := g.GetEdgeCandidates(nil, page)
		for _, site := range page {
			for _, edge := range rows.Site(site.From, site.Line, site.Kind) {
				if selected[graph.EdgeIdentityFor(edge)] {
					incoming[edge.To] = append(incoming[edge.To], edge)
				}
			}
		}
	}
	return incoming
}

package store_pebble

import (
	"bytes"
	"context"
	"iter"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// These read shapes are also exercised by the legacy Store conformance suite.
func (s *Snapshot) GetNodesByQualNames(names []string) map[string][]*graph.Node {
	out := make(map[string][]*graph.Node)
	for _, name := range names {
		if name == "" || out[name] != nil {
			continue
		}
		if hits := s.findIndexedNodes(keyStrings([]byte{familyName, nameQualified}, name)); len(hits) != 0 {
			out[name] = hits
		}
	}
	return out
}

func (s *Snapshot) FindNodesByNameInRepo(name, repo string) []*graph.Node {
	var out []*graph.Node
	s.indexedNodes(keyStrings([]byte{familyName, nameExact}, name), func(n *graph.Node) bool {
		if repo == "" || n.RepoPrefix == repo {
			out = append(out, n)
		}
		return true
	})
	return out
}

func (s *Snapshot) FindNodesByNames(names []string) map[string][]*graph.Node {
	out := make(map[string][]*graph.Node)
	for _, name := range names {
		if hits := s.FindNodesByName(name); len(hits) != 0 {
			out[name] = hits
		}
	}
	return out
}

func (s *Snapshot) GetFileNodesByPaths(paths []string) map[string][]*graph.Node {
	out := make(map[string][]*graph.Node)
	for _, path := range paths {
		if path != "" && out[path] == nil {
			if nodes := s.GetFileNodes(path); len(nodes) != 0 {
				out[path] = nodes
			}
		}
	}
	return out
}

func (s *Snapshot) GetRepoNodesByLanguage(repo, language string) []*graph.Node {
	return s.findIndexedNodes(keyStrings([]byte{familyName, nameRepoLanguage}, repo, language))
}

func (s *Snapshot) GetNodesByLanguage(language string) []*graph.Node {
	return s.findIndexedNodes(keyStrings([]byte{familyName, nameLanguage}, language))
}

func (s *Snapshot) GetRepoNonContentNodes(repo string) []*graph.Node {
	prefix := []byte{familyName, nameNonContent}
	if repo != "" {
		prefix = appendKeyString(prefix, repo)
	}
	return s.findIndexedNodes(prefix)
}

func (s *Snapshot) GetRepoEdges(repo string) []*graph.Edge {
	if repo == "" {
		return nil
	}
	var out []*graph.Edge
	s.indexedNodes(keyStrings([]byte{familyRepo}, repo), func(n *graph.Node) bool {
		out = append(out, s.GetOutEdges(n.ID)...)
		return true
	})
	return out
}

func (s *Snapshot) GetEdgeCandidates(endpoints []graph.EdgeEndpoint, sites []graph.EdgeSite) graph.EdgeCandidateSet {
	out := graph.NewEdgeCandidateSet()
	seenEndpoint := make(map[graph.EdgeEndpoint]bool)
	for _, endpoint := range endpoints {
		if seenEndpoint[endpoint] {
			continue
		}
		seenEndpoint[endpoint] = true
		for _, e := range s.GetInEdges(endpoint.To) {
			if e.From == endpoint.From {
				out.AddEndpoint(e)
			}
		}
	}
	seenSite := make(map[graph.EdgeSite]bool)
	for _, site := range sites {
		if seenSite[site] {
			continue
		}
		seenSite[site] = true
		prefix := appendKeyLine(keyStrings([]byte{familyEdge, edgeOut}, site.From), site.Line)
		decodeRows(s, prefix, decodeEdge, func(e *graph.Edge) bool {
			if site.Kind == "" || e.Kind == site.Kind {
				out.AddSite(e)
			}
			return true
		})
	}
	return out
}

func externalTarget(id string) bool {
	return strings.HasPrefix(id, "dep::") || strings.HasPrefix(id, "external::") || strings.HasPrefix(id, "external-call::") || strings.HasPrefix(id, "stdlib::") || strings.Contains(id, "::stdlib::")
}

func (s *Snapshot) DistinctExternalTargets(kinds []graph.EdgeKind, fileSuffix string) []string {
	seen := make(map[string]struct{})
	for _, kind := range kinds {
		for e := range s.EdgesByKind(kind) {
			if externalTarget(e.To) && strings.HasSuffix(e.FilePath, fileSuffix) {
				seen[e.To] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

func (s *Snapshot) EdgesWithUnresolvedTarget() iter.Seq[*graph.Edge] {
	return func(yield func(*graph.Edge) bool) {
		decodeRows(s, []byte{familyEdge, edgeIn}, decodeEdge, func(e *graph.Edge) bool {
			return !graph.IsUnresolvedTarget(e.To) || graph.IsFnValuePlaceholder(e.To) || yield(e)
		})
	}
}

func (s *Snapshot) FnValuePlaceholderEdges() iter.Seq[*graph.Edge] {
	return func(yield func(*graph.Edge) bool) {
		for e := range s.EdgesByKind(graph.EdgeReferences) {
			if graph.IsFnValuePlaceholder(e.To) && !yield(e) {
				return
			}
		}
	}
}

func (s *Snapshot) ValueRefPlaceholderEdges() iter.Seq[*graph.Edge] {
	return func(yield func(*graph.Edge) bool) {
		for e := range s.EdgesByKind(graph.EdgeReads) {
			if strings.HasPrefix(e.To, "unresolved::valueref::") && !yield(e) {
				return
			}
		}
	}
}

func (s *Snapshot) AllNodesLight() []*graph.Node {
	return listRows(s, []byte{familyNode}, decodeNodeLight)
}

// Proxy identities are not allowed in a published graph.
func (s *Snapshot) ProxyNodeCountAtLeast(limit int) bool { return limit <= 0 }

func (s *Snapshot) RepoPrefixes() []string {
	return slices.DeleteFunc(s.repos(), func(repo string) bool { return repo == "" })
}

func (s *Snapshot) RepoMemoryEstimate(repo string) graph.RepoMemoryEstimate {
	return graph.RepoMemoryEstimate{
		NodeCount: s.count("repo", repo), EdgeCount: s.count("repo edges", repo),
		NodeBytes: uint64(s.count("repo node bytes", repo)), EdgeBytes: uint64(s.count("repo edge bytes", repo)),
	}
}

func (s *Snapshot) AllRepoMemoryEstimates() map[string]graph.RepoMemoryEstimate {
	out := make(map[string]graph.RepoMemoryEstimate)
	for _, repo := range s.repos() {
		out[repo] = s.RepoMemoryEstimate(repo)
	}
	return out
}

func (s *Snapshot) VerifyEdgeIdentities() error {
	return scan(context.Background(), s, []byte{familyEdge, edgeOut}, func(key, value []byte) (bool, error) {
		e, err := decodeEdge(value)
		if err != nil {
			return false, err
		}
		id := edgeIdentity(e)
		if !bytes.Equal(key, outEdgeKey(e, id)) {
			return false, invalid("out-edge identity mismatch")
		}
		in := append(keyStrings([]byte{familyEdge, edgeIn}, e.To, string(e.Kind)), id[:]...)
		same, err := point(s, in, func(v []byte) (bool, error) { return bytes.Equal(v, value), nil })
		if err != nil {
			return false, err
		}
		if !same {
			return false, invalid("in-edge identity mismatch")
		}
		return true, nil
	})
}

package store_pebble

import (
	"context"
	"encoding/binary"

	"github.com/zzet/gortex/internal/graph"
)

func countKey(fields ...string) []byte { return keyStrings([]byte{familyCount}, fields...) }

func encodeCount(n int) []byte { return binary.BigEndian.AppendUint64(nil, uint64(n)) }

func decodeCount(b []byte) (int, error) {
	if len(b) != 8 {
		return 0, invalid("counter width")
	}
	n := binary.BigEndian.Uint64(b)
	if int(n) < 0 || uint64(int(n)) != n {
		return 0, invalid("counter overflow")
	}
	return int(n), nil
}

func (s *Snapshot) count(fields ...string) int {
	n, err := point(s, countKey(fields...), decodeCount)
	s.retain(err)
	return n
}

func (s *Snapshot) NodeCount() int             { return s.count("nodes") }
func (s *Snapshot) EdgeCount() int             { return s.count("edges") }
func (s *Snapshot) EdgeIdentityRevisions() int { return s.count("revisions") }

func (s *Snapshot) repos() []string {
	prefix := countKey("repo")
	var repos []string
	s.retain(scan(context.Background(), s, prefix, func(key, _ []byte) (bool, error) {
		repo, tail, err := readKeyString(key[len(prefix):])
		if err != nil || len(tail) != 0 {
			return false, invalid("repository counter key")
		}
		repos = append(repos, repo)
		return true, nil
	}))
	return repos
}

func (s *Snapshot) countLabels(prefix []byte) map[string]int {
	out := make(map[string]int)
	s.retain(scan(context.Background(), s, prefix, func(key, value []byte) (bool, error) {
		label, tail, err := readKeyString(key[len(prefix):])
		if err != nil || len(tail) != 0 {
			return false, invalid("stats counter key")
		}
		n, err := decodeCount(value)
		if err != nil {
			return false, err
		}
		out[label] = n
		return true, nil
	}))
	return out
}

func (s *Snapshot) Stats() graph.GraphStats {
	return graph.GraphStats{TotalNodes: s.NodeCount(), TotalEdges: s.EdgeCount(), ByKind: s.countLabels(countKey("node kind")), ByLanguage: s.countLabels(countKey("language"))}
}

func (s *Snapshot) RepoStats() map[string]graph.GraphStats {
	out := make(map[string]graph.GraphStats)
	for _, repo := range s.repos() {
		if repo == "" {
			continue
		}
		out[repo] = graph.GraphStats{TotalNodes: s.count("repo", repo), TotalEdges: s.count("repo edges", repo),
			ByKind: s.countLabels(countKey("repo kind", repo)), ByLanguage: s.countLabels(countKey("repo language", repo))}
	}
	return out
}

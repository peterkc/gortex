package store_pebble

import (
	"bytes"
	"container/heap"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
	"modernc.org/libc"
)

var (
	_ graph.SymbolSearchReader         = (*Snapshot)(nil)
	_ graph.SymbolBundleSearcher       = (*Snapshot)(nil)
	_ graph.ScopedSymbolBundleSearcher = (*Snapshot)(nil)
)

func searchRepos(all, allow []string) []string {
	if len(allow) == 0 {
		return append([]string(nil), all...)
	}
	out := []string{""}
	seen := map[string]bool{"": true}
	for _, v := range allow {
		if !seen[v] {
			out = append(out, v)
			seen[v] = true
		}
	}
	sort.Strings(out)
	return out
}
func searchPath(file, repo string) string {
	rel := strings.ReplaceAll(file, "\\", "/")
	if repo != "" {
		rel = strings.TrimPrefix(rel, repo+"/")
	}
	return rel
}
func searchPathAllows(path string, allow []string) bool {
	if len(allow) == 0 {
		return true
	}
	for _, p := range allow {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}
func searchExactKind(kind string) bool {
	switch graph.NodeKind(kind) {
	case graph.KindModule, graph.KindPackage, graph.KindFile, graph.KindImport, graph.KindLocal, graph.KindParam, graph.KindGenericParam, graph.KindClosure, graph.KindBuiltin:
		return false
	}
	return true
}
func searchIdentifier(q string) bool { return q != "" && !strings.ContainsAny(q, " \t\n/.:,") }
func searchASCII(q string) bool {
	for i := 0; i < len(q); i++ {
		if q[i] >= 128 {
			return false
		}
	}
	return true
}

type searchScored struct {
	ID      string
	NodeOff bool
	Rowid   int64
	Score   float64
}
type searchTop []searchScored

func (h searchTop) Len() int { return len(h) }
func searchBetter(a, b searchScored) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	return a.Rowid < b.Rowid
}
func (h searchTop) Less(i, j int) bool { return searchBetter(h[j], h[i]) }
func (h searchTop) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *searchTop) Push(x any)        { *h = append(*h, x.(searchScored)) }
func (h *searchTop) Pop() any          { old := *h; n := len(old); v := old[n-1]; *h = old[:n-1]; return v }
func searchKeep(top *searchTop, v searchScored, limit int) {
	if len(*top) < limit {
		heap.Push(top, v)
		return
	}
	if searchBetter(v, (*top)[0]) {
		(*top)[0] = v
		heap.Fix(top, 0)
	}
}
func searchScore(freq, idf []float64, length int64, avgdl float64) float64 {
	score := float64(0)
	for i := range freq {
		score = score + float64(idf[i]*searchFactor(freq[i], length, avgdl))
	}
	return -float64(-float64(1) * score)
}
func searchFreq(d searchDoc, phrases []string) ([]float64, string) {
	freq := make([]float64, len(phrases))
	first := ""
	for _, term := range d.Terms {
		for i, prefix := range phrases {
			if strings.HasPrefix(term.Text, prefix) {
				freq[i] += float64(term.Freq)
				if first == "" {
					first = term.Text
				}
			}
		}
	}
	return freq, first
}

type searchRange struct{ lo, hi []byte }

func searchPostingRanges(repo, term string, paths []string) []searchRange {
	base := searchKey("post", repo, term)
	if len(paths) == 0 {
		return []searchRange{{base, prefixEnd(base)}}
	}
	var ranges []searchRange
	for _, path := range paths {
		exact := appendKeyString(append([]byte(nil), base...), path)
		desc := searchPartial(base, path+"/")
		ranges = append(ranges, searchRange{exact, prefixEnd(exact)}, searchRange{desc, prefixEnd(desc)})
	}
	sort.Slice(ranges, func(i, j int) bool { return bytes.Compare(ranges[i].lo, ranges[j].lo) < 0 })
	var merged []searchRange
	for _, rr := range ranges {
		if len(merged) > 0 && bytes.Compare(rr.lo, merged[len(merged)-1].hi) <= 0 {
			if bytes.Compare(rr.hi, merged[len(merged)-1].hi) > 0 {
				merged[len(merged)-1].hi = rr.hi
			}
			continue
		}
		merged = append(merged, rr)
	}
	return merged
}

// SearchContent refuses the deferred content FTS family (D64). This reader
// does not implement the write side of graph.ContentSearcher.
func (s *Snapshot) SearchContent(_, _ string, _ int) ([]graph.ContentHit, error) {
	return nil, fmt.Errorf("pebble content search: %w", errors.ErrUnsupported)
}

func (s *Snapshot) SearchSymbols(q string, limit int) ([]graph.SymbolHit, error) {
	return s.search(context.Background(), q, nil, nil, limit)
}

func (s *Snapshot) SearchSymbolsRepoScopedContext(ctx context.Context, q string, repos []string, limit int) ([]graph.SymbolHit, error) {
	return s.search(ctx, q, repos, nil, limit)
}

func (s *Snapshot) SearchSymbolBundles(q string, limit int) ([]graph.SymbolBundle, error) {
	return s.SearchSymbolBundlesRepoScoped(q, nil, limit)
}

func (s *Snapshot) SearchSymbolBundlesRepoScoped(q string, repos []string, limit int) ([]graph.SymbolBundle, error) {
	hits, err := s.SearchSymbolsRepoScopedContext(context.Background(), q, repos, limit)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.NodeID)
	}
	nodes, err := s.GetNodesByIDsContext(context.Background(), ids)
	if err != nil {
		return nil, err
	}
	ins, outs := s.GetInEdgesByNodeIDs(ids), s.GetOutEdgesByNodeIDs(ids)
	if err := s.ReadError(); err != nil {
		return nil, err
	}
	var out []graph.SymbolBundle
	for _, h := range hits {
		if node := nodes[h.NodeID]; node != nil {
			out = append(out, graph.SymbolBundle{Node: node, Score: h.Score, InEdges: ins[h.NodeID], OutEdges: outs[h.NodeID]})
		}
	}
	return out, nil
}

const searchMemoEntries = 32
const searchMemoRows = 1024

type searchMemoEntry struct {
	key  string
	hits []graph.SymbolHit
}

func (s *Snapshot) memo(key string) ([]graph.SymbolHit, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, e := range s.searchMemo {
		if e.key == key {
			copy(s.searchMemo[1:i+1], s.searchMemo[:i])
			s.searchMemo[0] = e
			return slices.Clone(e.hits), true
		}
	}
	return nil, false
}

func (s *Snapshot) saveMemo(key string, hits []graph.SymbolHit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, e := range s.searchMemo {
		if e.key == key {
			s.searchMemo = slices.Delete(s.searchMemo, i, i+1)
			break
		}
	}
	s.searchMemo = append(s.searchMemo, searchMemoEntry{})
	copy(s.searchMemo[1:], s.searchMemo[:len(s.searchMemo)-1])
	s.searchMemo[0] = searchMemoEntry{key, slices.Clone(hits)}
	if len(s.searchMemo) > searchMemoEntries {
		s.searchMemo[searchMemoEntries] = searchMemoEntry{}
		s.searchMemo = s.searchMemo[:searchMemoEntries]
	}
}

func searchMemoKey(query string, repos, paths []string, limit int) string {
	key := keyStrings(nil, query, strconv.Itoa(limit), strconv.Itoa(len(repos)))
	key = keyStrings(key, repos...)
	return string(keyStrings(key, paths...))
}

func (s *Snapshot) search(ctx context.Context, q string, repos, paths []string, limit int) ([]graph.SymbolHit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if q == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	stats, err := point(s, searchKey("stats"), searchReadStats)
	if err != nil {
		return nil, err
	}
	owners := searchRepos(stats.Repos, repos)
	if searchIdentifier(q) {
		var hits []graph.SymbolHit
		for _, repo := range owners {
			err := scan(ctx, s, searchKey("exact", repo, q), func(_, value []byte) (bool, error) {
				id := string(value)
				if id == "" {
					return false, invalid("missing exact symbol ID")
				}
				if len(paths) != 0 {
					n, err := point(s, keyStrings([]byte{familyNode}, id), decodeNodeLight)
					if err != nil {
						return false, err
					}
					if n == nil {
						return false, invalid("missing exact symbol node")
					}
					if !searchPathAllows(searchPath(n.FilePath, n.RepoPrefix), paths) {
						return true, nil
					}
				}
				hits = append(hits, graph.SymbolHit{NodeID: id, Score: 100})
				return true, nil
			})
			if err != nil {
				return nil, err
			}
		}
		if len(hits) != 0 {
			slices.SortFunc(hits, func(a, b graph.SymbolHit) int { return strings.Compare(a.NodeID, b.NodeID) })
			return hits[:min(len(hits), limit)], nil
		}
	}
	if !searchASCII(q) {
		return nil, fmt.Errorf("non-ASCII FTS query: %w", errors.ErrUnsupported)
	}
	phrases, ok := search.FTSRankTerms(q)
	if !ok {
		tokens := search.Tokenize(q)
		if len(tokens) == 0 {
			tokens = search.TokenizeQuery(q)
		}
		if len(search.NormalizeFTSTokens(tokens)) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("FTS phrase not one unicode61 term: %w", errors.ErrUnsupported)
	}
	if stats.Rows == 0 || stats.Tokens == 0 {
		return nil, nil
	}
	memoKey := searchMemoKey(q, repos, paths, limit)
	if hits, ok := s.memo(memoKey); ok {
		return hits, ctx.Err()
	}
	idf := make([]float64, len(phrases))
	prefixes := make([]uint32, len(phrases))
	for i, p := range phrases {
		type prefixStat struct{ count, id int }
		row, err := point(s, searchKey("prefix-df", p), func(raw []byte) (prefixStat, error) {
			c := recordCursor{buf: raw}
			row := prefixStat{c.integer(), c.integer()}
			if err := c.finish(); err != nil {
				return prefixStat{}, err
			}
			if row.count <= 0 || int64(row.count) > stats.Rows || row.id <= 0 || uint64(row.id) > uint64(^uint32(0)) {
				return prefixStat{}, invalid("search prefix statistics")
			}
			return row, nil
		})
		if err != nil {
			return nil, err
		}
		prefixes[i] = uint32(row.id)
		n := float64(row.count)
		idf[i] = libc.Xlog(nil, (float64(stats.Rows)-n+0.5)/(n+0.5))
		if idf[i] <= 0 {
			idf[i] = 1e-6
		}
	}
	ordered := slices.Clone(phrases)
	slices.Sort(ordered)
	var minimal []string
	for _, p := range ordered {
		if len(minimal) == 0 || !strings.HasPrefix(p, minimal[len(minimal)-1]) {
			minimal = append(minimal, p)
		}
	}
	top := &searchTop{}
	avg := float64(stats.Tokens) / float64(stats.Rows)
	for _, repo := range owners {
		for _, prefix := range minimal {
			err := scan(ctx, s, searchPartial(searchKey("term", repo), prefix), func(_, value []byte) (bool, error) {
				term := string(value)
				for _, bounds := range searchPostingRanges(repo, term, paths) {
					err := scanRange(ctx, s, bounds.lo, bounds.hi, func(_, raw []byte) (bool, error) {
						bq := searchBlockQuery{Phrases: phrases, PrefixIDs: prefixes, IDF: idf, CanSkip: len(*top) == limit}
						if bq.CanSkip {
							bq.Threshold = (*top)[0].Score
						}
						_, err := searchReadBlock(raw, bq, func(ref searchPosting) error {
							d, err := point(s, binary.BigEndian.AppendUint64(searchKey("doc"), uint64(ref.Rowid)), searchReadDoc)
							if err != nil {
								return err
							}
							if d.Rowid != ref.Rowid || d.NodePresent != ref.NodePresent {
								return invalid("posting document identity")
							}
							if !searchPathAllows(d.Path, paths) {
								return nil
							}
							freq, first := searchFreq(d, phrases)
							if first == term {
								searchKeep(top, searchScored{ID: d.ID, NodeOff: !d.NodePresent, Rowid: d.Rowid, Score: searchScore(freq, idf, d.Length, avg)}, limit)
							}
							return nil
						})
						return err == nil, err
					})
					if err != nil {
						return false, err
					}
				}
				return true, nil
			})
			if err != nil {
				return nil, err
			}
		}
	}
	sort.Slice(*top, func(i, j int) bool { return searchBetter((*top)[i], (*top)[j]) })
	var hits []graph.SymbolHit
	for _, v := range *top {
		if v.NodeOff {
			return nil, groupOffError{localNodeGroup, s.profile}
		}
		if v.ID != "" {
			hits = append(hits, graph.SymbolHit{NodeID: v.ID, Score: v.Score})
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= searchMemoRows {
		s.saveMemo(memoKey, hits)
	}
	return hits, nil
}

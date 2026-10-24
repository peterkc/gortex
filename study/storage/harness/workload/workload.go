// Package workload owns raw calls, untimed validation, and deterministic requests.
package workload

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"math"
	"slices"
	"strings"
)

const FanoutCap = 20
const VisitCap = 1000
const MinimumRankedOverlap = 0.8
const CutoffRelativeTolerance = 1e-5

func NearCutoff(score, cutoff float64) bool {
	return math.Abs(score-cutoff) <= CutoffRelativeTolerance*math.Max(math.Abs(cutoff), 1e-12)
}

type Query struct {
	NativeScoring bool              `json:"-"`
	MatchRepos    map[string]string `json:"-"`
	ID            string            `json:"id"`
	Op            string            `json:"op"`
	Repo          string            `json:"repo,omitempty"`
	Key           string            `json:"key,omitempty"`
	Kind          string            `json:"kind,omitempty"`
	K             int               `json:"k,omitempty"`
	Depth         int               `json:"depth,omitempty"`
	Vec           []float32         `json:"vec,omitempty"`
	MinOverlap    float64           `json:"min_overlap,omitempty"`
	ExactRank     bool              `json:"exact_rank,omitempty"`
	ForbiddenIDs  []string          `json:"forbidden_ids,omitempty"`
	RequiredIDs   []string          `json:"required_ids,omitempty"`
	CutoffTies    []adapter.Hit     `json:"cutoff_ties,omitempty"`
}
type Answer struct {
	Node    *dataset.Node  `json:"node,omitempty"`
	Nodes   []dataset.Node `json:"nodes,omitempty"`
	Edges   []dataset.Edge `json:"edges,omitempty"`
	IDs     []string       `json:"ids,omitempty"`
	Hits    []adapter.Hit  `json:"hits,omitempty"`
	Missing bool           `json:"missing,omitempty"`
}
type Expected struct {
	Local      *Answer           `json:"local,omitempty"`
	MatchRepos map[string]string `json:"match_repositories,omitempty"`
	Global     *Answer           `json:"global,omitempty"`
	Ties       []adapter.Hit     `json:"ties,omitempty"`
	GlobalTies []adapter.Hit     `json:"global_ties,omitempty"`

	QueryID string `json:"query_id"`
	Answer  Answer `json:"answer"`
}
type Check struct {
	NativeScoring bool     `json:"native_scoring,omitempty"`
	LocalOverlap  *float64 `json:"local_overlap_at_k,omitempty"`
	CutoffTie     bool     `json:"cutoff_tie_accepted,omitempty"`
	GlobalOverlap *float64 `json:"global_overlap_at_k,omitempty"`
	QueryID       string   `json:"query_id"`
	Operation     string   `json:"operation"`
	Status        string   `json:"status"`
	Mismatch      bool     `json:"mismatch"`
	Overlap       *float64 `json:"overlap_at_k,omitempty"`
	Detail        string   `json:"detail,omitempty"`
}

func Lexical(op string) bool {
	return strings.HasSuffix(op, "symbols") || strings.HasSuffix(op, "content")
}
func Ranked(op string) bool {
	return strings.Contains(op, "symbols") || strings.Contains(op, "content") || strings.Contains(op, "vector")
}
func Canonical(a Answer) Answer {
	a.Nodes = slices.Clone(a.Nodes)
	a.IDs = slices.Clone(a.IDs)
	slices.SortFunc(a.Nodes, func(x, y dataset.Node) int { return strings.Compare(x.ID, y.ID) })
	slices.Sort(a.IDs)
	// Precompute each full edge key once, never JSON-marshal in a sort comparator.
	type keyed struct {
		value dataset.Edge
		key   string
	}
	keys := make([]keyed, len(a.Edges))
	for i, v := range a.Edges {
		b, _ := json.Marshal(v)
		keys[i] = keyed{v, string(b)}
	}
	slices.SortFunc(keys, func(x, y keyed) int { return strings.Compare(x.key, y.key) })
	a.Edges = make([]dataset.Edge, len(keys))
	for i, v := range keys {
		a.Edges[i] = v.value
	}
	return a
}

// Validate is deliberately outside every timed call.
func Validate(q Query, a Answer) error {
	if !Ranked(q.Op) {
		return nil
	}
	if q.NativeScoring && !Lexical(q.Op) {
		return fmt.Errorf("native ranking mode requires a lexical operation")
	}
	if q.K >= 0 && len(a.Hits) > q.K {
		return fmt.Errorf("ranked answer exceeds k=%d", q.K)
	}
	seen := map[string]bool{}
	for i, h := range a.Hits {
		if h.ID == "" || seen[h.ID] || math.IsNaN(h.Score) || math.IsInf(h.Score, 0) {
			return fmt.Errorf("invalid or duplicate ranked hit %q", h.ID)
		}
		if i > 0 && h.Score > a.Hits[i-1].Score {
			return fmt.Errorf("ranked scores are not descending")
		}
		if q.NativeScoring {
			repo, ok := q.MatchRepos[h.ID]
			if !ok || (q.Repo != "" && repo != q.Repo) {
				return fmt.Errorf("native hit %q does not satisfy the query match rule", h.ID)
			}
		}
		seen[h.ID] = true
	}
	return nil
}
func Compare(q Query, want, got Answer) Check {
	c := Check{QueryID: q.ID, Operation: q.Op, Status: "passed", NativeScoring: q.NativeScoring}
	fail := func(detail string) { c.Mismatch = true; c.Status = "failed"; c.Detail = detail }
	if err := Validate(q, got); err != nil {
		fail(err.Error())
		return c
	}
	if Ranked(q.Op) {
		expected := map[string]bool{}
		for _, h := range want.Hits {
			expected[h.ID] = true
		}
		n := 0
		for _, h := range got.Hits {
			if expected[h.ID] {
				n++
			}
			if slices.Contains(q.ForbiddenIDs, h.ID) {
				fail("ranked answer contains a removed ID")
			}
		}
		gotIDs := map[string]bool{}
		for _, h := range got.Hits {
			gotIDs[h.ID] = true
		}
		for _, id := range q.RequiredIDs {
			if !gotIDs[id] {
				fail("ranked answer omits required ID " + id)
			}
		}
		den := len(want.Hits)
		v := 0.
		if den > 0 {
			v = float64(n) / float64(den)
		} else if len(got.Hits) == 0 {
			v = 1
		}
		c.Overlap = &v
		if den > 0 && len(got.Hits) == 0 {
			fail("oracle has hits but adapter returned none")
		}
		if den == 0 && len(got.Hits) > 0 {
			fail("oracle has no hits but adapter returned hits")
		}
		floor := q.MinOverlap
		gateOverlap := v
		if q.ExactRank {
			floor = 1
			if len(got.Hits) != len(want.Hits) {
				fail("exact ranked result has a different hit count")
			} else if v < 1 && exactCutoffTie(q, want, got) {
				c.CutoffTie = true
				gateOverlap = 1
			}
		}
		if gateOverlap < floor {
			fail(fmt.Sprintf("overlap %.6f below required %.6f", v, floor))
		}
		return c
	}
	a, _ := json.Marshal(Canonical(want))
	b, _ := json.Marshal(Canonical(got))
	if string(a) != string(b) {
		fail(fmt.Sprintf("exact answer differs (expected %d bytes, got %d)", len(a), len(b)))
	}
	return c
}

// Overlap is a quality metric, independent of whether the gate accepts ties.
func Overlap(want, got []adapter.Hit) float64 {
	if len(want) == 0 {
		if len(got) == 0 {
			return 1
		}
		return 0
	}
	ids := map[string]bool{}
	for _, h := range want {
		ids[h.ID] = true
	}
	seen := map[string]bool{}
	n := 0
	for _, h := range got {
		if ids[h.ID] && !seen[h.ID] {
			n++
		}
		seen[h.ID] = true
	}
	return float64(n) / float64(len(want))
}
func exactCutoffTie(q Query, want, got Answer) bool {
	if len(want.Hits) == 0 || len(want.Hits) != q.K || len(got.Hits) != len(want.Hits) {
		return false
	}
	cutoff := want.Hits[len(want.Hits)-1].Score
	trusted := map[string]float64{}
	for _, h := range q.CutoffTies {
		trusted[h.ID] = h.Score
	}
	wantIDs, gotIDs := map[string]bool{}, map[string]bool{}
	for _, h := range want.Hits {
		wantIDs[h.ID] = true
	}
	for _, h := range got.Hits {
		gotIDs[h.ID] = true
	}
	for _, h := range want.Hits {
		if !gotIDs[h.ID] && !NearCutoff(h.Score, cutoff) {
			return false
		}
	}
	for _, h := range got.Hits {
		if !wantIDs[h.ID] {
			score, ok := trusted[h.ID]
			if !ok || !NearCutoff(score, cutoff) {
				return false
			}
		}
	}
	return true
}

type incoming interface {
	InEdges(context.Context, string, string) ([]dataset.Edge, error)
}

func Traverse(ctx context.Context, s incoming, id string, depth int) ([]string, error) {
	seen := map[string]bool{id: true}
	front := []string{id}
	var result []string
	for hop := 0; hop < depth; hop++ {
		var next []string
		for _, v := range front {
			edges, e := s.InEdges(ctx, v, "calls")
			if e != nil {
				return nil, e
			}
			var callers []string
			for _, edge := range edges {
				callers = append(callers, edge.From)
			}
			slices.Sort(callers)
			callers = slices.Compact(callers)
			if len(callers) > FanoutCap {
				callers = callers[:FanoutCap]
			}
			for _, n := range callers {
				if !seen[n] {
					seen[n] = true
					result = append(result, n)
					next = append(next, n)
					if len(result) >= VisitCap {
						slices.Sort(result)
						return result, nil
					}
				}
			}
		}
		front = next
	}
	slices.Sort(result)
	return result, nil
}

// Evaluate performs raw adapter calls only. Call Validate/Compare after stopping
// the timer. BFS bookkeeping remains timed for explicitly emulated traversal.
func Evaluate(ctx context.Context, segments map[string]adapter.Segment, m adapter.MultiRepo, q Query) (Answer, error) {
	var a Answer
	var e error
	s := segments[q.Repo]
	switch q.Op {
	case "id":
		var n dataset.Node
		n, e = s.NodeByID(ctx, q.Key)
		if errors.Is(e, adapter.ErrNotFound) {
			a.Missing = true
			e = nil
		} else if e == nil {
			a.Node = &n
		}
	case "name":
		a.Nodes, e = s.NodesByName(ctx, q.Key, q.K)
	case "file":
		a.Nodes, e = s.FileNodes(ctx, q.Key)
	case "out":
		a.Edges, e = s.OutEdges(ctx, q.Key, q.Kind)
	case "in":
		a.Edges, e = s.InEdges(ctx, q.Key, q.Kind)
	case "traverse2", "traverse3":
		a.IDs, e = Traverse(ctx, s, q.Key, q.Depth)
	case "native_traverse2", "native_traverse3":
		if n, ok := s.(adapter.NativeTraverser); ok {
			a.IDs, e = n.NativeTraverse(ctx, q.Key, q.Depth, FanoutCap, VisitCap)
		} else {
			e = adapter.ErrUnsupported
		}
	case "symbols":
		a.Hits, e = s.SearchSymbols(ctx, q.Key, q.K)
	case "content":
		a.Hits, e = s.SearchContent(ctx, q.Key, q.K)
	case "vector":
		a.Hits, e = s.VectorTopK(ctx, q.Vec, q.K)
	case "multi_name", "native_multi_name":
		a.Nodes, e = m.NodesByName(ctx, q.Key, q.K)
	case "multi_in", "native_multi_in":
		a.Edges, e = m.InEdges(ctx, q.Key, q.Kind)
	case "multi_symbols", "native_multi_symbols":
		a.Hits, e = m.SearchSymbols(ctx, q.Key, q.K)
	case "multi_vector", "native_multi_vector":
		a.Hits, e = m.VectorTopK(ctx, q.Vec, q.K)
	case "multi_traverse2", "multi_traverse3":
		a.IDs, e = Traverse(ctx, m, q.Key, q.Depth)
	case "native_multi_traverse2", "native_multi_traverse3":
		if n, ok := m.(adapter.NativeTraverser); ok {
			a.IDs, e = n.NativeTraverse(ctx, q.Key, q.Depth, FanoutCap, VisitCap)
		} else {
			e = adapter.ErrUnsupported
		}
	default:
		e = fmt.Errorf("unknown operation %q", q.Op)
	}
	return a, e
}
func hashOrder(keys []string, seed int64) []string {
	keys = slices.Clone(keys)
	slices.SortFunc(keys, func(a, b string) int {
		ha := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", seed, a)))
		hb := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", seed, b)))
		return strings.Compare(string(ha[:]), string(hb[:]))
	})
	return keys
}

// stratified picks distinct IDs from degree quartiles plus the largest hub.
func stratified(degrees map[string]int, seed int64, n int) []string {
	keys := make([]string, 0, len(degrees))
	for id := range degrees {
		keys = append(keys, id)
	}
	slices.SortFunc(keys, func(a, b string) int {
		if degrees[a] != degrees[b] {
			return degrees[a] - degrees[b]
		}
		return strings.Compare(a, b)
	})
	var out []string
	for quart := 0; quart < 4; quart++ {
		part := hashOrder(keys[len(keys)*quart/4:len(keys)*(quart+1)/4], seed+int64(quart))
		out = append(out, part[:min(len(part), n/4)]...)
	}
	if len(keys) > 0 && !slices.Contains(out, keys[len(keys)-1]) {
		out = append(out, keys[len(keys)-1])
	}
	return out
}
func Generate(ds []dataset.Dataset, seed int64) []Query {
	if len(ds) == 0 {
		return nil
	}
	d := ds[0]
	var qs []Query
	seen := map[string]bool{}
	add := func(q Query) {
		if !strings.HasPrefix(q.Op, "multi_") {
			q.Repo = d.Repo
		}
		key := fmt.Sprintf("%s|%s|%s|%s", q.Op, q.Repo, q.Kind, q.Key)
		if !strings.Contains(q.Op, "vector") && seen[key] {
			return
		}
		seen[key] = true
		q.ID = fmt.Sprintf("q%04d", len(qs))
		qs = append(qs, q)
	}
	nodes := map[string]dataset.Node{}
	var ids []string
	for _, n := range d.Nodes {
		nodes[n.ID] = n
		ids = append(ids, n.ID)
	}
	for _, id := range hashOrder(ids, seed)[:min(24, len(ids))] {
		add(Query{Op: "id", Key: id})
		add(Query{Op: "name", Key: nodes[id].Name, K: 100})
	}
	add(Query{Op: "id", Key: "__missing_node__"})
	add(Query{Op: "name", Key: "__missing_name__", K: 100})
	var paths []string
	for _, f := range d.Files {
		paths = append(paths, f.Path)
	}
	for _, p := range hashOrder(paths, seed+1)[:min(12, len(paths))] {
		add(Query{Op: "file", Key: p})
	}
	add(Query{Op: "file", Key: "__missing_file__"})
	counts := map[string]int{}
	for _, e := range d.Edges {
		counts[e.Kind]++
	}
	kinds := []string{"any"}
	for kind, n := range counts {
		if n >= 100 || kind == "calls" || kind == "references" {
			kinds = append(kinds, kind)
		}
	}
	slices.Sort(kinds)
	for _, kind := range kinds {
		out, in := map[string]int{}, map[string]int{}
		for _, e := range d.Edges {
			if kind == "any" || e.Kind == kind {
				out[e.From]++
				in[e.To]++
			}
		}
		for _, id := range stratified(out, seed, 8) {
			add(Query{Op: "out", Key: id, Kind: kind})
		}
		for _, id := range stratified(in, seed+7, 8) {
			add(Query{Op: "in", Key: id, Kind: kind})
		}
		if kind == "calls" {
			for _, id := range stratified(in, seed+19, 12) {
				add(Query{Op: "traverse2", Key: id, Depth: 2})
				add(Query{Op: "traverse3", Key: id, Depth: 3})
			}
		}
	}
	add(Query{Op: "in", Key: "__missing_node__", Kind: "any"})
	add(Query{Op: "out", Key: "__missing_node__", Kind: "any"})
	for _, text := range []string{"store", "graph node", "index", "function call", "repository", "segment", "resolve", "language", "reference", "symbol", "publish", "__absent_term_97a__"} {
		add(Query{Op: "symbols", Key: text, K: 10})
		add(Query{Op: "content", Key: text, K: 10})
	}
	for i := 0; i < 12; i++ {
		add(Query{Op: "vector", Key: fmt.Sprint(i), Vec: dataset.MakeVector(fmt.Sprintf("heldout-query:%d:%d", seed, i), seed).Values, K: 10})
	}
	if len(ds) > 1 {
		names := map[string]map[string]int{}
		for _, repo := range ds {
			for _, n := range repo.Nodes {
				if names[n.Name] == nil {
					names[n.Name] = map[string]int{}
				}
				names[n.Name][repo.Repo]++
			}
		}
		var choices []string
		totals := map[string]int{}
		for name, repos := range names {
			total := 0
			for _, n := range repos {
				total += n
			}
			totals[name] = total
			if len(repos) >= 2 && total <= 2000 {
				choices = append(choices, name)
			}
		}
		choices = hashOrder(choices, seed+50)
		slices.SortStableFunc(choices, func(a, b string) int { return len(names[b]) - len(names[a]) })
		for _, name := range choices[:min(12, len(choices))] {
			add(Query{Op: "multi_name", Key: name, K: totals[name]})
		}
		for _, text := range []string{"store", "graph", "index", "repository", "symbol", "publish"} {
			add(Query{Op: "multi_symbols", Key: text, K: 10})
		}
		for i := 0; i < 12; i++ {
			add(Query{Op: "multi_vector", Key: fmt.Sprint(i), Vec: dataset.MakeVector(fmt.Sprintf("heldout-multi:%d:%d", seed, i), seed).Values, K: 10})
		}
		for i, repo := range ds {
			degree := map[string]int{}
			for _, e := range repo.Edges {
				if e.CrossRepo && e.Kind == "calls" {
					degree[e.To]++
				}
			}
			targets := stratified(degree, seed+int64(i), 4)
			for _, id := range targets {
				add(Query{Op: "multi_in", Key: id, Kind: "calls"})
				if i < 8 {
					add(Query{Op: "multi_traverse2", Key: id, Depth: 2})
					add(Query{Op: "multi_traverse3", Key: id, Depth: 3})
				}
			}
		}
	}
	return qs
}

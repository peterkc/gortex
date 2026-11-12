package indexer

import (
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// The resolver's provides index (DI `provides_for` bindings) is built from
// every EdgeProvides row of the graph, bucketed by the repository of its
// source node. Through a delta that is a composed scan of the kind over the
// whole store — every tracked repository — plus a placement read of every
// source: seconds per save on a large store, and the same answer on every
// delta over the same stack.
//
// Over a dirty chain, the cache keeps the provides edges below the chain,
// including sources not placed there. Each delta composes the edges above
// that stack and places their sources through its own view: a chain layer
// can restate a source outside the paths it covers. Without a chain, the
// stack's placed rows are kept and each delta replaces its changed paths.

type providesRow struct {
	edge *graph.Edge
	repo string
	file string // the source node's file
}

type editDeltaProvidesEntry struct {
	key   string
	rows  []providesRow // no-chain rows, also populated by pre-warming
	edges []*graph.Edge // unplaced below-chain edges, under a separate key
}

var editDeltaProvidesCache struct {
	sync.Mutex
	entries []editDeltaProvidesEntry // most recent last
}

func cachedEditDeltaProvides(key string) (editDeltaProvidesEntry, bool) {
	editDeltaProvidesCache.Lock()
	defer editDeltaProvidesCache.Unlock()
	for i := len(editDeltaProvidesCache.entries) - 1; i >= 0; i-- {
		entry := editDeltaProvidesCache.entries[i]
		if entry.key != key {
			continue
		}
		editDeltaProvidesCache.entries = append(append(editDeltaProvidesCache.entries[:i:i], editDeltaProvidesCache.entries[i+1:]...), entry)
		return entry, true
	}
	return editDeltaProvidesEntry{}, false
}

func storeEditDeltaProvides(key string, rows []providesRow) {
	storeEditDeltaProvidesEntry(editDeltaProvidesEntry{key: key, rows: rows})
}

func storeEditDeltaProvidesEntry(entry editDeltaProvidesEntry) {
	editDeltaProvidesCache.Lock()
	defer editDeltaProvidesCache.Unlock()
	editDeltaProvidesCache.entries = append(editDeltaProvidesCache.entries, entry)
	if over := len(editDeltaProvidesCache.entries) - editDeltaContractCacheEntries; over > 0 {
		editDeltaProvidesCache.entries = append([]editDeltaProvidesEntry(nil), editDeltaProvidesCache.entries[over:]...)
	}
}

// resetEditDeltaProvides empties the cache (tests).
func resetEditDeltaProvides() {
	editDeltaProvidesCache.Lock()
	editDeltaProvidesCache.entries = nil
	editDeltaProvidesCache.Unlock()
}

// editDeltaProvides is one delta's provides source.
type editDeltaProvides struct {
	view    graph.Store  // the delta's view
	stack   graph.Reader // the immutable stack the rows are kept for
	key     string
	changed []string // graph paths the delta's view answers for itself
	logger  *zap.Logger
	cached  bool
	calls   int
}

// installEditDeltaProvides sets the per-stack provides source on idx's resolver.
func installEditDeltaProvides(idx *Indexer, stack graph.Reader, key string, changedRel []string) *editDeltaProvides {
	if idx == nil || idx.resolver == nil || key == "" || stack == nil {
		return nil
	}
	changed := make([]string, 0, len(changedRel))
	for _, rel := range changedRel {
		changed = append(changed, idx.prefixPath(rel))
	}
	o := &editDeltaProvides{view: idx.graph, stack: stack, key: key, changed: changed, logger: idx.logger}
	idx.resolver.SetProvidesRowsSource(o.rows)
	return o
}

// providesRowsOf reads the provides rows sourced in paths from r.
func providesRowsOf(r graph.Reader, paths []string) []providesRow {
	var ids []string
	placed := make(map[string]*graph.Node)
	for _, p := range paths {
		for _, n := range r.GetFileNodes(p) {
			if n != nil && n.ID != "" {
				ids = append(ids, n.ID)
				placed[n.ID] = n
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Strings(ids)
	var rows []providesRow
	out := r.GetOutEdgesByNodeIDs(ids)
	for _, id := range ids {
		for _, e := range out[id] {
			if e != nil && e.Kind == graph.EdgeProvides {
				rows = append(rows, providesRow{edge: e, repo: placed[id].RepoPrefix, file: placed[id].FilePath})
			}
		}
	}
	return rows
}

// scanProvidesRows is the composed read the resolver makes without a source:
// every provides row of s with its source's placement.
func scanProvidesRows(s graph.Store) []providesRow {
	var edges []*graph.Edge
	for e := range s.EdgesByKind(graph.EdgeProvides) {
		if e != nil {
			edges = append(edges, e)
		}
	}
	return placeProvidesRows(s, edges)
}

// placeProvidesRows applies the scan's placement rule to non-nil edges.
func placeProvidesRows(r graph.Store, edges []*graph.Edge) []providesRow {
	sources := make(map[string]struct{})
	for _, e := range edges {
		sources[e.From] = struct{}{}
	}
	ids := make([]string, 0, len(sources))
	for id := range sources {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	placements := graph.NodePlacementsByIDs(r, ids)
	rows := make([]providesRow, 0, len(edges))
	for _, e := range edges {
		placement, ok := placements[e.From]
		if !ok || placement.RepoPrefix == "" {
			continue
		}
		rows = append(rows, providesRow{edge: e, repo: placement.RepoPrefix, file: placement.FilePath})
	}
	return rows
}

func (o *editDeltaProvides) rows() map[string][]*graph.Edge {
	started := time.Now()
	o.calls++
	if dw, ok := o.view.(*graph.DeltaWriter); ok && dw.ChainLayers() > 0 {
		return o.chainRows(dw, started)
	}
	changed := make(map[string]struct{}, len(o.changed))
	for _, p := range o.changed {
		changed[p] = struct{}{}
	}
	_, hit := cachedEditDeltaProvides(o.key)
	// A pre-warm reading the same rows is waited for, not repeated.
	dw, _ := o.view.(*graph.DeltaWriter)
	singleFlight("provides\x00"+o.key, func() bool { _, ok := cachedEditDeltaProvides(o.key); return ok }, func() {
		// The stack's own answer, kept for the next delta: read below every
		// layer the rows are not kept for when the delta can split its view,
		// otherwise the view's rows with the change set's rows as the stack
		// holds them.
		if rows, ok := providesRowsBelowChain(dw); ok {
			storeEditDeltaProvides(o.key, rows)
			return
		}
		var rows []providesRow
		for _, row := range scanProvidesRows(o.view) {
			if _, mine := changed[row.file]; !mine {
				rows = append(rows, row)
			}
		}
		rows = append(rows, providesRowsOf(o.stack, o.changed)...)
		storeEditDeltaProvides(o.key, rows)
	})
	entry, _ := cachedEditDeltaProvides(o.key)
	stackRows := entry.rows
	o.cached = hit
	out := make(map[string][]*graph.Edge)
	for _, row := range stackRows {
		if _, mine := changed[row.file]; mine || row.repo == "" {
			continue
		}
		// A layer above the kept stack (the chain's, the delta's own) may
		// hide a row it does not re-derive: its target's identity.
		if dw != nil && !dw.EdgeVisibleAboveChain(row.edge) {
			continue
		}
		out[row.repo] = append(out[row.repo], row.edge)
	}
	for _, row := range providesRowsOf(o.view, o.changed) {
		if row.repo != "" {
			out[row.repo] = append(out[row.repo], row.edge)
		}
	}
	if o.logger != nil {
		o.logger.Info("edit delta: provides rows",
			zap.Bool("stack_cached", hit), zap.Int("stack_rows", len(stackRows)),
			zap.Int64("ms", time.Since(started).Milliseconds()))
	}
	return out
}

// chainRows composes unplaced edges, then reads each source's current placement.
func (o *editDeltaProvides) chainRows(dw *graph.DeltaWriter, started time.Time) map[string][]*graph.Edge {
	// Pre-warmed placed rows can omit sources placed only by a chain layer.
	key := "edges\x00" + o.key
	var entry editDeltaProvidesEntry
	var hit, ok bool
	if _, split := dw.ChainSplitBase(); split {
		_, hit = cachedEditDeltaProvides(key)
		singleFlight("provides\x00"+key, func() bool { _, ok := cachedEditDeltaProvides(key); return ok }, func() {
			if edges, ok := dw.EdgesByKindBelowChain(graph.EdgeProvides); ok {
				storeEditDeltaProvidesEntry(editDeltaProvidesEntry{key: key, edges: edges})
			}
		})
		entry, ok = cachedEditDeltaProvides(key)
	}
	o.cached = hit
	var rows []providesRow
	if !ok {
		// A view-level fallback must not be kept under the below-chain key.
		rows = scanProvidesRows(o.view)
	} else {
		var edges []*graph.Edge
		for _, e := range entry.edges {
			if dw.EdgeVisibleAboveChain(e) {
				edges = append(edges, e)
			}
		}
		edges = append(edges, dw.EdgesByKindAboveChain(graph.EdgeProvides)...)
		rows = placeProvidesRows(o.view, edges)
	}
	out := make(map[string][]*graph.Edge)
	for _, row := range rows {
		out[row.repo] = append(out[row.repo], row.edge)
	}
	if o.logger != nil {
		o.logger.Info("edit delta: provides rows",
			zap.Bool("stack_cached", hit), zap.Int("stack_rows", len(entry.edges)),
			zap.Int64("ms", time.Since(started).Milliseconds()))
	}
	return out
}

// providesRowsBelowChain is scanProvidesRows over the part of dw's stack its
// caches are kept for, placed there; ok is false when dw cannot split its
// view or scan that part.
func providesRowsBelowChain(dw *graph.DeltaWriter) ([]providesRow, bool) {
	if dw == nil {
		return nil, false
	}
	below, ok := dw.ChainSplitBase()
	if !ok {
		return nil, false
	}
	edges, ok := dw.EdgesByKindBelowChain(graph.EdgeProvides)
	if !ok {
		return nil, false
	}
	sources := make(map[string]struct{})
	for _, e := range edges {
		if e != nil {
			sources[e.From] = struct{}{}
		}
	}
	ids := make([]string, 0, len(sources))
	for id := range sources {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	placed := below.GetNodesByIDs(ids)
	rows := make([]providesRow, 0, len(edges))
	for _, e := range edges {
		if e == nil {
			continue
		}
		n := placed[e.From]
		if n == nil || n.RepoPrefix == "" {
			continue
		}
		rows = append(rows, providesRow{edge: e, repo: n.RepoPrefix, file: n.FilePath})
	}
	return rows, true
}

package store_pebble

import (
	"context"
	"errors"
	"slices"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

// Only the dropped marker fields used by the bridge joins are retained. These
// rows are coverage evidence; no resolver can consume them as live calls.
func (b *BuildStore) captureRebuildFrameworkHeldMarker(e *graph.Edge, key []byte, deferred []string) error {
	if !rebuildFrameworkBridgeMarker(e) {
		return nil
	}
	module, method := rebuildRNNames(e.Meta)
	projection := &graph.Edge{From: e.From, To: e.To, Kind: e.Kind, FilePath: e.FilePath, Line: e.Line,
		Meta: map[string]any{"via": "rn.native", "rn_module": module, "rn_method": method}}
	value, err := encodeEdge(projection)
	if err != nil {
		return err
	}
	for _, language := range deferred {
		if err := b.pending.Set(keyStrings(rebuildHoldoutKey('f', nil), language, string(key)), value, pebble.NoSync); err != nil {
			return err
		}
	}
	return nil
}

func rebuildFrameworkBridgeMarker(e *graph.Edge) bool {
	if e.Kind != graph.EdgeCalls || e.From == "" || e.Meta["via"] != "rn.native" {
		return false
	}
	module, method := rebuildRNNames(e.Meta)
	return module != "" && method != ""
}

// RebuildFrameworkIsolationGate reports whether the coverage-only full node
// census could admit this turn. The live frozen selection is never changed.
// Missing live gate bits are attributed to the held effective extractors that
// supply them. The all-supported path needs only two prefix probes.
func (b *BuildStore) RebuildFrameworkIsolationGate(ctx context.Context, required uint8, role func(*graph.Node) uint8) (bool, []string, error) {
	var held bool
	for _, col := range []byte{'n', 'f'} {
		found, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildHoldoutKey(col, nil))
		if err != nil {
			return false, nil, err
		}
		held = held || found
	}
	if !held {
		return false, nil, nil
	}
	var live, full uint8
	if err := b.RebuildView().VisitFrameworkGateNodes(ctx, func(n *graph.Node) error {
		live |= role(n)
		return nil
	}); err != nil {
		return false, nil, err
	}
	full = live
	var causes []string
	err := b.visitRebuildHeldNodes(ctx, func(n *graph.Node, _ string, _ []byte) error {
		bits := role(n)
		full |= bits
		if bits&required&^live == 0 {
			return nil
		}
		language, err := rebuildHeldLanguage(b.pending, n.ID)
		if err == nil && !slices.Contains(causes, language) {
			causes = append(causes, language)
		}
		return err
	})
	return full&required == required, causes, err
}

// Framework coverage scans use the same working-store scratch bounds as the
// reducers. Their only retained writes are isolation events.
type rebuildFrameworkIsolation struct {
	b        *BuildStore
	language func(string) string
	cause    string
}

func (p rebuildFrameworkIsolation) mark(ctx context.Context, file, deferred, cause, event string) error {
	file = pathkey.Normalize(file)
	language := p.language(file)
	if file == "" || !RebuildLinkedExtractor(language) {
		return nil
	}
	return p.b.MarkRebuildIsolationAffected(ctx, RebuildIsolationAffectedScope{
		FilePath: file, SupportedLanguage: language, DeferredLanguage: deferred, Cause: cause,
	}, event)
}

func (p rebuildFrameworkIsolation) markEdge(ctx context.Context, e *graph.Edge, deferred, cause string) error {
	key := pendingEdgeKey(e)
	if cause == p.cause {
		// One coarse event per affected file and deferred extractor, not per
		// speculative output key or number of held input rows.
		key = []byte(cause)
	}
	if err := p.mark(ctx, e.FilePath, deferred, cause, string(key)); err != nil {
		return err
	}
	for _, id := range []string{e.From, e.To} {
		n, err := p.b.RebuildView().Node(ctx, id)
		if err != nil {
			return err
		}
		if n != nil {
			if err := p.mark(ctx, n.FilePath, deferred, cause, string(key)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p rebuildFrameworkIsolation) fresh(e *graph.Edge) (bool, error) {
	if e == nil {
		return false, nil
	}
	found, err := pendingGet(p.b.pending, pendingEdgeKey(e), fnPointerPresent)
	return !found, err
}

func (p rebuildFrameworkIsolation) scratch(ctx context.Context, prefix []byte, run func() error) (err error) {
	busy, err := rebuildGuardHasPrefix(ctx, p.b.pending, prefix)
	if err != nil {
		return err
	}
	if busy {
		return invalid("framework isolation scratch range occupied")
	}
	defer func() { err = errors.Join(err, p.b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync)) }()
	return run()
}

// Selected and held candidate projections stay in separate namespaces. Their
// values include only the fields needed to compute a prospective output key.
func (p rebuildFrameworkIsolation) candidate(key []byte, n *graph.Node, deferred string) error {
	projected := &graph.Node{ID: n.ID, Kind: n.Kind, FilePath: n.FilePath, StartLine: n.StartLine,
		WorkspaceID: n.WorkspaceID, Language: n.Language}
	value, err := encodeNode(projected)
	if err != nil {
		return err
	}
	return p.b.putRebuildScratch(key, append(appendMetaString(nil, deferred), value...), 16<<20, p.cause)
}

func readFrameworkIsolationCandidate(r row) (*graph.Node, string, error) {
	d := recordCursor{buf: r.value}
	deferred := d.str()
	if d.err != nil {
		return nil, "", d.err
	}
	n, err := decodeNodeLight(d.buf)
	return n, deferred, err
}

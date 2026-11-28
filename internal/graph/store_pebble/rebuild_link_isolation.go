package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

// ErrRebuildLinkIsolation marks a refused private holdout or restoration.
var ErrRebuildLinkIsolation = errors.New("pebble rebuild link isolation failed")

// This private prefix is not a link election input or a serving family.
// n: elected node bytes with receipt path (empty only for a checked skip).
// e: exact parsed edge key to owning receipt path. r: saved restore edge bytes.
// l: effective extractor by held ID. m: exact name/kind/extractor presence.
// s/w: exact sanctioned native manifest replacement node/edge bytes.
// o: contract isolation affected scope omissions. p: supported first edge receipt attribution.
var rebuildHoldoutPrefix = []byte{'g', '!'}

func rebuildHoldoutKey(col byte, key []byte) []byte {
	return append([]byte{'g', '!', col}, key...)
}

func (b *BuildStore) captureRebuildHeldNode(id, receipt, language string) error {
	key := pendingNodeKey(id)
	raw, err := pendingGet(b.pending, key, copyValue)
	if err != nil {
		return err
	}
	if raw == nil {
		return invalid("held node missing from file graph")
	}
	if err := b.pending.Set(rebuildHoldoutKey('n', key), append(appendMetaString(nil, receipt), raw...), pebble.NoSync); err != nil {
		return err
	}
	return b.pending.Set(rebuildHoldoutKey('l', key), []byte(language), pebble.NoSync)
}

// AddRebuildHeldSkip keeps a checked deferred skip out of linking.
// Restore its original file node, not receipt evidence or test metadata.
func (b *BuildStore) AddRebuildHeldSkip(n *graph.Node) error {
	return b.withWrites(func() error {
		if b.rebuildLinksIsolated || n == nil || n.Kind != graph.KindFile || n.ID == "" {
			return fmt.Errorf("%w: invalid held skip", ErrRebuildLinkIsolation)
		}
		if err := b.writeNode(n); err != nil {
			return err
		}
		return b.captureRebuildHeldNode(n.ID, "", n.Language)
	})
}

func (b *BuildStore) visitRebuildHeldNodes(ctx context.Context, visit func(*graph.Node, string, []byte) error) error {
	prefix := rebuildHoldoutKey('n', nil)
	return pendingVisit(ctx, b.pending, prefix, func(r row) error {
		d := recordCursor{buf: r.value}
		receipt := d.str()
		if d.err != nil {
			return d.err
		}
		n, err := decodeNode(d.buf)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key[len(prefix):], pendingNodeKey(n.ID)) {
			return invalid("held node key differs from capture")
		}
		return visit(n, receipt, d.buf)
	})
}

// IsolateRebuildLinks opens the private cut after the all-language FileGraph
// offer. Its callers must use selected receipts and skip facts as well.
func (b *BuildStore) IsolateRebuildLinks(ctx context.Context) error {
	return b.isolateRebuildLinksForCoverage(ctx, false)
}

// IsolateRebuildLinksForCoverage opens the worker cut and records affected
// supported first-payload edges before deleting them.
func (b *BuildStore) IsolateRebuildLinksForCoverage(ctx context.Context) error {
	return b.isolateRebuildLinksForCoverage(ctx, true)
}

func (b *BuildStore) isolateRebuildLinksForCoverage(ctx context.Context, coverage bool) error {
	return b.withWrites(func() error {
		if b.rebuildLinksIsolated || b.contractOverlay {
			return fmt.Errorf("%w: private graph already in use", ErrRebuildLinkIsolation)
		}
		if err := b.isolateRebuildLinks(ctx, coverage); err != nil {
			return fmt.Errorf("%w: %w", ErrRebuildLinkIsolation, err)
		}
		proof := rebuildHoldoutKey('p', nil)
		if err := b.pending.DeleteRange(proof, prefixEnd(proof), pebble.NoSync); err != nil {
			return err
		}
		b.rebuildLinksIsolated = true
		return nil
	})
}

func (b *BuildStore) isolateRebuildLinks(ctx context.Context, coverage bool) error {
	for _, col := range []byte{'n', 'e'} {
		held, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildHoldoutKey(col, nil))
		if err != nil {
			return err
		}
		if held {
			return b.removeRebuildHeldLinks(ctx, coverage)
		}
	}
	return nil
}

func (b *BuildStore) removeRebuildHeldLinks(ctx context.Context, coverage bool) error {
	if err := b.visitRebuildHeldNodes(ctx, func(n *graph.Node, receipt string, raw []byte) error {
		match, err := pendingGet(b.pending, pendingNodeKey(n.ID), func(value []byte) (bool, error) {
			return bytes.Equal(value, raw), nil
		})
		if err != nil {
			return err
		}
		if !match {
			return fmt.Errorf("held raw node %q differs from receipt capture", n.ID)
		}
		if receipt != "" && n.Name != "" {
			language, err := rebuildHeldLanguage(b.pending, n.ID)
			if err != nil {
				return err
			}
			key := keyStrings(rebuildHoldoutKey('m', nil), n.Name, string(n.Kind), language)
			return b.pending.Set(key, nil, pebble.NoSync)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := pendingVisit(ctx, b.pending, []byte{familyEdge, edgeOut}, func(r row) error { return b.holdRebuildEdge(ctx, r, coverage) }); err != nil {
		return err
	}
	return b.visitRebuildHeldNodes(ctx, func(n *graph.Node, _ string, _ []byte) error {
		if err := b.pending.Delete(parsedConstantKey(n.ID), pebble.NoSync); err != nil {
			return err
		}
		if err := b.updateShingles(n.ID, nil, nil); err != nil {
			return err
		}
		return b.deleteNodeRecord(n)
	})
}

func (b *BuildStore) holdRebuildEdge(ctx context.Context, r row, coverage bool) error {
	e, err := decodeEdge(r.value)
	if err != nil {
		return err
	}
	receipt, err := pendingGet(b.pending, rebuildHoldoutKey('e', r.key), copyValue)
	if err != nil {
		return err
	}
	owner := func(id string) (*string, error) {
		return pendingGet(b.pending, rebuildHoldoutKey('n', pendingNodeKey(id)), func(raw []byte) (*string, error) {
			d := recordCursor{buf: raw}
			path := d.str()
			return &path, d.err
		})
	}
	from, err := owner(e.From)
	if err != nil {
		return err
	}
	to, err := owner(e.To)
	if err != nil {
		return err
	}
	if receipt == nil && from == nil && to == nil {
		return nil
	}
	if len(receipt) != 0 && from != nil && to != nil && *from == string(receipt) && *to == *from &&
		(e.Kind == graph.EdgeDefines || e.Kind == graph.EdgeContains) {
		// No parse writer attaches a move recorder or Go source proof to
		// either kind. A recorder would no longer be a raw receipt row.
		custody, err := readRebuildEdgeCustody(b.pending, r.key)
		if err != nil {
			return err
		}
		if custody.raw != nil || custody.proof != 0 {
			return invalid("held containment edge has non-raw custody")
		}
		if err := b.pending.Set(rebuildHoldoutKey('r', r.key), r.value, pebble.NoSync); err != nil {
			return err
		}
	}
	if coverage && receipt == nil && (from != nil || to != nil) {
		attribution, err := pendingGet(b.pending, rebuildHoldoutKey('p', r.key), copyValue)
		if err != nil {
			return err
		}
		if attribution == nil {
			return invalid("supported held-endpoint edge lacks receipt attribution")
		}
		d := recordCursor{buf: attribution}
		file, language := d.str(), d.str()
		if err := d.finish(); err != nil {
			return err
		}
		for _, endpoint := range []struct {
			id   string
			held *string
		}{{e.From, from}, {e.To, to}} {
			if endpoint.held == nil {
				continue
			}
			deferred, err := rebuildHeldLanguage(b.pending, endpoint.id)
			if err != nil {
				return err
			}
			if err := b.markRebuildIsolationAffected(ctx, RebuildIsolationAffectedScope{FilePath: file, SupportedLanguage: language, DeferredLanguage: deferred, Cause: "held-endpoint"}, string(r.key)); err != nil {
				return err
			}
		}
	}
	if err := b.deleteEdge(r.key, e); err != nil {
		return err
	}
	// A supported receipt may point to a held-out winner. Remove its
	// parse evidence too, so later reuse of that key cannot inherit it.
	for _, key := range [][]byte{factoryChainReceiptKey(r.key), rebuildExternalEvidenceKey('p', r.key), rebuildExternalEvidenceKey('r', r.key)} {
		if err := b.pending.Delete(key, pebble.NoSync); err != nil {
			return err
		}
	}
	return nil
}

// SelectRebuildLinkFacts removes claims on held-out winners from a fresh
// supported receipt. The caller owns the decoded slices and uses this only
// while the private cut is open. Holdout keys are immutable during the cut.
func (b *BuildStore) SelectRebuildLinkFacts(ctx context.Context, f ParseFacts) (ParseFacts, error) {
	anyHeld, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildHoldoutKey('n', nil))
	if err != nil || !anyHeld {
		return f, err
	}
	seen := make(map[string]bool)
	held := func(id string) bool {
		if dropped, ok := seen[id]; ok {
			return dropped
		}
		if err != nil {
			return false
		}
		if err = ctx.Err(); err != nil {
			return false
		}
		var dropped bool
		dropped, err = pendingGet(b.pending, rebuildHoldoutKey('n', pendingNodeKey(id)), fnPointerPresent)
		seen[id] = dropped
		return dropped
	}
	f.Nodes = slices.DeleteFunc(f.Nodes, func(n *graph.Node) bool { return held(n.ID) })
	f.Edges = slices.DeleteFunc(f.Edges, func(e *graph.Edge) bool { return held(e.From) || held(e.To) })
	f.Constants = slices.DeleteFunc(f.Constants, func(c graph.ConstantValueRow) bool { return held(c.NodeID) })
	f.Symbols = slices.DeleteFunc(f.Symbols, func(s graph.SymbolFTSItem) bool { return held(s.NodeID) })
	for id := range f.Shingles {
		if held(id) {
			delete(f.Shingles, id)
		}
	}
	if err != nil {
		return ParseFacts{}, fmt.Errorf("%w: select supported receipt %q: %w", ErrRebuildLinkIsolation, f.File.FilePath, err)
	}
	f.File.NodeCount = len(f.Nodes)
	return f, nil
}

// RestoreRebuildDeclarations closes the private cut after all resolver and C3
// input reads, before any Relations or later stage writer. Only an exact
// writer-owned manifest supersession may replace a held file.
func (b *BuildStore) RestoreRebuildDeclarations(ctx context.Context) error {
	return b.withWrites(func() error {
		if !b.rebuildLinksIsolated || b.contractOverlay {
			return fmt.Errorf("%w: private cut is not ready to restore", ErrRebuildLinkIsolation)
		}
		if err := b.restoreRebuildDeclarations(ctx); err != nil {
			return fmt.Errorf("%w: %w", ErrRebuildLinkIsolation, err)
		}
		b.rebuildLinksIsolated = false
		return nil
	})
}

func (b *BuildStore) restoreRebuildDeclarations(ctx context.Context) error {
	held, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildHoldoutPrefix)
	if err != nil || !held {
		return err
	}
	if err := b.visitRebuildHeldNodes(ctx, func(n *graph.Node, _ string, _ []byte) error {
		live, err := b.pendingNode(n.ID)
		if err != nil {
			return err
		}
		owned, err := b.rebuildManifestSupersession(n.ID)
		if err != nil {
			return err
		}
		if live != nil && !owned {
			return fmt.Errorf("isolated link node collides with held node %q", n.ID)
		}
		for _, dir := range []byte{edgeIn, edgeOut} {
			if err := pendingVisit(ctx, b.pending, keyStrings([]byte{familyEdge, dir}, n.ID), func(r row) error {
				key := r.key
				if dir == edgeIn {
					key = r.value
				}
				if owned {
					match, err := b.rebuildManifestEdgeMatches(key)
					if err != nil || match {
						return err
					}
				}
				return fmt.Errorf("isolated link edge uses held node %q", n.ID)
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	edgeProof := rebuildHoldoutKey('w', nil)
	if err := pendingVisit(ctx, b.pending, edgeProof, func(r row) error {
		match, err := b.rebuildManifestEdgeMatches(r.key[len(edgeProof):])
		if err != nil || match {
			return err
		}
		return invalid("manifest writer edge differs from capture")
	}); err != nil {
		return err
	}
	if err := b.visitRebuildHeldNodes(ctx, func(n *graph.Node, _ string, raw []byte) error {
		owned, err := b.rebuildManifestSupersession(n.ID)
		if err != nil || owned {
			return err
		}
		return b.writeNodeRecord(n, nil, raw)
	}); err != nil {
		return err
	}
	prefix := rebuildHoldoutKey('r', nil)
	if err := pendingVisit(ctx, b.pending, prefix, func(r row) error {
		e, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		return b.writeEdgeRecord(e, r.key[len(prefix):], r.value, rebuildEdgeCustody{}, false)
	}); err != nil {
		return err
	}
	return b.pending.DeleteRange(rebuildHoldoutPrefix, prefixEnd(rebuildHoldoutPrefix), pebble.NoSync)
}

// rebuildHeldLanguage reads the winning receipt's effective extractor, not
// the declaration's language tag (which can name another language).
func rebuildHeldLanguage(db pebble.Reader, id string) (string, error) {
	language, err := pendingGet(db, rebuildHoldoutKey('l', pendingNodeKey(id)), func(raw []byte) (string, error) {
		return string(raw), nil
	})
	if err != nil {
		return "", err
	}
	if language == "" {
		return "", invalid("held node has no effective extractor")
	}
	return language, nil
}

// RebuildHeldDeclaration reports raw attribution only, never a candidate
// payload. A checked skip is not a declaration and returns an empty result.
func (v RebuildView) RebuildHeldDeclaration(id string) (file, language string, kind graph.NodeKind, startLine int, err error) {
	err = v.read(context.Background(), func(snap *pebble.Snapshot) error {
		decode := func(raw []byte) (*graph.Node, error) {
			d := recordCursor{buf: raw}
			receipt := d.str()
			if d.err != nil || receipt == "" {
				return nil, d.err
			}
			return decodeNodeLight(d.buf)
		}
		n, err := pendingGet(snap, rebuildHoldoutKey('n', pendingNodeKey(id)), decode)
		if err != nil || n == nil {
			return err
		}
		if n.ID != id {
			return invalid("held node key differs from capture")
		}
		language, err = rebuildHeldLanguage(snap, id)
		if err != nil {
			return err
		}
		file, kind, startLine = n.FilePath, n.Kind, n.StartLine
		return nil
	})
	return file, language, kind, startLine, err
}

// RebuildHeldNameLanguages reports the effective extractors that may change
// an exact-name answer. These rows cannot supply values or enter election.
func (v RebuildView) RebuildHeldNameLanguages(ctx context.Context, name string, kinds ...graph.NodeKind) ([]string, error) {
	var languages []string
	prefix := keyStrings(rebuildHoldoutKey('m', nil), name)
	err := v.visit(ctx, prefix, func(_ *pebble.Snapshot, r row) error {
		kind, rest, err := readKeyString(r.key[len(prefix):])
		if err != nil {
			return err
		}
		language, rest, err := readKeyString(rest)
		if err != nil || len(rest) != 0 || kind == "" || language == "" {
			return invalid("held name attribution key")
		}
		if (len(kinds) == 0 || slices.Contains(kinds, graph.NodeKind(kind))) && !slices.Contains(languages, language) {
			languages = append(languages, language)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(languages)
	return languages, nil
}

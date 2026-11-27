package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

// ErrRebuildPlaceholderSources marks a failed private reconciliation. The
// coordinator must discard the build on any error, including Close failure.
var ErrRebuildPlaceholderSources = errors.New("pebble rebuild placeholder sources failed")

var (
	placeholderPrefix = []byte{'p', 's'}
	placeholderSites  = []byte{'p', 's', 1}
	placeholderMoves  = []byte{'p', 's', 2}
	placeholderLinks  = []byte{'p', 's', 3}
)

// RebuildPlaceholderStats counts saved census identities, claimed identities,
// distinct retarget links, and exact-refetch windows and rows submitted for
// reindex. Flushes counts closed threshold SST runs consumed by Apply, and
// SpillBytes counts their physical bytes. Overlay rows and the in-memory tail
// add none.
type RebuildPlaceholderStats struct {
	Census, Claimed, Links, Windows, Reindexes int
	Flushes                                    int
	SpillBytes                                 uint64
}

// RebuildPlaceholderSources owns one private pending range and its move runs.
// The coordinator holds at most one handle per build and serializes calls
// and graph writes.
type RebuildPlaceholderSources struct {
	build    *BuildStore
	spillAt  int
	buffered int
	runDir   string
	moves    []row
	stats    RebuildPlaceholderStats
	sites    map[string]struct{} // unclaimed exact census site keys
	applied  bool
}

// CapturePlaceholderSources captures the light census AFTER the first
// nonempty main reindex commit, before its cross-repo parallels. Record that
// same batch and each subsequent commit. Do not capture for an empty pass.
// This is the enabled cold path; the caller owns configuration policy.
// spillAt bounds buffered move key and value bytes, not total heap use.
// The pre-append check may hold up to about twice that payload while it
// spills the buffer with the next ordinary row still in hand. An oversized
// row spills on its own. The live census is separate from this budget.
func (b *BuildStore) CapturePlaceholderSources(ctx context.Context, spillAt int) (*RebuildPlaceholderSources, error) {
	if spillAt < 1 {
		return nil, fmt.Errorf("%w: invalid spill threshold", ErrRebuildPlaceholderSources)
	}
	p := &RebuildPlaceholderSources{build: b, spillAt: spillAt, sites: make(map[string]struct{})}
	prefix := []byte{familyEdge, edgeOut}
	err := b.RebuildView().visit(ctx, prefix, func(_ *pebble.Snapshot, r row) error {
		e, err := decodeEdgeLight(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key, pendingEdgeKey(e)) {
			return invalid("placeholder census edge identity does not match key")
		}
		if !graph.PlaceholderSourceKind(e.Kind) || !strings.Contains(e.From, graph.UnresolvedMarker) {
			return nil
		}
		p.stats.Census++
		site := placeholderSite(e.From, e.FilePath, e.Line)
		p.sites[string(site)] = struct{}{}
		return b.pending.Set(append(site, r.key...), nil, pebble.NoSync)
	})
	if err != nil {
		return nil, placeholderError(errors.Join(err, p.Close()))
	}
	return p, nil
}

func placeholderSite(from, file string, line int) []byte {
	return appendKeyLine(keyStrings(bytes.Clone(placeholderSites), from, file), line)
}

// Record stages the native first claimant in commit order without changing
// graph rows. One site's fanout streams in complete pending identity order.
func (p *RebuildPlaceholderSources) Record(ctx context.Context, batch []graph.EdgeReindex) error {
	if p.build == nil || p.applied {
		return fmt.Errorf("%w: record outside main pass", ErrRebuildPlaceholderSources)
	}
	return placeholderError(p.build.withWrites(func() error {
		for i, ri := range batch {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := p.link(ri); err != nil {
				return err
			}
			// At most the two source forms for this one reference.
			for _, rp := range graph.PlaceholderSourceRepoints(batch[i : i+1]) {
				if err := p.claim(ctx, rp); err != nil {
					return err
				}
			}
		}
		return ctx.Err()
	}))
}

// claim records the first repoint of this exact site in the saved census.
// The caller holds the build's write lock.
func (p *RebuildPlaceholderSources) claim(ctx context.Context, rp graph.PlaceholderRepoint) error {
	if rp.NewFrom == rp.OldFrom || rp.OldFrom == "" || rp.NewFrom == "" {
		return nil
	}
	site := placeholderSite(rp.OldFrom, rp.FilePath, rp.Line)
	// Capture is the only writer of site rows. Successful claims remove
	// the entire exact prefix; neither moves nor links add sites.
	if _, ok := p.sites[string(site)]; !ok {
		return ctx.Err()
	}
	var claimedKeys [][]byte
	if err := pendingVisit(ctx, p.build.pending, site, func(r row) error {
		claimedKeys = append(claimedKeys, bytes.Clone(r.key))
		key := binary.BigEndian.AppendUint64(bytes.Clone(placeholderMoves), uint64(p.stats.Claimed))
		p.stats.Claimed++
		return p.stageMove(ctx, append(key, r.key[len(site):]...), []byte(rp.NewFrom))
	}); err != nil {
		return err
	}
	if len(claimedKeys) > 0 {
		batch := p.build.pending.NewBatch()
		defer batch.Close()
		for _, key := range claimedKeys {
			if err := batch.Delete(key, nil); err != nil {
				return err
			}
		}
		if err := batch.Commit(pebble.NoSync); err != nil {
			return err
		}
		delete(p.sites, string(site))
	}
	return nil
}

func (p *RebuildPlaceholderSources) link(ri graph.EdgeReindex) error {
	e := ri.Edge
	if e == nil || !graph.PlaceholderSourceKind(e.Kind) || !strings.Contains(e.From, graph.UnresolvedMarker) || ri.OldTo == "" {
		return nil
	}
	old := *e
	old.To = ri.OldTo
	if ri.OldFrom != "" {
		old.From = ri.OldFrom
	}
	if ri.OldKind != "" {
		old.Kind = ri.OldKind
	}
	if ri.OldFilePath != "" {
		old.FilePath = ri.OldFilePath
	}
	if ri.OldLine != 0 {
		old.Line = ri.OldLine
	}
	before, after := pendingEdgeKey(&old), pendingEdgeKey(e)
	if bytes.Equal(before, after) {
		return nil
	}
	key := append(bytes.Clone(placeholderLinks), before...)
	known, err := pendingGet(p.build.pending, key, fnPointerPresent)
	if err != nil {
		return err
	}
	if !known {
		p.stats.Links++
	}
	return p.build.pending.Set(key, after, pebble.NoSync)
}

func (p *RebuildPlaceholderSources) currentKey(ctx context.Context, key []byte) ([]byte, error) {
	// Preserve currentIdentity's hop count even for cycles and rewritten links.
	for hops := 0; hops <= p.stats.Links; hops++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		next, err := pendingGet(p.build.pending, append(bytes.Clone(placeholderLinks), key...), copyValue)
		if err != nil || next == nil || bytes.Equal(key, next) {
			return key, err
		}
		key = next
	}
	return key, nil
}

// Apply moves sources after the last main batch, before deferred LSP and
// guard work. Count 256 claimed identities per window, including misses; the
// next window must see the graph written by this one. Always Close the pass.
func (p *RebuildPlaceholderSources) Apply(ctx context.Context) (RebuildPlaceholderStats, error) {
	if p.build == nil || p.applied {
		return RebuildPlaceholderStats{}, fmt.Errorf("%w: apply outside main pass", ErrRebuildPlaceholderSources)
	}
	p.applied = true
	err := p.build.withWrites(func() error {
		var batch []graph.EdgeReindex
		current := make(map[string]*graph.Edge)
		var positions int
		flush := func() error {
			if positions == 0 {
				return nil
			}
			p.stats.Windows++
			p.stats.Reindexes += len(batch)
			if err := p.build.reindexChunk(batch); err != nil {
				return err
			}
			clear(current)
			clear(batch)
			batch = batch[:0]
			positions = 0
			return nil
		}
		if err := p.visitMoves(ctx, func(r row) error {
			const ordinalBytes = 8
			if !bytes.HasPrefix(r.key, placeholderMoves) || len(r.key) <= len(placeholderMoves)+ordinalBytes || len(r.value) == 0 {
				return invalid("placeholder move row")
			}
			key, err := p.currentKey(ctx, r.key[len(placeholderMoves)+ordinalBytes:])
			if err != nil {
				return err
			}
			e, fetched := current[string(key)]
			if !fetched {
				e, err = p.build.pendingEdge(key)
				if err != nil {
					return err
				}
				if e != nil && !bytes.Equal(pendingEdgeKey(e), key) {
					return invalid("placeholder refetched edge identity does not match key")
				}
				current[string(key)] = e
			}
			// Native exact lookup shares a pointer for converged identities.
			// Once mutated, it no longer matches the next old identity.
			if e != nil && bytes.Equal(pendingEdgeKey(e), key) {
				batch = append(batch, graph.EdgeReindex{Edge: e, RefreshIdentity: true,
					OldFrom: e.From, OldTo: e.To, OldKind: e.Kind, OldFilePath: e.FilePath, OldLine: e.Line})
				e.From = string(r.value)
			}
			positions++
			if positions == 256 {
				return flush()
			}
			return nil
		}); err != nil {
			return err
		}
		return flush()
	})
	if err != nil {
		return RebuildPlaceholderStats{}, placeholderError(err)
	}
	return p.stats, nil
}

func (p *RebuildPlaceholderSources) stageMove(ctx context.Context, key, value []byte) error {
	size := len(key) + len(value)
	if len(p.moves) != 0 && size > p.spillAt-p.buffered {
		if err := p.spillMoves(ctx); err != nil {
			return err
		}
	}
	p.moves = append(p.moves, row{key, value})
	p.buffered += size
	if p.buffered >= p.spillAt {
		return p.spillMoves(ctx)
	}
	return nil
}

func (p *RebuildPlaceholderSources) spillMoves(ctx context.Context) error {
	if p.runDir == "" {
		dir, err := os.MkdirTemp(filepath.Join(p.build.scratchParent, "pending"), "placeholder-moves-")
		if err != nil {
			return err
		}
		p.runDir = dir
	}
	path := p.runPath(p.stats.Flushes)
	if err := p.writeMoves(ctx, path); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	p.stats.Flushes++
	p.stats.SpillBytes += uint64(info.Size())
	clear(p.moves)
	p.moves = p.moves[:0]
	p.buffered = 0
	return nil
}

func (p *RebuildPlaceholderSources) runPath(run int) string {
	return filepath.Join(p.runDir, fmt.Sprintf("%d.sst", run))
}

func (p *RebuildPlaceholderSources) writeMoves(ctx context.Context, path string) (err error) {
	w, err := newLinkTable(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, w.Close()) }()
	// Claim ordinals are unique and increasing, so this buffer is sorted.
	for _, r := range p.moves {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := w.Set(r.key, r.value); err != nil {
			return err
		}
	}
	return nil
}

func (p *RebuildPlaceholderSources) visitMoves(ctx context.Context, yield func(row) error) error {
	for i := range p.stats.Flushes {
		if err := visitLinkRun(ctx, p.runPath(i), yield); err != nil {
			return err
		}
	}
	for _, r := range p.moves {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := yield(r); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// Close releases scratch even if a main mutation or Apply failed.
func (p *RebuildPlaceholderSources) Close() error {
	if p.build == nil {
		return nil
	}
	db := p.build.pending
	p.build = nil
	p.sites = nil
	p.moves = nil
	err := db.DeleteRange(placeholderPrefix, prefixEnd(placeholderPrefix), pebble.NoSync)
	if p.runDir != "" {
		err = errors.Join(err, os.RemoveAll(p.runDir))
	}
	return placeholderError(err)
}

func placeholderError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrRebuildPlaceholderSources, err)
}

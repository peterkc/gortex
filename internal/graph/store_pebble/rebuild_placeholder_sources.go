package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
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
// reindex. SpillBytes counts L0 bytes during threshold flushes, not heap size.
type RebuildPlaceholderStats struct {
	Census, Claimed, Links, Windows, Reindexes int
	Flushes                                    int
	SpillBytes                                 uint64
}

// RebuildPlaceholderSources owns one private pending range. The coordinator
// holds at most one handle per build and serializes calls and graph writes.
type RebuildPlaceholderSources struct {
	build     *BuildStore
	spillAt   int
	unflushed int
	stats     RebuildPlaceholderStats
	sites     map[string]struct{} // unclaimed exact census site keys
	applied   bool
}

// CapturePlaceholderSources captures the light census AFTER the first
// nonempty main reindex commit, before its cross-repo parallels. Record that
// same batch and each subsequent commit. Do not capture for an empty pass.
// This is the enabled cold path; the caller owns configuration policy.
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
		return p.put(append(site, r.key...), nil)
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
	before := p.stats.Claimed
	if err := pendingVisit(ctx, p.build.pending, site, func(r row) error {
		key := binary.BigEndian.AppendUint64(bytes.Clone(placeholderMoves), uint64(p.stats.Claimed))
		p.stats.Claimed++
		return p.put(append(key, r.key[len(site):]...), []byte(rp.NewFrom))
	}); err != nil {
		return err
	}
	if p.stats.Claimed > before {
		if err := p.build.pending.DeleteRange(site, prefixEnd(site), pebble.NoSync); err != nil {
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
	return p.put(key, after)
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
		if err := pendingVisit(ctx, p.build.pending, placeholderMoves, func(r row) error {
			const ordinalBytes = 8
			if len(r.key) <= len(placeholderMoves)+ordinalBytes || len(r.value) == 0 {
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

func (p *RebuildPlaceholderSources) put(key, value []byte) error {
	db := p.build.pending
	if err := db.Set(key, value, pebble.NoSync); err != nil {
		return err
	}
	p.unflushed += len(key) + len(value)
	if p.unflushed < p.spillAt {
		return nil
	}
	before := db.Metrics().Levels[0].TableBytesFlushed
	if err := db.Flush(); err != nil {
		return err
	}
	p.stats.SpillBytes += db.Metrics().Levels[0].TableBytesFlushed - before
	p.stats.Flushes++
	p.unflushed = 0
	return nil
}

// Close releases scratch even if a main mutation or Apply failed.
func (p *RebuildPlaceholderSources) Close() error {
	if p.build == nil {
		return nil
	}
	db := p.build.pending
	p.build = nil
	p.sites = nil
	return placeholderError(db.DeleteRange(placeholderPrefix, prefixEnd(placeholderPrefix), pebble.NoSync))
}

func placeholderError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrRebuildPlaceholderSources, err)
}

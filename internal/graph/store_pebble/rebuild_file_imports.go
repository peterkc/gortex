package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

// ErrRebuildFileImports marks refusal of the private file-import settle passes.
var ErrRebuildFileImports = errors.New("pebble rebuild file imports failed")

var fileImportPrefix = []byte{'p', 'i'}

var (
	fileImportByBase   = []byte{'p', 'i', 1}
	fileImportLua      = []byte{'p', 'i', 2}
	fileImportGodot    = []byte{'p', 'i', 3}
	fileImportRelative = []byte{'p', 'i', 4}
)

// RebuildFileImportStats counts checked file rows and staged moves. SpillBytes
// measures physical L0 table output during threshold flushes of the private DB;
// it can include other dirty pages of that DB, and is not an RSS measure.
type RebuildFileImportStats struct {
	Files, RelativeMoves, LuaMoves, GodotMoves int
	Flushes                                    int
	SpillBytes                                 uint64
}

// RebuildFileImportIndex lends typed reads over the pass's basename index.
// It is valid only during the synchronous target callbacks.
type RebuildFileImportIndex struct {
	db *pebble.DB
}

// File returns the saved exact file identity and language, or a zero row on
// a genuine miss. Non-file nodes are never admitted to the index.
func (f *RebuildFileImportIndex) File(ctx context.Context, id string) (graph.FileLanguageNode, error) {
	if err := ctx.Err(); err != nil {
		return graph.FileLanguageNode{}, err
	}
	return pendingGet(f.db, keyStrings(fileImportByBase, fileImportBase(id), id), func(raw []byte) (graph.FileLanguageNode, error) {
		return graph.FileLanguageNode{ID: id, Language: string(raw)}, nil
	})
}

// VisitBasename streams one basename bucket, retaining no candidate list.
func (f *RebuildFileImportIndex) VisitBasename(ctx context.Context, base string, yield func(id string)) error {
	prefix := keyStrings(fileImportByBase, base)
	return pendingVisit(ctx, f.db, prefix, func(r row) error {
		id, rest, err := readKeyString(r.key[len(prefix):])
		if err != nil {
			return err
		}
		if len(rest) != 0 || id == "" || fileImportBase(id) != base {
			return invalid("bad file-import basename row")
		}
		yield(id)
		return nil
	})
}

// RebuildFileImports settles relative imports before collecting Lua/Godot
// candidates from the updated rows. The coordinator owns this frozen
// single-checkout view: it must not interleave graph writes or another
// invocation, and must discard the private build on any error. Callbacks
// must not mutate the input or keep the borrowed index. Relative returns a
// replacement (only target/provenance may change) or nil on a miss. Lua and
// Godot return only the target or empty on a miss.
func (b *BuildStore) RebuildFileImports(ctx context.Context, spillAt int,
	relative func(*RebuildFileImportIndex, *graph.Edge) (*graph.Edge, error),
	lua, godot func(*RebuildFileImportIndex, *graph.Edge) (string, error),
) (stats RebuildFileImportStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildFileImportStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildFileImports, err)
		}
	}()
	if spillAt < 1 {
		return stats, invalid("file-import spill threshold must be positive")
	}
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(fileImportPrefix, prefixEnd(fileImportPrefix), pebble.NoSync))
	}()
	p := fileImportPass{build: b, spillAt: spillAt}
	v := b.RebuildView()
	var hasRelative, hasLua, hasGodot bool
	if err := v.VisitNodes(ctx, func(n *graph.Node) error {
		// SQLite HasLanguage sees named nodes of any kind. Relative and
		// Lua caller selection separately require an exact file node.
		if n.Name != "" {
			switch n.Language {
			case "python", "dart", "c", "cpp", "objc", "php":
				hasRelative = true
			case "lua", "luau":
				hasLua = true
			case "godot_resource", "gdscript":
				hasGodot = true
			}
		}
		if n.Kind != graph.KindFile || n.ID == "" {
			return nil
		}
		id := n.ID
		if path.Clean(id) != id || path.IsAbs(id) || id == "." || id == ".." || strings.HasPrefix(id, "../") {
			return invalid("file-import index requires canonical checkout-relative file IDs")
		}
		p.stats.Files++
		return p.put(keyStrings(fileImportByBase, fileImportBase(id), id), []byte(n.Language))
	}); err != nil {
		return stats, err
	}
	index := RebuildFileImportIndex{db: b.pending}
	if hasRelative {
		if relative == nil {
			return stats, invalid("relative-import reducer is required")
		}
		if err := v.VisitEdges(ctx, func(e *graph.Edge) error {
			if e.Kind != graph.EdgeImports {
				return nil
			}
			moved, err := relative(&index, e)
			if err != nil || moved == nil {
				return err
			}
			p.stats.RelativeMoves++
			return p.stage(fileImportRelative, e, moved)
		}); err != nil {
			return stats, err
		}
		if p.stats.RelativeMoves != 0 {
			if err := b.withWrites(func() error { return p.apply(ctx, fileImportRelative) }); err != nil {
				return stats, err
			}
		}
	}
	if !hasLua && !hasGodot {
		return p.stats, nil
	}
	// This visit opens a fresh snapshot after all relative reindex windows.
	// As before, Lua moves are excluded from the Godot input; canonical
	// file IDs cannot create a new res:// placeholder for Godot.
	if err := v.VisitEdges(ctx, func(e *graph.Edge) error {
		if hasLua && e.Kind == graph.EdgeImports {
			target, err := lua(&index, e)
			if err != nil {
				return err
			}
			if target != "" {
				p.stats.LuaMoves++
				return p.stageResolved(fileImportLua, e, target)
			}
		}
		if !hasGodot || (e.Kind != graph.EdgeReferences && e.Kind != graph.EdgeImports) {
			return nil
		}
		target, err := godot(&index, e)
		if err != nil || target == "" {
			return err
		}
		p.stats.GodotMoves++
		// The ref/import discriminator orders one combined Godot reindex
		// stream, including when the 5,000-row boundary crosses the groups.
		order := byte(0)
		if e.Kind == graph.EdgeImports {
			order = 1
		}
		return p.stageResolved(append(bytes.Clone(fileImportGodot), order), e, target)
	}); err != nil {
		return stats, err
	}
	if p.stats.LuaMoves+p.stats.GodotMoves != 0 {
		if err := b.withWrites(func() error {
			if err := p.apply(ctx, fileImportLua); err != nil {
				return err
			}
			return p.apply(ctx, fileImportGodot)
		}); err != nil {
			return stats, err
		}
	}
	return p.stats, nil
}

type fileImportPass struct {
	build              *BuildStore
	spillAt, unflushed int
	stats              RebuildFileImportStats
}

func (p *fileImportPass) put(key, value []byte) error {
	if err := p.build.pending.Set(key, value, pebble.NoSync); err != nil {
		return err
	}
	p.unflushed += len(key) + len(value)
	if p.unflushed < p.spillAt {
		return nil
	}
	before := p.build.pending.Metrics().Levels[0].TableBytesFlushed
	if err := p.build.pending.Flush(); err != nil {
		return err
	}
	p.stats.SpillBytes += p.build.pending.Metrics().Levels[0].TableBytesFlushed - before
	p.stats.Flushes++
	p.unflushed = 0
	return nil
}

func (p *fileImportPass) stageResolved(prefix []byte, e *graph.Edge, target string) error {
	moved := *e
	moved.To, moved.Origin = target, graph.OriginASTResolved
	return p.stage(prefix, e, &moved)
}

func (p *fileImportPass) stage(prefix []byte, e, moved *graph.Edge) error {
	raw, err := encodeEdge(moved)
	if err != nil {
		return err
	}
	key := append(bytes.Clone(prefix), pendingEdgeKey(e)...)
	return p.put(key, append(appendKeyString(nil, e.To), raw...))
}

func (p *fileImportPass) apply(ctx context.Context, prefix []byte) error {
	// Pinned SQLite frees all moving identities in a 5,000-row window
	// before any claim. BuildStore.ReindexEdges instead chunks at 256.
	const nativeChunk = 5000
	var moves []graph.EdgeReindex
	if err := pendingVisit(ctx, p.build.pending, prefix, func(r row) error {
		oldTo, raw, err := readKeyString(r.value)
		if err != nil {
			return err
		}
		e, err := decodeEdge(raw)
		if err != nil {
			return err
		}
		old := *e
		old.To = oldTo
		key := r.key[len(prefix):]
		if bytes.Equal(prefix, fileImportGodot) {
			if len(key) == 0 || !(key[0] == 0 && e.Kind == graph.EdgeReferences || key[0] == 1 && e.Kind == graph.EdgeImports) {
				return invalid("bad staged Godot move group")
			}
			key = key[1:]
		}
		if !bytes.Equal(key, pendingEdgeKey(&old)) {
			return invalid("staged file-import identity does not match key")
		}
		moves = append(moves, graph.EdgeReindex{Edge: e, OldTo: oldTo})
		if len(moves) < nativeChunk {
			return nil
		}
		if err := p.build.reindexChunk(moves); err != nil {
			return err
		}
		clear(moves)
		moves = moves[:0]
		return nil
	}); err != nil {
		return err
	}
	return p.build.reindexChunk(moves)
}

func fileImportBase(id string) string { return id[strings.LastIndexByte(id, '/')+1:] }

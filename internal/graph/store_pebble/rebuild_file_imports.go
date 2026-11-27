package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
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

// RebuildFileImportStats counts checked file rows and staged moves. Flushes
// counts closed threshold SST runs consumed by the pass, and SpillBytes counts
// their physical bytes. The basename overlay and final in-memory tails add none.
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
// Lua and Godot buffer moves separately during the same scan. Together,
// their buffered key and value bytes can approach twice spillAt, excluding
// the current row and writer overhead. Oversized rows spill on their own.
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
	defer func() {
		if p.dir != "" {
			err = errors.Join(err, os.RemoveAll(p.dir))
		}
	}()
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
		return b.pending.Set(keyStrings(fileImportByBase, fileImportBase(id), id), []byte(n.Language), pebble.NoSync)
	}); err != nil {
		return stats, err
	}
	index := RebuildFileImportIndex{db: b.pending}
	if hasRelative {
		stream := fileImportMoves{prefix: fileImportRelative}
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
			return p.stage(ctx, &stream, e, moved)
		}); err != nil {
			return stats, err
		}
		if p.stats.RelativeMoves != 0 {
			if err := b.withWrites(func() error { return p.apply(ctx, &stream) }); err != nil {
				return stats, err
			}
		}
	}
	if !hasLua && !hasGodot {
		return p.stats, nil
	}
	luaMoves := fileImportMoves{prefix: fileImportLua}
	godotMoves := fileImportMoves{prefix: fileImportGodot}
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
				return p.stageResolved(ctx, &luaMoves, e, target)
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
		return p.stageResolved(ctx, &godotMoves, e, target)
	}); err != nil {
		return stats, err
	}
	if p.stats.LuaMoves+p.stats.GodotMoves != 0 {
		if err := b.withWrites(func() error {
			if err := p.apply(ctx, &luaMoves); err != nil {
				return err
			}
			return p.apply(ctx, &godotMoves)
		}); err != nil {
			return stats, err
		}
	}
	return p.stats, nil
}

type fileImportPass struct {
	build   *BuildStore
	spillAt int
	dir     string
	stats   RebuildFileImportStats
}

type fileImportMoves struct {
	prefix []byte
	rows   []row
	bytes  int
	runs   int
}

func (p *fileImportPass) stageResolved(ctx context.Context, stream *fileImportMoves, e *graph.Edge, target string) error {
	moved := *e
	moved.To, moved.Origin = target, graph.OriginASTResolved
	return p.stage(ctx, stream, e, &moved)
}

func (p *fileImportPass) stage(ctx context.Context, stream *fileImportMoves, e, moved *graph.Edge) error {
	raw, err := encodeEdge(moved)
	if err != nil {
		return err
	}
	key := bytes.Clone(stream.prefix)
	if bytes.Equal(stream.prefix, fileImportGodot) {
		// Godot applies references before imports in one combined stream.
		group := byte(0)
		if e.Kind == graph.EdgeImports {
			group = 1
		}
		key = append(key, group)
	}
	key = append(key, pendingEdgeKey(e)...)
	value := append(appendKeyString(nil, e.To), raw...)
	size := len(key) + len(value)
	if len(stream.rows) != 0 && size > p.spillAt-stream.bytes {
		if err := p.spill(ctx, stream); err != nil {
			return err
		}
	}
	stream.rows = append(stream.rows, row{key, value})
	stream.bytes += size
	if stream.bytes >= p.spillAt {
		// A single large row can spill by itself; the threshold is not a cap.
		return p.spill(ctx, stream)
	}
	return nil
}

func (p *fileImportPass) spill(ctx context.Context, stream *fileImportMoves) error {
	if p.dir == "" {
		dir, err := os.MkdirTemp(filepath.Join(p.build.scratchParent, "pending"), "file-import-")
		if err != nil {
			return err
		}
		p.dir = dir
	}
	sortFileImportMoves(stream.rows)
	path := stream.runPath(p.dir, stream.runs)
	if err := writeLinkRun(ctx, path, stream.rows); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	stream.runs++
	p.stats.Flushes++
	p.stats.SpillBytes += uint64(info.Size())
	clear(stream.rows)
	stream.rows = stream.rows[:0]
	stream.bytes = 0
	return nil
}

func (s *fileImportMoves) runPath(dir string, i int) string {
	return filepath.Join(dir, fmt.Sprintf("%d-%d.sst", s.prefix[2], i))
}

func sortFileImportMoves(rows []row) {
	slices.SortFunc(rows, func(a, b row) int { return bytes.Compare(a.key, b.key) })
}

func (s *fileImportMoves) visit(ctx context.Context, dir string, yield func(row) error) error {
	sortFileImportMoves(s.rows)
	prefixes := [][]byte{s.prefix}
	if bytes.Equal(s.prefix, fileImportGodot) {
		prefixes = [][]byte{append(bytes.Clone(s.prefix), 0), append(bytes.Clone(s.prefix), 1)}
	}
	// Each stream gets at most one row per old key from VisitEdges, which
	// visits unique old keys in key order. A Godot kind is a subsequence of
	// that order. For each prefix, concatenate successive runs and the tail;
	// no heap merge or simultaneous readers are needed.
	for _, prefix := range prefixes {
		for i := range s.runs {
			if err := visitLinkRun(ctx, s.runPath(dir, i), prefix, yield); err != nil {
				return err
			}
		}
		for _, r := range s.rows {
			if err := ctx.Err(); err != nil {
				return err
			}
			if bytes.HasPrefix(r.key, prefix) {
				if err := yield(r); err != nil {
					return err
				}
			}
		}
	}
	return ctx.Err()
}

func (p *fileImportPass) apply(ctx context.Context, stream *fileImportMoves) error {
	prefix := stream.prefix
	// Pinned SQLite frees all moving identities in a 5,000-row window
	// before any claim. BuildStore.ReindexEdges instead chunks at 256.
	const nativeChunk = graph.SQLiteReindexChunkSize
	var moves []graph.EdgeReindex
	if err := stream.visit(ctx, p.dir, func(r row) error {
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
			if len(key) == 0 || ((key[0] != 0 || e.Kind != graph.EdgeReferences) && (key[0] != 1 || e.Kind != graph.EdgeImports)) {
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
		if err := p.install(ctx, stream, moves); err != nil {
			return err
		}
		clear(moves)
		moves = moves[:0]
		return nil
	}); err != nil {
		return err
	}
	return p.install(ctx, stream, moves)
}

func (p *fileImportPass) install(ctx context.Context, stream *fileImportMoves, moves []graph.EdgeReindex) error {
	if !bytes.Equal(stream.prefix, fileImportRelative) {
		return p.build.reindexChunk(moves)
	}
	// The relative pass alone can downgrade an unbound pyrel marker to
	// a node-free external target. Carry the actual installed raw key.
	effective := make(map[*graph.Edge]string)
	for _, move := range moves {
		if stem, ok := strings.CutPrefix(move.OldTo, "unresolved::pyrel::"); ok && move.Edge.To == "external::"+stem {
			effective[move.Edge] = stem
		}
	}
	if len(effective) == 0 {
		return p.build.reindexChunk(moves)
	}
	if err := p.build.reindexChunkWithRecorders(moves, true); err != nil {
		return err
	}
	return p.build.recordRebuildExternalMoves(ctx, moves, effective)
}

func fileImportBase(id string) string { return id[strings.LastIndexByte(id, '/')+1:] }

package store_pebble

import (
	"context"
	"errors"
	"fmt"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

// ErrRebuildReexports marks refusal of the private import closure. A caller
// must abort the build rather than treat a failed lookup as unreachable.
var ErrRebuildReexports = errors.New("pebble rebuild import closure failed")

// The entire column is private to this pass. Published edge indexes never
// scan it. Complete tuple keys use the same codec as the other edge columns.
const rebuildReexportColumn byte = 'x'

const (
	reexportPlacement byte = iota + 1
	reexportImport
	reexportHop
	reexportSeen
	reexportFrontier
	reexportNextFrontier
	reexportDirectory
)

// RebuildReexportStats describes successful closure construction. ScratchBytes
// counts logical key/value bytes submitted, not memory use. SpillBytes counts
// physical bytes of closed threshold SST runs consumed by this pass. It is
// zero because this pass uses only the pending overlay.
type RebuildReexportStats struct {
	ScratchBytes uint64
	SpillBytes   uint64
	Rounds       int
	Directories  int
}

// RebuildImportClosure is a read-only result borrowed during WithImportClosure.
// The caller must join all uses before returning from its callback.
type RebuildImportClosure struct {
	db *pebble.DB
}

// Reachable reports exact membership; a true miss is (false, nil).
func (c *RebuildImportClosure) Reachable(ctx context.Context, callerFile, targetDir string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("%w: %w", ErrRebuildReexports, err)
	}
	if c.db == nil {
		return false, fmt.Errorf("%w: result outside its callback", ErrRebuildReexports)
	}
	found, err := hasReexportKey(c.db, reexportKey(reexportDirectory, callerFile, targetDir))
	if err != nil {
		return false, fmt.Errorf("%w: read reachable directory: %w", ErrRebuildReexports, err)
	}
	return found, nil
}

// Visit streams the completed result in (caller file, target directory) order.
func (c *RebuildImportClosure) Visit(ctx context.Context, yield func(callerFile, targetDir string) error) error {
	if c.db == nil {
		return fmt.Errorf("%w: result outside its callback", ErrRebuildReexports)
	}
	if err := visitReexportPairs(ctx, c.db, reexportDirectory, yield); err != nil {
		return fmt.Errorf("%w: visit reachable directories: %w", ErrRebuildReexports, err)
	}
	return nil
}

// WithImportClosure streams the saved post-resolution facts into an exact
// closure. The worker must finish/join prior graph writes before entering and
// keep graph facts frozen until consume. Consume runs only after convergence;
// it may read/use the result synchronously, but must abort the build on any
// failure, including a cleanup failure after the callback. Only the worker
// that owns the private BuildStore may call this method.
//
// A nil repos set uses all callers. A non-nil set restricts caller seeds and
// import sources (not re-exports). skipTarget and directory are the native
// resolver's placeholder and path rules. spillAt must be positive. Pebble's
// usual memtable/cache limits apply; there is no app-side batch of graph rows.
func (v RebuildView) WithImportClosure(
	ctx context.Context, repos map[string]struct{}, spillAt int,
	skipTarget func(string) bool, directory func(string) string,
	consume func(*RebuildImportClosure) error,
) (stats RebuildReexportStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildReexportStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildReexports, err)
		}
	}()
	if v.build == nil || spillAt <= 0 || skipTarget == nil || directory == nil || consume == nil {
		return stats, errors.New("invalid import closure input")
	}
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	if err := v.mutationError(); err != nil {
		return stats, err
	}
	db := v.build.pending
	prefix := []byte{familyEdge, rebuildReexportColumn}
	if err := v.beginImportClosure(prefix); err != nil {
		return stats, err
	}
	result := &RebuildImportClosure{db: db}
	defer func() {
		result.db = nil
		err = errors.Join(err, db.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
	}()

	pass := reexportPass{db: db, directory: directory}
	if err := pass.load(ctx, v, repos, skipTarget); err != nil {
		return stats, fmt.Errorf("load placements and import relations: %w", err)
	}
	if err := pass.walk(ctx); err != nil {
		return stats, fmt.Errorf("expand re-export frontier: %w", err)
	}
	return pass.stats, errors.Join(consume(result), ctx.Err())
}

func (v RebuildView) beginImportClosure(prefix []byte) error {
	v.build.mu.Lock()
	defer v.build.mu.Unlock()
	db := v.build.pending
	occupied, err := hasReexportKey(db, prefix)
	if err != nil {
		return err
	}
	if occupied {
		return errors.New("import closure scratch already owned")
	}
	return db.Set(prefix, nil, pebble.NoSync)
}

// The only pass-local retained state is counters, scalar options, a single
// decoded fact or tuple per active cursor, and a constant-size write batch.
// Each frontier/adjacency scan holds a Pebble iterator; joins never collect
// an entire adjacency, seen set, or layer in Go memory.
type reexportPass struct {
	db        *pebble.DB
	directory func(string) string
	stats     RebuildReexportStats
}

func (p *reexportPass) load(ctx context.Context, v RebuildView, repos map[string]struct{}, skipTarget func(string) bool) error {
	inScope := func(repo string) bool {
		_, included := repos[repo]
		return repos == nil || included
	}
	if err := v.VisitNodes(ctx, func(n *graph.Node) error {
		if n.FilePath == "" {
			return nil
		}
		// Empty endpoint IDs are omitted from the native placement lookup.
		if n.ID != "" {
			if err := p.set(reexportKey(reexportPlacement, n.ID), []byte(n.FilePath)); err != nil {
				return err
			}
		}
		if n.Kind == graph.KindFile && inScope(n.RepoPrefix) && inScope(graph.RepoPrefixOfID(n.ID)) {
			return p.addDirectory(n.FilePath, p.directory(n.FilePath))
		}
		return nil
	}); err != nil {
		return err
	}
	return v.VisitEdges(ctx, func(e *graph.Edge) error {
		column := reexportHop
		switch e.Kind {
		case graph.EdgeImports:
			if !inScope(graph.RepoPrefixOfID(e.From)) {
				return nil
			}
			column = reexportImport
		case graph.EdgeReExports:
			// Full/scoped native assembly does not filter re-export sources.
		default:
			return nil
		}
		if skipTarget(e.To) {
			return nil
		}
		to, err := p.placement(e.To)
		if err != nil || to == "" {
			return err
		}
		from, err := p.placement(e.From)
		if err != nil {
			return err
		}
		if from == "" {
			from = e.FilePath
		}
		if from == "" {
			return nil
		}
		return p.set(reexportKey(column, from, to), nil)
	})
}

func (p *reexportPass) placement(id string) (string, error) {
	return pendingGet(p.db, reexportKey(reexportPlacement, id), func(raw []byte) (string, error) {
		return string(raw), nil
	})
}

func (p *reexportPass) walk(ctx context.Context) error {
	current, next := reexportFrontier, reexportNextFrontier
	if err := visitReexportPairs(ctx, p.db, reexportImport, func(caller, target string) error {
		return p.enqueue(current, caller, target)
	}); err != nil {
		return err
	}
	for {
		processed := false
		if err := visitReexportPairs(ctx, p.db, current, func(caller, file string) error {
			processed = true
			if err := p.addDirectory(caller, p.directory(file)); err != nil {
				return err
			}
			// The current layer never joins ordinary imports. Each newly seen
			// (caller, file) is scheduled once, even in cycles or diamonds.
			prefix := reexportKey(reexportHop, file)
			return pendingVisit(ctx, p.db, prefix, func(r row) error {
				target, rest, err := readKeyString(r.key[len(prefix):])
				if err != nil || target == "" || len(rest) != 0 {
					return invalid("re-export target file key")
				}
				return p.enqueue(next, caller, target)
			})
		}); err != nil {
			return err
		}
		if !processed {
			return nil
		}
		p.stats.Rounds++
		prefix := reexportKey(current)
		if err := p.db.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync); err != nil {
			return err
		}
		current, next = next, current
	}
}

func (p *reexportPass) enqueue(column byte, caller, target string) (err error) {
	seen := reexportKey(reexportSeen, caller, target)
	found, err := hasReexportKey(p.db, seen)
	if err != nil || found {
		return err
	}
	key := reexportKey(column, caller, target)
	batch := p.db.NewBatch()
	defer func() { err = errors.Join(err, batch.Close()) }()
	if err := batch.Set(seen, nil, nil); err != nil {
		return err
	}
	if err := batch.Set(key, nil, nil); err != nil {
		return err
	}
	if err := batch.Commit(pebble.NoSync); err != nil {
		return err
	}
	p.stats.ScratchBytes += uint64(len(seen) + len(key))
	return nil
}

func (p *reexportPass) addDirectory(caller, dir string) error {
	if dir == "" {
		return nil
	}
	key := reexportKey(reexportDirectory, caller, dir)
	found, err := hasReexportKey(p.db, key)
	if err != nil || found {
		return err
	}
	p.stats.Directories++
	return p.set(key, nil)
}

func (p *reexportPass) set(key, value []byte) error {
	if err := p.db.Set(key, value, pebble.NoSync); err != nil {
		return err
	}
	p.stats.ScratchBytes += uint64(len(key) + len(value))
	return nil
}

func hasReexportKey(db *pebble.DB, key []byte) (bool, error) {
	return pendingGet(db, key, func([]byte) (bool, error) { return true, nil })
}

func reexportKey(column byte, fields ...string) []byte {
	return keyStrings([]byte{familyEdge, rebuildReexportColumn, column}, fields...)
}

func visitReexportPairs(ctx context.Context, db *pebble.DB, column byte, visit func(string, string) error) error {
	prefix := reexportKey(column)
	return pendingVisit(ctx, db, prefix, func(r row) error {
		from, rest, err := readKeyString(r.key[len(prefix):])
		if err != nil || from == "" {
			return invalid("import closure key source")
		}
		to, rest, err := readKeyString(rest)
		if err != nil || to == "" || len(rest) != 0 {
			return invalid("import closure key target")
		}
		return visit(from, to)
	})
}

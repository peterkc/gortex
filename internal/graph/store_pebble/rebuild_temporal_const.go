package store_pebble

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

// ErrRebuildTemporalConst marks a failed worker lookup or scratch operation.
var ErrRebuildTemporalConst = errors.New("pebble rebuild Temporal constants failed")

var temporalConstPrefix = []byte{'p', 't'} // Private pending rows only.

const (
	temporalConstName byte = iota + 1
	temporalConstAlias
	temporalConstValue
	temporalConstAmbiguous
)

const temporalConstPassLimit = 8

// TemporalConstCandidate is the resolver's projection of one exact-name node.
// ReadSidecar selects that node ID, not its file or any other node of the same name.
type TemporalConstCandidate struct {
	ReadSidecar bool
	Alias       string
	JavaLiteral string
}

// TemporalConstStats counts names visited and rounds that discovered a name or
// installed a value. Each round count can reach eight. ScratchBytes counts
// logical key/value bytes submitted.
type TemporalConstStats struct {
	Names           int
	DiscoveryPasses int
	CollapsePasses  int
	ScratchBytes    uint64
}

// TemporalConstDeref is borrowed for the duration of the result callback.
type TemporalConstDeref struct {
	db *pebble.DB
}

// Value returns the saved dereference by exact name. An empty string with no
// error is a genuine miss (including an unresolved cycle or conflict).
func (r *TemporalConstDeref) Value(ctx context.Context, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("%w: %w", ErrRebuildTemporalConst, err)
	}
	if r.db == nil {
		return "", fmt.Errorf("%w: result outside its callback", ErrRebuildTemporalConst)
	}
	value, err := readTemporalConstValue(r.db, name)
	if err != nil {
		return "", fmt.Errorf("%w: read value: %w", ErrRebuildTemporalConst, err)
	}
	return value, nil
}

// WithTemporalConstDeref keeps the name set, aliases and literal state in the
// private working DB. The owner freezes graph facts through construction and
// joins all result reads before consume returns. Seed must call yield serially
// and propagate its error. No callback runs on partial construction; any error
// including cleanup after consume requires the worker to abort. Errors from
// consume retain their own cause; Value marks its constant lookup failures.
func (v RebuildView) WithTemporalConstDeref(
	ctx context.Context,
	seed func(yield func(string) error) error,
	project func(*graph.Node) TemporalConstCandidate,
	consume func(*TemporalConstDeref) error,
) (stats TemporalConstStats, err error) {
	var consumeErr error
	defer func() {
		if err != nil {
			err = fmt.Errorf("%w: %w", ErrRebuildTemporalConst, err)
		}
		// The reducer owns consume errors. Constant reads mark their own
		// failures in Value, while construction and cleanup are ours.
		err = errors.Join(err, consumeErr)
		if err != nil {
			stats = TemporalConstStats{}
		}
	}()
	if v.build == nil || seed == nil || project == nil || consume == nil {
		return stats, errors.New("invalid Temporal constant inputs")
	}
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	if err := v.startTemporalConstScratch(); err != nil {
		return stats, err
	}
	db := v.build.pending
	result := &TemporalConstDeref{db: db}
	defer func() {
		result.db = nil
		err = errors.Join(err, db.DeleteRange(temporalConstPrefix, prefixEnd(temporalConstPrefix), pebble.NoSync))
	}()

	p := temporalConstPass{view: v, project: project}
	if err := seed(func(name string) error {
		if err := ctx.Err(); err != nil || name == "" {
			return err
		}
		_, err := p.discover(name)
		return err
	}); err != nil {
		return stats, fmt.Errorf("seed names: %w", err)
	}
	if err := p.resolve(ctx); err != nil {
		return stats, err
	}
	if err := errors.Join(ctx.Err(), v.mutationError()); err != nil {
		return stats, err
	}
	consumeErr = consume(result)
	return p.stats, ctx.Err()
}

func (v RebuildView) startTemporalConstScratch() error {
	v.build.mu.Lock()
	defer v.build.mu.Unlock()
	if v.build.first != nil {
		return v.build.first
	}
	db := v.build.pending
	owned, err := temporalConstHas(db, temporalConstPrefix)
	if err != nil {
		return err
	}
	if owned {
		return errors.New("scratch for Temporal constants already owned")
	}
	return db.Set(temporalConstPrefix, nil, pebble.NoSync)
}

// At most one alias/name iterator and one name-candidate iterator are active.
// Only the current tuple, candidate and a few literal/alias strings are held.
type temporalConstPass struct {
	view    RebuildView
	project func(*graph.Node) TemporalConstCandidate
	stats   TemporalConstStats
	next    uint64
}

func (p *temporalConstPass) resolve(ctx context.Context) error {
	if err := p.visit(ctx, temporalConstName, func(name, ordinal string) error {
		return p.resolveName(ctx, name, ordinal)
	}); err != nil {
		return fmt.Errorf("resolve seed names: %w", err)
	}
	for range temporalConstPassLimit {
		before := p.stats.Names
		// The iterator sees only aliases known at the start of this round.
		// The live name-set read deduplicates referents discovered in it.
		if err := p.visit(ctx, temporalConstAlias, func(_, ref string) error {
			ordinal, err := p.discover(ref)
			if err != nil || ordinal == "" {
				return err
			}
			return p.resolveName(ctx, ref, ordinal)
		}); err != nil {
			return fmt.Errorf("discover alias names: %w", err)
		}
		if p.stats.Names == before {
			break
		}
		p.stats.DiscoveryPasses++
	}
	for range temporalConstPassLimit {
		progress := false
		// Use first discovery from the frozen dispatch stream, with live reads.
		// For chains longer than eight hops among discovered names, this is
		// a deterministic choice, not native-exact. The VM proof labels any
		// Go map-order mismatch as a native-defect difference.
		if err := p.visit(ctx, temporalConstAlias, func(name, ref string) error {
			db := p.view.build.pending
			value, err := readTemporalConstValue(db, name)
			if err != nil || value != "" {
				return err
			}
			value, err = readTemporalConstValue(db, ref)
			if err != nil || value == "" {
				return err
			}
			progress = true
			return p.set(temporalConstKey(temporalConstValue, name), value)
		}); err != nil {
			return fmt.Errorf("collapse aliases: %w", err)
		}
		if !progress {
			break
		}
		p.stats.CollapsePasses++
	}
	return nil
}

func (p *temporalConstPass) discover(name string) (string, error) {
	key := temporalConstKey(temporalConstName, name)
	known, err := temporalConstHas(p.view.build.pending, key)
	if err != nil || known {
		return "", err
	}
	ordinal := string(binary.BigEndian.AppendUint64(nil, p.next))
	p.next++
	return ordinal, p.set(key, ordinal)
}

func (p *temporalConstPass) resolveName(ctx context.Context, name, ordinal string) error {
	p.stats.Names++
	var alias, aliasID, javaLiteral, javaID, literal string
	var conflict bool
	ingest := func(value string) {
		if value == "" {
			return
		}
		conflict = conflict || (literal != "" && literal != value)
		literal = value
	}
	if err := p.view.VisitNameCandidates(ctx, name, nil, func(n *graph.Node) error {
		c := p.project(n)
		// Native last-wins slots have no defined candidate order. Select
		// the last exact identity by ID, independent of T4 transport order.
		if c.Alias != "" && (alias == "" || n.ID > aliasID) {
			alias, aliasID = c.Alias, n.ID
		}
		if c.JavaLiteral != "" && (javaLiteral == "" || n.ID > javaID) {
			javaLiteral, javaID = c.JavaLiteral, n.ID
		}
		if !c.ReadSidecar {
			return nil
		}
		saved, err := p.view.ConstantValue(ctx, n.ID)
		if err != nil {
			return err
		}
		if saved != nil {
			ingest(saved.Value)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("read candidates for %q: %w", name, err)
	}
	if alias != "" {
		if len(ordinal) != 8 {
			return invalid("Temporal constant name ordinal")
		}
		key := appendKeyString(append(temporalConstKey(temporalConstAlias), ordinal...), name)
		if err := p.set(key, alias); err != nil {
			return err
		}
	}
	// The chosen Java value is ingested after all selected sidecar rows.
	// A conflict removes the literal, not the alias: the native collapse
	// may still fill that name from a resolved reference.
	ingest(javaLiteral)
	if conflict {
		return p.set(temporalConstKey(temporalConstAmbiguous, name), "")
	}
	if literal != "" {
		return p.set(temporalConstKey(temporalConstValue, name), literal)
	}
	return nil
}

func (p *temporalConstPass) visit(ctx context.Context, col byte, yield func(name, value string) error) error {
	prefix := temporalConstKey(col)
	return pendingVisit(ctx, p.view.build.pending, prefix, func(r row) error {
		key := r.key[len(prefix):]
		if col == temporalConstAlias {
			if len(key) <= 8 {
				return invalid("Temporal constant alias ordinal")
			}
			key = key[8:]
		}
		name, tail, err := readKeyString(key)
		if err != nil || name == "" || len(tail) != 0 {
			return invalid("Temporal constant scratch key")
		}
		return yield(name, string(r.value))
	})
}

func (p *temporalConstPass) set(key []byte, value string) error {
	db := p.view.build.pending
	if err := db.Set(key, []byte(value), pebble.NoSync); err != nil {
		return err
	}
	p.stats.ScratchBytes += uint64(len(key) + len(value))
	return nil
}

func temporalConstKey(col byte, names ...string) []byte {
	return keyStrings([]byte{'p', 't', col}, names...)
}

func temporalConstHas(db *pebble.DB, key []byte) (bool, error) {
	return pendingGet(db, key, func([]byte) (bool, error) { return true, nil })
}

func readTemporalConstValue(db *pebble.DB, name string) (string, error) {
	return pendingGet(db, temporalConstKey(temporalConstValue, name), func(raw []byte) (string, error) {
		if len(raw) == 0 {
			return "", invalid("empty Temporal constant value row")
		}
		return string(raw), nil
	})
}

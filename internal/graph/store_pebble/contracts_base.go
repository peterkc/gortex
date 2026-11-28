package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"slices"

	"github.com/cockroachdb/pebble/v2/sstable"
	"github.com/zeebo/blake3"
)

// RelationsBase is the retained FileGraph plus the exact Relations point
// diff. Its hard links live in the originating build's lease until Close.
type RelationsBase struct {
	files   FileGraphBase
	paths   []string
	witness Witness
	digest  HeadDigest
}

// RetainRelations keeps the sealed relation tables before ingest consumes
// their outward names. Nothing is ingested into the private working store.
func (b *BuildStore) RetainRelations(ctx context.Context, base FileGraphBase, relations StageTables) (RelationsBase, error) {
	if err := ctx.Err(); err != nil {
		return RelationsBase{}, err
	}
	w := relations.Witness
	if base.build != b || w.Stage != StageRelations || w.Profile != b.profile || !filepath.IsAbs(relations.Dir) {
		return RelationsBase{}, invalid("contracts needs retained relations from this build")
	}
	if relations.Base != base.digest || relations.Incarnation != base.incarnation ||
		w.Version() != base.witness.Version() || w.FrozenInputs != base.witness.FrozenInputs {
		return RelationsBase{}, ErrStaleBase
	}
	if len(relations.Files) >= goldFileCap {
		return RelationsBase{}, ErrStageTableLimit
	}
	for _, tag := range []byte{familyManifest, familyFileMeta} {
		match := func(f FamilyWitness) bool { return f.Family == tag }
		old, next := slices.IndexFunc(base.witness.Families, match), slices.IndexFunc(w.Families, match)
		if (old == -1) != (next == -1) || (old >= 0 && base.witness.Families[old] != w.Families[next]) {
			return RelationsBase{}, invalid("relations changed an immutable family")
		}
	}
	raw, err := encodeWitness(w)
	if err != nil {
		return RelationsBase{}, err
	}
	paths, err := b.retainStageFiles(ctx, relations, "relations")
	if err != nil {
		return RelationsBase{}, err
	}
	w.Families = slices.Clone(w.Families)
	w.Dependencies = slices.Clone(w.Dependencies)
	if w.Relations != nil {
		w.Relations = &RelationCoverage{DeferredLanguages: slices.Clone(w.Relations.DeferredLanguages)}
	}
	return RelationsBase{files: base, paths: paths, witness: w, digest: blake3.Sum256(raw)}, nil
}

// relationBaseCursor joins two sorted, non-overlapping-in-layer SST streams.
// Each cursor holds one reader. The joined stream must match the exact sealed
// witness; the file-graph cursor separately checks its immutable file rows.
type relationBaseCursor struct {
	base, delta         fileGraphCursor
	baseLive, deltaLive bool
	advance, override   bool
	stream              goldWitness
	expected            []FamilyWitness
	err                 error
}

func (b RelationsBase) cursor(ctx context.Context) *relationBaseCursor {
	c := &relationBaseCursor{
		base:   fileGraphCursor{ctx: ctx, options: b.files.build.options, paths: b.files.paths, witness: goldWitness{digest: blake3.New()}},
		delta:  fileGraphCursor{ctx: ctx, options: b.files.build.options, paths: b.paths, delta: true},
		stream: goldWitness{digest: blake3.New()},
	}
	for _, f := range b.files.witness.Families {
		if f.Family != familyManifest {
			c.base.expected = append(c.base.expected, f)
		}
	}
	for _, f := range b.witness.Families {
		if f.Family != familyManifest && f.Family != familyFileMeta {
			c.expected = append(c.expected, f)
		}
	}
	return c
}

func (c *relationBaseCursor) First() bool {
	c.baseLive, c.deltaLive = c.base.First(), c.delta.First()
	return c.next()
}

func (c *relationBaseCursor) Next() bool {
	if c.advance {
		c.baseLive = c.base.Next()
	}
	if c.override {
		c.deltaLive = c.delta.Next()
	}
	return c.next()
}

func (c *relationBaseCursor) next() bool {
	for c.baseLive || c.deltaLive {
		// Negative picks the file-graph row; positive picks a new delta row.
		cmp := 1
		if !c.deltaLive {
			cmp = -1
		} else if c.baseLive {
			cmp = bytes.Compare(c.base.Key(), c.delta.Key())
		}
		c.advance, c.override = cmp <= 0, cmp >= 0
		if c.override && c.delta.deleted {
			if cmp != 0 {
				c.err = invalid("retained relations deletes an absent row")
				return false
			}
			c.baseLive, c.deltaLive = c.base.Next(), c.delta.Next()
			continue
		}
		c.stream.add(c.current())
		return true
	}
	c.stream.flush()
	if !slices.Equal(c.stream.families, c.expected) {
		c.err = invalid("retained relations rows differ from their witness")
	}
	return false
}

func (c *relationBaseCursor) current() row {
	if c.override {
		return c.delta.row
	}
	return c.base.row
}

func (c *relationBaseCursor) Key() []byte                  { return c.current().key }
func (c *relationBaseCursor) ValueAndErr() ([]byte, error) { return c.current().value, nil }
func (c *relationBaseCursor) Error() error {
	return errors.Join(c.err, c.base.Error(), c.delta.Error())
}
func (c *relationBaseCursor) Close() error { return errors.Join(c.base.Close(), c.delta.Close()) }

// Carry the Relations-only endpoint counters. Contracts adds no node-free
// endpoints, so those two exact counts are unchanged. The full diff still
// verifies the relation stream against its witness.
func (b RelationsBase) addEndpointCounts(ctx context.Context, gold *sortedRuns) error {
	keys := [][]byte{countKey("external import endpoints"), countKey("go dataflow source endpoints")}
	for _, path := range b.paths {
		if err := addContractPredecessorCounts(ctx, path, keys, gold); err != nil {
			return err
		}
	}
	return nil
}

func addContractPredecessorCounts(ctx context.Context, path string, keys [][]byte, gold *sortedRuns) (err error) {
	table, err := openGoldTable(ctx, path, gold.options)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, table.Close()) }()
	for _, key := range keys {
		if err := copyContractPredecessorCount(table, key, gold); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func copyContractPredecessorCount(table *sstable.Reader, key []byte, gold *sortedRuns) (err error) {
	it, err := table.NewIter(sstable.NoTransforms, key, prefixEnd(key), sstable.TableBlobContext{})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, it.Close()) }()
	kv := it.SeekGE(key, 0)
	if kv == nil || !bytes.Equal(kv.K.UserKey, key) {
		return it.Error()
	}
	if kv.K.Kind() != sstable.InternalKeyKindSet {
		return invalid("invalid retained endpoint counter")
	}
	value, _, err := kv.Value(nil)
	if err != nil {
		return err
	}
	return gold.add(key, bytes.Clone(value))
}

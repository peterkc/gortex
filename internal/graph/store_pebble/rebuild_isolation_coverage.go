package store_pebble

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"strings"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/sstable"
	"github.com/zeebo/blake3"
)

// RebuildIsolationAffectedScope counts unique cause events affecting a
// supported file.
type RebuildIsolationAffectedScope struct {
	FilePath, SupportedLanguage, DeferredLanguage, Cause string
	Count                                                uint64
}

// RebuildIsolationDependency summarizes a sealed scope for language-only
// witness checks, without putting per-file data on the worker control wire.
type RebuildIsolationDependency struct {
	SupportedLanguage, DeferredLanguage string
}

func compareIsolationDependency(a, b RebuildIsolationDependency) int {
	if c := cmp.Compare(a.SupportedLanguage, b.SupportedLanguage); c != 0 {
		return c
	}
	return cmp.Compare(a.DeferredLanguage, b.DeferredLanguage)
}

func linkedIsolationLanguage(id string) bool {
	return id == "go" || id == "javascript" || id == "typescript"
}

func validIsolationDependency(pair RebuildIsolationDependency) bool {
	return linkedIsolationLanguage(pair.SupportedLanguage) && pair.DeferredLanguage != "" &&
		!linkedIsolationLanguage(pair.DeferredLanguage) && !strings.ContainsRune(pair.DeferredLanguage, 0)
}

func isolationScopeKey(s RebuildIsolationAffectedScope) ([]byte, error) {
	if !validIsolationDependency(RebuildIsolationDependency{s.SupportedLanguage, s.DeferredLanguage}) ||
		s.FilePath == "" || strings.ContainsRune(s.FilePath, 0) {
		return nil, invalid("invalid isolation scope")
	}
	switch s.Cause {
	case "clone-corpus", "ts-alias", "npm-candidate", "held-endpoint":
	default:
		return nil, invalid("unknown isolation cause")
	}
	return keyStrings([]byte{familyIsolation}, s.FilePath, s.SupportedLanguage, s.DeferredLanguage, s.Cause), nil
}

func decodeIsolationScope(key, value []byte) (RebuildIsolationAffectedScope, error) {
	var s RebuildIsolationAffectedScope
	if len(key) == 0 || key[0] != familyIsolation || len(value) != 8 {
		return s, invalid("invalid isolation row")
	}
	tail := key[1:]
	for _, field := range []*string{&s.FilePath, &s.SupportedLanguage, &s.DeferredLanguage, &s.Cause} {
		var err error
		*field, tail, err = readKeyString(tail)
		if err != nil {
			return s, err
		}
	}
	s.Count = binary.BigEndian.Uint64(value)
	canonical, err := isolationScopeKey(s)
	if err != nil {
		return s, err
	}
	if s.Count == 0 || len(tail) != 0 || !bytes.Equal(canonical, key) {
		return s, invalid("invalid isolation count or key")
	}
	return s, nil
}

// MarkRebuildIsolationAffected records one event. Repeating the same event
// in the same scope does not increase its count. Count is zero or one on input.
func (b *BuildStore) MarkRebuildIsolationAffected(ctx context.Context, scope RebuildIsolationAffectedScope, event string) error {
	return b.withWrites(func() error { return b.markRebuildIsolationAffected(ctx, scope, event) })
}

// Internal callers with b.mu already held use the same marker path.
func (b *BuildStore) markRebuildIsolationAffected(ctx context.Context, scope RebuildIsolationAffectedScope, event string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.rebuildIsolationCoverage != nil || event == "" || scope.Count > 1 {
		return invalid("isolation event after census or invalid event")
	}
	key, err := isolationScopeKey(scope)
	if err != nil {
		return err
	}
	key[0] = 'i' // private event rows; only grouped scopes enter serving tables
	return b.pending.Set(appendKeyString(key, event), nil, pebble.NoSync)
}

// CheckRebuildIsolationCoverage closes the event inventory and declares the
// complete deferred extractor census, including an explicitly empty census.
func (b *BuildStore) CheckRebuildIsolationCoverage(deferred []string) error {
	return b.withWrites(func() error {
		if b.rebuildIsolationCoverage != nil || b.rebuildLinksIsolated {
			return invalid("isolation census already checked or cut still open")
		}
		var prev string
		for _, id := range deferred {
			if id <= prev || linkedIsolationLanguage(id) || strings.ContainsRune(id, 0) {
				return invalid("invalid isolation deferred census")
			}
			prev = id
		}
		b.rebuildIsolationCoverage = &RelationCoverage{DeferredLanguages: slices.Clone(deferred), IsolationChecked: true}
		return nil
	})
}

// prepareIsolationScopes groups the sorted private event rows by scope. It
// keeps one scope and a bounded-by-extractor-census set of dependency pairs.
// PrepareRelations holds b.mu and owns the only transition to serving rows.
func (b *BuildStore) prepareIsolationScopes(ctx context.Context) (*RelationCoverage, error) {
	coverage := cloneRelationCoverage(b.rebuildIsolationCoverage)
	prefix := []byte{familyIsolation}
	if err := b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync); err != nil {
		return nil, err
	}
	var last []byte
	var count uint64
	pairs := make(map[RebuildIsolationDependency]struct{})
	flush := func() error {
		if count == 0 {
			return nil
		}
		return b.pending.Set(last, binary.BigEndian.AppendUint64(nil, count), pebble.NoSync)
	}
	if err := pendingVisit(ctx, b.pending, []byte{'i'}, func(r row) error {
		if coverage == nil || len(r.value) != 0 {
			return invalid("isolation event without checked census")
		}
		tail := r.key[1:]
		for range 4 {
			_, rest, err := readKeyString(tail)
			if err != nil {
				return err
			}
			tail = rest
		}
		event, rest, err := readKeyString(tail)
		if err != nil || event == "" || len(rest) != 0 {
			return invalid("invalid private isolation event")
		}
		key := bytes.Clone(r.key[:len(r.key)-len(tail)])
		key[0] = familyIsolation
		if !bytes.Equal(key, last) {
			if err := flush(); err != nil {
				return err
			}
			scope, err := decodeIsolationScope(key, binary.BigEndian.AppendUint64(nil, 1))
			if err != nil {
				return err
			}
			if !slices.Contains(coverage.DeferredLanguages, scope.DeferredLanguage) {
				return invalid("isolation event outside deferred census")
			}
			pairs[RebuildIsolationDependency{scope.SupportedLanguage, scope.DeferredLanguage}] = struct{}{}
			last, count = key, 0
		}
		if count == ^uint64(0) {
			return invalid("isolation scope count overflow")
		}
		count++
		return nil
	}); err != nil {
		return nil, err
	}
	if err := flush(); err != nil {
		return nil, err
	}
	for pair := range pairs {
		coverage.IsolationDependencies = append(coverage.IsolationDependencies, pair)
	}
	if coverage != nil {
		slices.SortFunc(coverage.IsolationDependencies, compareIsolationDependency)
	}
	return coverage, nil
}

func cloneRelationCoverage(c *RelationCoverage) *RelationCoverage {
	if c == nil {
		return nil
	}
	return &RelationCoverage{DeferredLanguages: slices.Clone(c.DeferredLanguages), IsolationChecked: c.IsolationChecked,
		IsolationDependencies: slices.Clone(c.IsolationDependencies)}
}

func equalRelationCoverage(a, b *RelationCoverage) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.IsolationChecked == b.IsolationChecked && slices.Equal(a.DeferredLanguages, b.DeferredLanguages) &&
		slices.Equal(a.IsolationDependencies, b.IsolationDependencies)
}

func isolationFamily(w Witness) FamilyWitness {
	for _, f := range w.Families {
		if f.Family == familyIsolation {
			return f
		}
	}
	return FamilyWitness{}
}

func checkIsolationWitness(w Witness) error {
	f := isolationFamily(w)
	c := w.Relations
	if c == nil || !c.IsolationChecked {
		if f != (FamilyWitness{}) || (c != nil && len(c.IsolationDependencies) != 0) {
			return invalid("isolation family without checked coverage")
		}
		return nil
	}
	if w.Stage.rank() < StageRelations.rank() || (f == (FamilyWitness{})) != (len(c.IsolationDependencies) == 0) ||
		(f != (FamilyWitness{}) && (f.Count < uint64(len(c.IsolationDependencies)) || f.Digest == (HeadDigest{}))) {
		return invalid("isolation family and coverage differ")
	}
	for i, pair := range c.IsolationDependencies {
		if !validIsolationDependency(pair) || !slices.Contains(c.DeferredLanguages, pair.DeferredLanguage) ||
			(i > 0 && compareIsolationDependency(c.IsolationDependencies[i-1], pair) >= 0) {
			return invalid("invalid or unordered isolation dependency")
		}
	}
	for _, id := range c.DeferredLanguages {
		if linkedIsolationLanguage(id) || strings.ContainsRune(id, 0) {
			return invalid("invalid checked deferred language")
		}
	}
	return nil
}

func (c *RelationCoverage) deferredInScope(languages []string) []string {
	var missing []string
	for _, id := range c.DeferredLanguages {
		if len(languages) == 0 || slices.Contains(languages, id) {
			missing = append(missing, id)
		}
	}
	return missing
}

// isolationVerification checks rows against the compact witness and optionally
// yields the exact scopes. The same check protects ingestion and snapshot reads.
type isolationVerification struct {
	w      Witness
	digest *blake3.Hasher
	count  uint64
	last   []byte
	pairs  map[RebuildIsolationDependency]struct{}
}

func newIsolationVerification(w Witness) *isolationVerification {
	return &isolationVerification{w: w, digest: blake3.New(), pairs: make(map[RebuildIsolationDependency]struct{})}
}

func (v *isolationVerification) visit(key, value []byte) (RebuildIsolationAffectedScope, error) {
	scope, err := decodeIsolationScope(key, value)
	if err != nil {
		return scope, err
	}
	pair := RebuildIsolationDependency{scope.SupportedLanguage, scope.DeferredLanguage}
	if v.w.Relations == nil || !v.w.Relations.IsolationChecked ||
		!slices.Contains(v.w.Relations.IsolationDependencies, pair) || bytes.Compare(v.last, key) >= 0 ||
		v.count >= isolationFamily(v.w).Count {
		return scope, invalid("unexpected isolation scope row")
	}
	v.last = append(v.last[:0], key...)
	v.count++
	v.pairs[pair] = struct{}{}
	writeWitnessRow(v.digest, key, value)
	return scope, nil
}

func (v *isolationVerification) finish() error {
	want := isolationFamily(v.w)
	if v.count != want.Count || (v.count != 0 && HeadDigest(v.digest.Sum(nil)) != want.Digest) ||
		(v.w.Relations != nil && len(v.pairs) != len(v.w.Relations.IsolationDependencies)) {
		return invalid("isolation rows differ from witness")
	}
	return nil
}

// checkIsolationTables verifies the initial sealed relation scope rows before
// ingest. No later point diff may change these rows. Range deletion is not
// part of any of the stage table writers.
func checkIsolationTables(ctx context.Context, options *pebble.Options, paths []string, w Witness, delta bool) error {
	initial := !delta && w.Stage == StageRelations
	v := newIsolationVerification(w)
	for _, path := range paths {
		if err := visitIsolationTable(ctx, options, path, func(key, value []byte) error {
			if !initial {
				return invalid("publication changes sealed isolation family")
			}
			_, err := v.visit(key, value)
			return err
		}); err != nil {
			return err
		}
	}
	if initial {
		return v.finish()
	}
	return nil
}

func visitIsolationTable(ctx context.Context, options *pebble.Options, path string, visit func([]byte, []byte) error) (err error) {
	table, err := openGoldTable(ctx, path, options)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, table.Close()) }()
	props, err := table.ReadPropertiesBlock(ctx, nil)
	if err != nil {
		return err
	}
	if props.NumRangeDeletions != 0 {
		return invalid("publication stage uses range deletion")
	}
	prefix := []byte{familyIsolation}
	it, err := table.NewIter(sstable.NoTransforms, prefix, prefixEnd(prefix), sstable.TableBlobContext{})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, it.Close()) }()
	for kv := it.SeekGE(prefix, 0); kv != nil && bytes.HasPrefix(kv.K.UserKey, prefix); kv = it.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if kv.K.Kind() != sstable.InternalKeyKindSet {
			return invalid("isolation stage row is not a set")
		}
		value, _, err := kv.Value(nil)
		if err != nil {
			return err
		}
		if err := visit(kv.K.UserKey, value); err != nil {
			return err
		}
	}
	return it.Error()
}

// RequireRelationCoverageContext checks one pinned serving snapshot. An empty
// language scope requires all languages; empty file scope requires all files.
func (s *Snapshot) RequireRelationCoverageContext(ctx context.Context, languages, files []string) error {
	w, err := s.Publication()
	if err != nil {
		return err
	}
	if err := w.RequireRelationCoverage(languages); err != nil && (w.Relations == nil || !w.Relations.IsolationChecked) {
		return err
	}
	if err := errors.Join(ctx.Err(), s.ReadError()); err != nil {
		return err
	}
	if w.Relations == nil || !w.Relations.IsolationChecked {
		return w.RequireRelationCoverage(languages)
	}
	missing := w.Relations.deferredInScope(languages)
	var affected []RebuildIsolationAffectedScope
	if err := s.verifyIsolationCoverage(ctx, w, func(scope RebuildIsolationAffectedScope) {
		if (len(languages) == 0 || slices.Contains(languages, scope.SupportedLanguage)) &&
			(len(files) == 0 || slices.Contains(files, scope.FilePath)) {
			affected = append(affected, scope)
			if !slices.Contains(missing, scope.DeferredLanguage) {
				missing = append(missing, scope.DeferredLanguage)
			}
		}
	}); err != nil {
		return err
	}
	if len(missing) != 0 {
		slices.Sort(missing)
		return &ErrLinksNotAvailable{Languages: slices.Compact(missing), Affected: affected}
	}
	return nil
}

func (s *Snapshot) verifyIsolationCoverage(ctx context.Context, w Witness, yield func(RebuildIsolationAffectedScope)) error {
	v := newIsolationVerification(w)
	if err := scan(ctx, s, []byte{familyIsolation}, func(key, value []byte) (bool, error) {
		scope, err := v.visit(key, value)
		if err != nil {
			return false, err
		}
		if yield != nil {
			yield(scope)
		}
		return true, nil
	}); err != nil {
		return err
	}
	return v.finish()
}

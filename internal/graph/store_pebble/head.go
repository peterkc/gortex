package store_pebble

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/cockroachdb/pebble/v2"
	"github.com/google/uuid"
	"github.com/zeebo/blake3"
)

var (
	ErrStaleBase          = errors.New("stale publication base")
	ErrInvalidIncarnation = errors.New("checkout incarnation must be a canonical UUIDv7")
	ErrStageOrder         = errors.New("publication stage is repeated or out of order")
	ErrStageTableLimit    = errors.New("publication exceeds the SST file limit")
	ErrCoverageRegression = errors.New("publication would reduce relation coverage")
)

// Head is the daemon-owned writable side of one checkout head. The catalog
// owns its lifetime and must drain its pins before closing it. The worker
// receives a read-only checkpoint or owns private files, never this handle.
type Head struct {
	db          *pebble.DB
	options     *pebble.Options
	incarnation string
	profile     Profile
	directory   string
}

// The bound is across all heads owned by one daemon, not per checkout.
var ingestMu sync.Mutex

// ValidateIncarnation checks the exact catalog spelling, without normalizing it.
func ValidateIncarnation(incarnation string) error {
	if len(incarnation) != 36 {
		return ErrInvalidIncarnation
	}
	id, err := uuid.Parse(incarnation)
	if err != nil || id.Version() != 7 || id.Variant() != uuid.RFC4122 || id.String() != incarnation {
		return ErrInvalidIncarnation
	}
	return nil
}

func OpenHead(path string, incarnation string, profile Profile) (*Head, error) {
	if err := ValidateIncarnation(incarnation); err != nil {
		return nil, err
	}
	if profile != DefaultProfile && profile != FullProfile {
		return nil, invalid("head profile")
	}
	options := headOptions()
	db, err := pebble.Open(path, options)
	if err != nil {
		return nil, err
	}
	return &Head{db: db, options: options, incarnation: incarnation, profile: profile, directory: filepath.Clean(path)}, nil
}

func (h *Head) Close() error { return h.db.Close() }

func (h *Head) OpenSnapshot() *Snapshot {
	return &Snapshot{snap: pebbleSnapshotReader{h.db.NewSnapshot()}, profile: h.profile}
}

func (h *Head) Checkpoint(dir string) error { return h.db.Checkpoint(dir) }

// StageTables is an owned, sealed set of non-overlapping SST files in Dir.
// Files are simple names: only the daemon supplies the leased directory.
// Base is the digest of the exact previous witness value, not its version.
type StageTables struct {
	Dir         string
	Files       []string
	Incarnation string
	Base        HeadDigest
	Witness     Witness
}

func (h *Head) PublishStage(ctx context.Context, tables StageTables) (HeadDigest, error) {
	return h.publish(ctx, tables, false)
}

func (h *Head) PublishDelta(ctx context.Context, tables StageTables) (HeadDigest, error) {
	return h.publish(ctx, tables, true)
}

func (h *Head) publish(ctx context.Context, tables StageTables, delta bool) (HeadDigest, error) {
	var zero HeadDigest
	if len(tables.Files) >= goldFileCap { // one table reserved for the witness
		return zero, fmt.Errorf("%w: %d files plus witness, maximum %d", ErrStageTableLimit, len(tables.Files), goldFileCap)
	}
	if tables.Dir == "" || !filepath.IsAbs(tables.Dir) {
		return zero, invalid("invalid stage table directory")
	}
	if tables.Witness.Profile != h.profile {
		return zero, invalid("publication profile differs from head")
	}
	raw, err := encodeWitness(tables.Witness)
	if err != nil {
		return zero, err
	}
	ingestMu.Lock()
	defer ingestMu.Unlock()
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if tables.Incarnation != h.incarnation {
		return zero, fmt.Errorf("%w: checkout incarnation changed", ErrStaleBase)
	}
	prior, base, err := h.currentWitness()
	if err != nil {
		return zero, err
	}
	if delta {
		if prior.Stage != StageSearch || tables.Witness.Stage != StageSearch {
			return zero, fmt.Errorf("%w: edit delta requires a complete base and result", ErrStageOrder)
		}
		if tables.Witness.PlatformCatalogRevision != prior.PlatformCatalogRevision {
			return zero, fmt.Errorf("%w: changed platform catalog requires a fresh build", ErrStaleBase)
		}
	} else {
		if prior.Stage != 0 && (tables.Witness.Version() != prior.Version() || tables.Witness.FrozenInputs != prior.FrozenInputs) {
			return zero, fmt.Errorf("%w: stage belongs to a different version", ErrStaleBase)
		}
		if !followsStage(tables.Witness.Stage, prior.Stage) {
			return zero, fmt.Errorf("%w: got %s after %s", ErrStageOrder, tables.Witness.Stage, prior.Stage)
		}
	}
	if base != tables.Base {
		return zero, fmt.Errorf("%w: publication witness changed", ErrStaleBase)
	}
	if delta {
		if !tables.Witness.CoversRelationsAtLeast(prior) {
			return zero, ErrCoverageRegression
		}
	} else if prior.Stage >= StageRelations {
		if !equalRelationCoverage(tables.Witness.Relations, prior.Relations) || isolationFamily(tables.Witness) != isolationFamily(prior) {
			return zero, invalid("publication relation coverage changed after relations stage")
		}
	}
	if delta && prior.Relations != nil && prior.Relations.IsolationChecked &&
		(!equalRelationCoverage(tables.Witness.Relations, prior.Relations) || isolationFamily(tables.Witness) != isolationFamily(prior)) {
		return zero, ErrCoverageRegression
	}

	nextContracts := tables.Witness.Contracts
	if prior.Contracts == nil && nextContracts != nil && tables.Witness.Stage != StageContracts {
		return zero, invalid("publication contract coverage must start at contracts")
	}
	if prior.Contracts != nil && (nextContracts == nil || (!delta && *nextContracts != *prior.Contracts)) {
		return zero, invalid("publication contract coverage changed")
	}
	if !delta && tables.Witness.Stage == StageDerived {
		if len(tables.Witness.Unsupported) == 0 || len(tables.Files) != 0 || !slices.Equal(tables.Witness.Families, prior.Families) {
			return zero, invalid("D64 derived stage must only declare unsupported families")
		}
	}
	if tables.Witness.Stage == StageSearch && (!delta || len(prior.Unsupported) != 0) {
		if len(prior.Unsupported) == 0 || !slices.Equal(tables.Witness.Unsupported, prior.Unsupported) {
			return zero, invalid("publication changed D64 unsupported families")
		}
		if !delta {
			before := slices.DeleteFunc(slices.Clone(tables.Witness.Families), func(f FamilyWitness) bool { return f.Family == familySymbol })
			if !slices.Equal(before, prior.Families) {
				return zero, invalid("search changed a non-symbol family")
			}
		}
	}

	paths := make([]string, 0, len(tables.Files)+1)
	seen := make(map[string]struct{}, len(tables.Files))
	for _, file := range tables.Files {
		if filepath.Base(file) != file || file == "." || filepath.Ext(file) != ".sst" {
			return zero, invalid("publication table name")
		}
		if _, duplicate := seen[file]; duplicate {
			return zero, invalid("repeated table in one publication")
		}
		seen[file] = struct{}{}
		path := filepath.Join(tables.Dir, file)
		info, err := os.Lstat(path)
		if err != nil {
			return zero, err
		}
		if !info.Mode().IsRegular() {
			return zero, invalid("publication table is not a regular file")
		}
		paths = append(paths, path)
	}
	if err := checkIsolationTables(ctx, h.options, paths, tables.Witness, delta); err != nil {
		return zero, err
	}
	witnessPath, err := h.writeWitness(tables.Dir, raw)
	if err != nil {
		return zero, err
	}
	// The build directory's owner removes the unpublished stage's files after
	// refusal. Ingest consumes its inputs on success, including the witness.
	if err := h.db.Ingest(ctx, append(paths, witnessPath)); err != nil {
		return zero, err
	}
	return blake3.Sum256(raw), nil
}

func followsStage(next, prior Stage) bool {
	// The low-level head still accepts legacy stages without Contracts.
	return next.rank() == prior.rank()+1 || (prior == StageRelations && next == StageDerived)
}

// CoversRelationsAtLeast compares proved coverage in validated witnesses.
// A legacy witness proves none. The replacement owner must also check stage
// order and head identity.
func (w Witness) CoversRelationsAtLeast(prior Witness) bool {
	if prior.Relations == nil {
		return true
	}
	if w.Relations == nil {
		return false
	}
	if prior.Relations.IsolationChecked && !w.Relations.IsolationChecked {
		return false
	}
	if !prior.Relations.IsolationChecked && len(prior.Relations.DeferredLanguages) != 0 && w.Relations.IsolationChecked {
		return true // the old mixed witness proved no supported language complete
	}
	for _, pair := range w.Relations.IsolationDependencies {
		if !slices.Contains(prior.Relations.DeferredLanguages, pair.SupportedLanguage) &&
			!slices.ContainsFunc(prior.Relations.IsolationDependencies, func(old RebuildIsolationDependency) bool {
				return old.SupportedLanguage == pair.SupportedLanguage
			}) {
			return false
		}
	}
	for _, id := range w.Relations.DeferredLanguages {
		if _, found := slices.BinarySearch(prior.Relations.DeferredLanguages, id); !found {
			return false
		}
	}
	return true
}

func (h *Head) currentWitness() (Witness, HeadDigest, error) {
	var zero HeadDigest
	value, closer, err := h.db.Get(publicationWitnessKey)
	if errors.Is(err, pebble.ErrNotFound) {
		return Witness{}, zero, nil
	}
	if err != nil {
		return Witness{}, zero, err
	}
	w, err := decodeWitness(value)
	digest := blake3.Sum256(value)
	return w, digest, errors.Join(err, closer.Close())
}

func (h *Head) writeWitness(dir string, raw []byte) (string, error) {
	f, err := os.CreateTemp(dir, "witness-*.sst")
	if err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	format := h.db.FormatMajorVersion().MaxTableFormat()
	w, err := newTable(f.Name(), h.options.MakeWriterOptions(goldLevel, format))
	if err != nil {
		return "", err
	}
	err = errors.Join(w.Set(publicationWitnessKey, raw), w.Close())
	return f.Name(), err
}

package store_pebble

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/zeebo/blake3"
)

// Stage is the last complete layer in the prefix of a cold build. Zero is
// unpublished. Vectors are separate and are not a stage of the core set.
type Stage uint8

const (
	StageManifest Stage = iota + 1
	StageFileGraph
	StageRelations
	StageDerived
	StageSearch
	// Append wire values; old ordinals are part of the witness and worker wire.
	StageContracts
)

func (s Stage) String() string {
	switch s {
	case StageManifest:
		return "manifest"
	case StageFileGraph:
		return "file_graph"
	case StageRelations:
		return "relations"
	case StageContracts:
		return "contracts"
	case StageDerived:
		return "derived"
	case StageSearch:
		return "search"
	default:
		return "unpublished"
	}
}

// CoreStages lists the core stages in publication order, independent of the
// persisted stage numbers. The array is a copy; callers cannot change the order.
func CoreStages() [6]Stage {
	return [6]Stage{StageManifest, StageFileGraph, StageRelations, StageContracts, StageDerived, StageSearch}
}

func (s Stage) rank() int {
	order := CoreStages()
	if i := slices.Index(order[:], s); i >= 0 {
		return i + 1
	}
	return int(s)
}

var ErrNotPublished = errors.New("no head has been published")

// ErrStagePending names the stage a request needs and the last complete stage
// visible to its snapshot.
type ErrStagePending struct {
	Need, Published Stage
}

func (e *ErrStagePending) Error() string {
	return fmt.Sprintf("stage %s is pending (published through %s)", e.Need, e.Published)
}

// ErrLinksNotAvailable means the published stage cannot prove the requested links.
// A nil Languages list means the witness has no coverage declaration.
type ErrLinksNotAvailable struct {
	Languages []string
	Affected  []RebuildIsolationAffectedScope
}

func (e *ErrLinksNotAvailable) Error() string {
	if len(e.Languages) == 0 {
		return "links not available: relation coverage is unproved"
	}
	return "links not available for " + strings.Join(e.Languages, ", ")
}

// RelationCoverage declares the admitted language IDs whose links were deferred.
// IDs are ascending and unique. An empty set declares full relation coverage.
type RelationCoverage struct {
	DeferredLanguages []string
	IsolationChecked  bool
	// IsolationDependencies is the compact sorted set of supported/deferred
	// extractor pairs represented by the sealed isolation family.
	IsolationDependencies []RebuildIsolationDependency
}

// ContractCoverage declares the D83 omissions among logical contract records.
// A zero count is complete, including when no records were found.
type ContractCoverage struct {
	OmittedRecords uint64
}

type HeadDigest [32]byte

type FamilyWitness struct {
	Family byte
	Count  uint64
	Digest HeadDigest
}

// Witness describes the portable rows in a published head. Local progress,
// event IDs, checkout incarnation and paths are never mixed into these bytes.
type Witness struct {
	Input, Recipe       HeadDigest
	FrozenInputs        HeadDigest // optional digest of the private build-input sidecar
	UsesGitIgnorePolicy bool       // membership depends on operator-owned Git ignore policy
	Dependencies        []HeadDigest
	Profile             Profile
	Stage               Stage
	Families            []FamilyWitness     // ascending, unique family tags
	Relations           *RelationCoverage   // nil before relations or in legacy witnesses
	Contracts           *ContractCoverage   // nil before contracts or in legacy witnesses
	Unsupported         []UnsupportedFamily // exact D64 declarations from derived onward

	PlatformCatalogRevision uint32 // zero in legacy publications
}

// Version identifies the complete input and recipe. The witness digest also
// identifies the actual stage and rows; several witnesses share a version.
func (w Witness) Version() HeadDigest {
	h := blake3.New()
	_, _ = h.Write(w.Input[:])
	_, _ = h.Write(w.Recipe[:])
	for _, d := range w.Dependencies {
		_, _ = h.Write(d[:])
	}
	if w.UsesGitIgnorePolicy {
		_, _ = h.Write([]byte("gortex/git-ignore-policy/v1\x00"))
	}
	if w.PlatformCatalogRevision != 0 {
		_, _ = h.Write([]byte("gortex/platform-catalog/v1\x00"))
		_, _ = h.Write(binary.BigEndian.AppendUint32(nil, w.PlatformCatalogRevision))
	}
	return HeadDigest(h.Sum(nil))
}

var publicationWitnessKey = []byte{familyWitness}

const (
	witnessFormat          byte = 1
	witnessInputsFormat    byte = 2
	witnessCoverageFormat  byte = 3
	witnessGitPolicyFormat byte = 4
	witnessD64Format       byte = 5
	witnessCatalogFormat   byte = 6
	witnessContractsFormat byte = 7
	witnessIsolationFormat byte = 8
)

func encodeWitness(w Witness) ([]byte, error) {
	if w.Stage < StageManifest || w.Stage > StageContracts ||
		(w.Profile != DefaultProfile && w.Profile != FullProfile) || w.Input == (HeadDigest{}) || w.Recipe == (HeadDigest{}) {
		return nil, invalid("publication witness identity")
	}
	if (w.Stage == StageContracts && w.Contracts == nil) || (w.Contracts != nil && w.Stage.rank() < StageContracts.rank()) {
		return nil, invalid("publication contract coverage stage")
	}
	var omitted uint64
	for _, f := range w.Families {
		if f.Family == familyOmission {
			if w.Contracts == nil || f.Count == 0 {
				return nil, invalid("publication contract omission family without coverage or rows")
			}
			omitted = f.Count
		}
	}
	if w.Contracts != nil && omitted != w.Contracts.OmittedRecords {
		return nil, invalid("publication contract omission count differs from sealed rows")
	}
	if err := checkIsolationWitness(w); err != nil {
		return nil, err
	}
	format := witnessFormat
	if w.FrozenInputs != (HeadDigest{}) {
		format = witnessInputsFormat
	}
	if w.Relations != nil {
		if w.Stage < StageRelations {
			return nil, invalid("relation coverage before relations stage")
		}
		format = witnessCoverageFormat
	}
	if w.UsesGitIgnorePolicy {
		if w.FrozenInputs == (HeadDigest{}) {
			return nil, invalid("Git policy witness without frozen inputs")
		}
		format = witnessGitPolicyFormat
	}
	if len(w.Unsupported) != 0 {
		if w.Stage.rank() < StageDerived.rank() || !slices.Equal(w.Unsupported, deferredFamilies(w.Contracts != nil)) {
			return nil, invalid("publication D64 unsupported families")
		}
		format = witnessD64Format
	}
	// Format 6 prefixes the revision to the existing layout. This keeps each
	// older layout's rules for optional fields intact.
	var b []byte
	if w.Relations != nil && w.Relations.IsolationChecked {
		b = binary.AppendUvarint([]byte{witnessIsolationFormat}, uint64(len(w.Relations.IsolationDependencies)))
		for _, pair := range w.Relations.IsolationDependencies {
			b = appendMetaString(b, pair.SupportedLanguage)
			b = appendMetaString(b, pair.DeferredLanguage)
		}
	}
	if w.Contracts != nil {
		b = binary.AppendUvarint(append(b, witnessContractsFormat), w.Contracts.OmittedRecords)
	}
	if w.PlatformCatalogRevision != 0 {
		b = binary.BigEndian.AppendUint32(append(b, witnessCatalogFormat), w.PlatformCatalogRevision)
	}
	b = append(b, format, byte(w.Stage))
	b = appendMetaString(b, string(w.Profile))
	b = append(b, w.Input[:]...)
	b = append(b, w.Recipe[:]...)
	version := w.Version()
	b = append(b, version[:]...)
	b = binary.AppendUvarint(b, uint64(len(w.Dependencies)))
	for _, d := range w.Dependencies {
		b = append(b, d[:]...)
	}
	b = binary.AppendUvarint(b, uint64(len(w.Families)))
	var prev byte
	for _, f := range w.Families {
		if f.Family <= prev || f.Family == familyWitness {
			return nil, invalid("publication witness family order")
		}
		prev = f.Family
		b = append(b, f.Family)
		b = binary.AppendUvarint(b, f.Count)
		b = append(b, f.Digest[:]...)
	}
	if format >= witnessInputsFormat {
		b = append(b, w.FrozenInputs[:]...)
	}
	if format == witnessD64Format || w.UsesGitIgnorePolicy {
		coverage := byte(0)
		if format == witnessD64Format && w.UsesGitIgnorePolicy {
			coverage = 2
		}
		if w.Relations != nil {
			coverage |= 1
		}
		b = append(b, coverage)
	}
	if w.Relations != nil {
		ids := w.Relations.DeferredLanguages
		b = binary.AppendUvarint(b, uint64(len(ids)))
		var prev string
		for _, id := range ids {
			if id <= prev {
				return nil, invalid("publication deferred language order or empty ID")
			}
			b = appendMetaString(b, id)
			prev = id
		}
	}
	for _, f := range w.Unsupported {
		b = appendMetaString(b, f.Family)
		b = appendMetaString(b, f.Reason)
	}
	return b, nil
}

func decodeWitness(encodedWitness []byte) (Witness, error) {
	raw := encodedWitness
	checked := len(raw) != 0 && raw[0] == witnessIsolationFormat
	var dependencies []RebuildIsolationDependency
	if checked {
		count, n := binary.Uvarint(raw[1:])
		if n <= 0 || count > uint64((len(raw)-1-n)/4) {
			return Witness{}, invalid("publication isolation dependency count")
		}
		d := recordCursor{buf: raw[1+n:]}
		for range count {
			pair := RebuildIsolationDependency{SupportedLanguage: d.str(), DeferredLanguage: d.str()}
			if d.err != nil {
				return Witness{}, d.err
			}
			dependencies = append(dependencies, pair)
		}
		raw = d.buf
	}
	var contracts *ContractCoverage
	if len(raw) != 0 && raw[0] == witnessContractsFormat {
		omitted, n := binary.Uvarint(raw[1:])
		if n <= 0 {
			return Witness{}, invalid("publication omitted contract count")
		}
		contracts = &ContractCoverage{OmittedRecords: omitted}
		raw = raw[1+n:]
	}
	var revision uint32
	if len(raw) >= 5 && raw[0] == witnessCatalogFormat {
		revision = binary.BigEndian.Uint32(raw[1:5])
		raw = raw[5:]
	}
	if len(raw) < 2 || raw[0] < witnessFormat || raw[0] > witnessD64Format {
		return Witness{}, invalid("publication witness format")
	}
	d := recordCursor{buf: raw[2:]}
	w := Witness{Stage: Stage(raw[1]), Profile: Profile(d.str()), Contracts: contracts, PlatformCatalogRevision: revision}
	if d.err != nil || len(d.buf) < 96 {
		return Witness{}, invalid("truncated publication witness identity")
	}
	w.Input, w.Recipe = HeadDigest(d.buf[:32]), HeadDigest(d.buf[32:64])
	d.buf = d.buf[96:] // canonical re-encode below checks the stored version key
	deps, n := binary.Uvarint(d.buf)
	if n <= 0 || deps > uint64((len(d.buf)-n)/32) {
		return Witness{}, invalid("publication dependency count")
	}
	d.buf = d.buf[n:]
	for range deps {
		w.Dependencies = append(w.Dependencies, HeadDigest(d.buf[:32]))
		d.buf = d.buf[32:]
	}
	families, n := binary.Uvarint(d.buf)
	if n <= 0 || families > 255 || families > uint64((len(d.buf)-n)/34) {
		return Witness{}, invalid("publication family count")
	}
	d.buf = d.buf[n:]
	for range families {
		family := d.buf[0]
		count, n := binary.Uvarint(d.buf[1:])
		if n <= 0 || len(d.buf)-1-n < 32 {
			return Witness{}, invalid("truncated publication family")
		}
		d.buf = d.buf[1+n:]
		w.Families = append(w.Families, FamilyWitness{Family: family, Count: count, Digest: HeadDigest(d.buf[:32])})
		d.buf = d.buf[32:]
		if len(w.Families) < int(families) && len(d.buf) < 34 {
			return Witness{}, invalid("truncated publication families")
		}
	}
	if raw[0] >= witnessInputsFormat {
		if len(d.buf) < len(w.FrozenInputs) {
			return Witness{}, invalid("publication build-input reference")
		}
		w.FrozenInputs = HeadDigest(d.buf[:len(w.FrozenInputs)])
		d.buf = d.buf[len(w.FrozenInputs):]
	}
	hasCoverage := raw[0] == witnessCoverageFormat
	if raw[0] >= witnessGitPolicyFormat {
		maxFlag := byte(1)
		if raw[0] == witnessD64Format {
			maxFlag = 3
		}
		if len(d.buf) == 0 || d.buf[0] > maxFlag {
			return Witness{}, invalid("publication relation coverage flag")
		}
		w.UsesGitIgnorePolicy = raw[0] == witnessGitPolicyFormat || d.buf[0]&2 != 0
		hasCoverage = d.buf[0]&1 != 0
		d.buf = d.buf[1:]
	}
	if hasCoverage {
		count, n := binary.Uvarint(d.buf)
		if n <= 0 || count > uint64((len(d.buf)-n)/2) { // nonempty, length-framed IDs
			return Witness{}, invalid("publication deferred language count")
		}
		d.buf = d.buf[n:]
		w.Relations = &RelationCoverage{}
		for range count {
			id := d.str()
			if d.err != nil {
				return Witness{}, d.err
			}
			w.Relations.DeferredLanguages = append(w.Relations.DeferredLanguages, id)
		}
	}
	if raw[0] == witnessD64Format {
		for range len(deferredFamilies(w.Contracts != nil)) {
			f := UnsupportedFamily{Family: d.str(), Reason: d.str()}
			if d.err != nil {
				return Witness{}, d.err
			}
			w.Unsupported = append(w.Unsupported, f)
		}
	}
	if checked {
		if w.Relations == nil {
			return Witness{}, invalid("isolation proof without relation coverage")
		}
		w.Relations.IsolationChecked = true
		w.Relations.IsolationDependencies = dependencies
	}
	encoded, err := encodeWitness(w)
	if err != nil {
		return Witness{}, err
	}
	if !slices.Equal(encoded, encodedWitness) {
		return Witness{}, invalid("noncanonical publication witness")
	}
	return w, nil
}

// Publication reads the witness inside this request's point-in-time view.
func (s *Snapshot) Publication() (Witness, error) {
	w, err := point(s, publicationWitnessKey, decodeWitness)
	if err == nil && w.Stage == 0 {
		err = ErrNotPublished
	}
	return w, err
}

func (s *Snapshot) RequireStage(need Stage) error {
	w, err := s.Publication()
	if err != nil {
		return err
	}
	if w.Stage.rank() < need.rank() || (need == StageContracts && w.Contracts == nil) {
		return &ErrStagePending{Need: need, Published: w.Stage}
	}
	return nil
}

// RequireRelationCoverage checks the relation dependencies of a request using
// the witness from its pinned snapshot. An empty scope requires full coverage.
func (w Witness) RequireRelationCoverage(languages []string) error {
	if w.Stage == 0 {
		return ErrNotPublished
	}
	if w.Stage < StageRelations {
		return &ErrStagePending{Need: StageRelations, Published: w.Stage}
	}
	if w.Relations == nil {
		return &ErrLinksNotAvailable{}
	}
	if !w.Relations.IsolationChecked && len(w.Relations.DeferredLanguages) != 0 {
		// Mixed legacy heads do not prove isolation left supported answers intact.
		return &ErrLinksNotAvailable{Languages: slices.Clone(w.Relations.DeferredLanguages)}
	}
	missing := w.Relations.deferredInScope(languages)
	for _, pair := range w.Relations.IsolationDependencies {
		if len(languages) == 0 || slices.Contains(languages, pair.SupportedLanguage) {
			missing = append(missing, pair.DeferredLanguage)
		}
	}
	if len(missing) != 0 {
		slices.Sort(missing)
		return &ErrLinksNotAvailable{Languages: slices.Compact(missing)}
	}
	return nil
}

// Rows use the same length-framed stream in the writer and in an explicit
// verification scan. Counts and digests exclude the witness itself.
func writeWitnessRow(h *blake3.Hasher, key, value []byte) {
	var size [binary.MaxVarintLen64]byte
	for _, b := range [][]byte{key, value} {
		n := binary.PutUvarint(size[:], uint64(len(b)))
		_, _ = h.Write(size[:n])
		_, _ = h.Write(b)
	}
}

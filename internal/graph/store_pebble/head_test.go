package store_pebble

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
)

const (
	testHeadIncarnation        = "0198abcd-0123-7abc-8def-0123456789ab"
	testReplacementIncarnation = "0198abcd-0123-7abc-8def-0123456789ac"
)

func TestOpenHeadRejectsInvalidIncarnation(t *testing.T) {
	for name, incarnation := range map[string]string{
		"empty":         "",
		"zero":          "00000000-0000-0000-0000-000000000000",
		"uppercase":     strings.ToUpper(testHeadIncarnation),
		"wrong version": "0198abcd-0123-4abc-8def-0123456789ab",
		"wrong variant": "0198abcd-0123-7abc-0def-0123456789ab",
		"unhyphenated":  strings.ReplaceAll(testHeadIncarnation, "-", ""),
		"braced":        "{" + testHeadIncarnation + "}",
		"urn":           "urn:uuid:" + testHeadIncarnation,
		"malformed":     "z198abcd-0123-7abc-8def-0123456789ab",
		"overlong":      strings.Repeat("x", 4096),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "head")
			h, err := OpenHead(path, incarnation, FullProfile)
			if h != nil {
				t.Cleanup(func() { require.NoError(t, h.Close()) })
			}
			require.ErrorIs(t, err, ErrInvalidIncarnation)
			require.Nil(t, h)
			require.NoDirExists(t, path)
		})
	}
}

func makeStageTable(t *testing.T, h *Head, dir, name string, key, value []byte) {
	t.Helper()
	w, err := newTable(filepath.Join(dir, name), h.options.MakeWriterOptions(goldLevel, h.db.FormatMajorVersion().MaxTableFormat()))
	require.NoError(t, err)
	writeErr := w.Set(key, value)
	require.NoError(t, w.Close())
	require.NoError(t, writeErr)
}

func stageRead(t *testing.T, h *Head) *Snapshot {
	t.Helper()
	s := h.OpenSnapshot()
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

func TestManifestFrozenInputWitness(t *testing.T) {
	ctx := t.Context()
	w := Witness{Input: HeadDigest{1}, Recipe: HeadDigest{2}, Profile: FullProfile, Stage: StageManifest}
	legacy := []byte{1, byte(StageManifest), 4, 'f', 'u', 'l', 'l'}
	legacy = append(legacy, w.Input[:]...)
	legacy = append(legacy, w.Recipe[:]...)
	version := blake3.Sum256(append(w.Input[:], w.Recipe[:]...))
	require.Equal(t, HeadDigest(version), w.Version(), "legacy identity")
	legacy = append(legacy, version[:]...)
	legacy = append(legacy, 0, 0) // no dependencies or families
	raw, err := encodeWitness(w)
	require.NoError(t, err)
	require.Equal(t, legacy, raw, "no-reference witness bytes stay unchanged")
	got, err := decodeWitness(legacy)
	require.NoError(t, err)
	require.Equal(t, w, got)

	w.FrozenInputs = HeadDigest{3}
	raw, err = encodeWitness(w)
	require.NoError(t, err)
	require.EqualValues(t, 2, raw[0])
	got, err = decodeWitness(raw)
	require.NoError(t, err)
	require.Equal(t, w, got)
	_, err = decodeWitness(raw[:len(raw)-1])
	require.Error(t, err)
	clear(raw[len(legacy):])
	_, err = decodeWitness(raw)
	require.Error(t, err, "an explicit zero reference is not canonical")

	h, err := OpenHead(filepath.Join(t.TempDir(), "head"), testHeadIncarnation, w.Profile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	stage, err := PrepareManifest(ctx, t.TempDir(), nil, w, testHeadIncarnation)
	require.NoError(t, err)
	base, err := h.PublishStage(ctx, stage)
	require.NoError(t, err)
	got, err = stageRead(t, h).Publication()
	require.NoError(t, err)
	require.Equal(t, w.FrozenInputs, got.FrozenInputs)

	got.Stage, got.FrozenInputs = StageFileGraph, HeadDigest{4}
	_, err = h.PublishStage(ctx, StageTables{Dir: t.TempDir(), Base: base, Witness: got, Incarnation: testHeadIncarnation})
	require.ErrorIs(t, err, ErrStaleBase)
}

func TestWitnessCatalogRevisionEncoding(t *testing.T) {
	w := Witness{Input: HeadDigest{1}, Recipe: HeadDigest{2}, Profile: FullProfile, Stage: StageManifest, PlatformCatalogRevision: 7}
	raw, err := encodeWitness(w)
	require.NoError(t, err)
	require.Equal(t, []byte{witnessCatalogFormat, 0, 0, 0, 7, witnessFormat}, raw[:6])
	got, err := decodeWitness(raw)
	require.NoError(t, err)
	require.Equal(t, w, got)
	for n := range len(raw) {
		_, err := decodeWitness(raw[:n])
		require.ErrorIs(t, err, errInvalidRecord)
	}
	_, err = decodeWitness(append(bytes.Clone(raw[:5]), raw...))
	require.ErrorIs(t, err, errInvalidRecord, "the revision cannot be nested")
	raw[4]++ // the stored version still belongs to revision 7
	_, err = decodeWitness(raw)
	require.ErrorIs(t, err, errInvalidRecord)
	w.PlatformCatalogRevision = 0
	legacy, err := encodeWitness(w)
	require.NoError(t, err)
	_, err = decodeWitness(append([]byte{witnessCatalogFormat, 0, 0, 0, 0}, legacy...))
	require.ErrorIs(t, err, errInvalidRecord, "zero must keep its legacy encoding")
}

func TestManifestGitPolicyWitness(t *testing.T) {
	ctx := t.Context()
	w := Witness{Input: HeadDigest{1}, Recipe: HeadDigest{2}, Profile: FullProfile, Stage: StageManifest, UsesGitIgnorePolicy: true}
	_, err := encodeWitness(w)
	require.ErrorIs(t, err, errInvalidRecord)
	w.FrozenInputs = HeadDigest{3}
	unmarked := w
	unmarked.UsesGitIgnorePolicy = false
	require.NotEqual(t, w.Version(), unmarked.Version())
	raw, err := encodeWitness(w)
	require.NoError(t, err)
	require.Equal(t, witnessGitPolicyFormat, raw[0])
	got, err := decodeWitness(raw)
	require.NoError(t, err)
	require.Equal(t, w, got)
	_, err = decodeWitness(raw[:len(raw)-1])
	require.ErrorIs(t, err, errInvalidRecord)
	raw[len(raw)-1] = 2
	_, err = decodeWitness(raw)
	require.ErrorIs(t, err, errInvalidRecord)

	h, err := OpenHead(filepath.Join(t.TempDir(), "head"), testHeadIncarnation, w.Profile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	stage, err := PrepareManifest(ctx, t.TempDir(), nil, w, testHeadIncarnation)
	require.NoError(t, err)
	base, err := h.PublishStage(ctx, stage)
	require.NoError(t, err)
	got, err = stageRead(t, h).Publication()
	require.NoError(t, err)
	require.True(t, got.UsesGitIgnorePolicy)
	got.Stage, got.UsesGitIgnorePolicy = StageFileGraph, false
	_, err = h.PublishStage(ctx, StageTables{Dir: t.TempDir(), Base: base, Witness: got, Incarnation: testHeadIncarnation})
	require.ErrorIs(t, err, ErrStaleBase)

	w.Stage = StageRelations
	for _, c := range []*RelationCoverage{nil, {}, {DeferredLanguages: []string{"python"}}} {
		w.Relations = c
		raw, err := encodeWitness(w)
		require.NoError(t, err)
		got, err := decodeWitness(raw)
		require.NoError(t, err)
		require.Equal(t, w, got)
	}
}

func TestStageHeadOrderAndPinnedView(t *testing.T) {
	ctx := t.Context()
	h, err := OpenHead(filepath.Join(t.TempDir(), "head"), testHeadIncarnation, FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	require.ErrorIs(t, stageRead(t, h).RequireStage(StageManifest), ErrNotPublished)

	dir := t.TempDir()
	w := Witness{Input: HeadDigest{1}, Recipe: HeadDigest{2}, Profile: FullProfile, Stage: StageFileGraph}
	input := StageTables{Dir: dir, Incarnation: testHeadIncarnation, Witness: w}
	_, err = h.PublishStage(ctx, input)
	require.ErrorIs(t, err, ErrStageOrder)
	require.ErrorIs(t, stageRead(t, h).RequireStage(StageManifest), ErrNotPublished)

	key, value := []byte("Bmanifest"), []byte("two files")
	makeStageTable(t, h, dir, "manifest.sst", key, value)
	input.Files = []string{"manifest.sst"}
	input.Witness.Stage = StageManifest
	input.Witness.Families = []FamilyWitness{{Family: key[0], Count: 1, Digest: blake3.Sum256(value)}}
	base, err := h.PublishStage(ctx, input)
	require.NoError(t, err)
	require.NotEqual(t, HeadDigest{}, base)
	pinned := stageRead(t, h)
	pw, err := pinned.Publication()
	require.NoError(t, err)
	require.Equal(t, input.Witness, pw)
	saved, err := point(pinned, key, copyValue)
	require.NoError(t, err)
	require.Equal(t, value, saved)
	var pending *ErrStagePending
	require.ErrorAs(t, pinned.RequireStage(StageFileGraph), &pending)
	require.Equal(t, StageManifest, pending.Published)
	require.Equal(t, StageFileGraph, pending.Need)

	_, err = h.PublishStage(ctx, input)
	require.ErrorIs(t, err, ErrStageOrder, "repeat refuses even though ingest already consumed its files")
	input.Files = make([]string, goldFileCap)
	input.Witness.Stage = StageFileGraph
	input.Base = base
	_, err = h.PublishStage(ctx, input)
	require.ErrorIs(t, err, ErrStageTableLimit)
	fileKey, fileValue := []byte("Nname"), []byte("new declaration")
	makeStageTable(t, h, dir, "file.sst", fileKey, fileValue)
	input.Files = []string{"file.sst"}
	input.Witness.Families = append(input.Witness.Families, FamilyWitness{Family: fileKey[0], Count: 1, Digest: blake3.Sum256(fileValue)})
	input.Incarnation = testReplacementIncarnation
	_, err = h.PublishStage(ctx, input)
	require.ErrorIs(t, err, ErrStaleBase)
	unchanged := stageRead(t, h)
	require.ErrorAs(t, unchanged.RequireStage(StageFileGraph), &pending)
	notPublished, err := point(unchanged, fileKey, copyValue)
	require.NoError(t, err)
	require.Nil(t, notPublished)
	require.FileExists(t, filepath.Join(dir, "file.sst"))

	input.Incarnation = testHeadIncarnation
	input.Base = HeadDigest{99}
	_, err = h.PublishStage(ctx, input)
	require.ErrorIs(t, err, ErrStaleBase)
	input.Base = base
	fileBase, err := h.PublishStage(ctx, input)
	require.NoError(t, err)
	require.NotEqual(t, base, fileBase, "stages of one version must have distinct witnesses")
	current := stageRead(t, h)
	cw, err := current.Publication()
	require.NoError(t, err)
	require.Equal(t, pw.Version(), cw.Version())
	require.Equal(t, StageFileGraph, cw.Stage)
	require.Len(t, cw.Families, 2)
	require.NoError(t, current.RequireStage(StageFileGraph))
	seen, err := point(current, fileKey, copyValue)
	require.NoError(t, err)
	require.Equal(t, fileValue, seen)
	notSeen, err := point(pinned, fileKey, copyValue)
	require.NoError(t, err)
	require.Nil(t, notSeen)
	oldWitness, err := pinned.Publication()
	require.NoError(t, err)
	require.Equal(t, pw, oldWitness)
}

func TestStageWitnessRejectsMalformedValue(t *testing.T) {
	w := Witness{Input: HeadDigest{1}, Recipe: HeadDigest{2}, Profile: FullProfile, Stage: StageRelations,
		Dependencies: []HeadDigest{{3}}, Families: []FamilyWitness{{Family: 'A', Count: 128, Digest: HeadDigest{4}}}}
	raw, err := encodeWitness(w)
	require.NoError(t, err)
	require.NotEmpty(t, raw)
	got, err := decodeWitness(raw)
	require.NoError(t, err)
	require.Equal(t, w, got)
	for n := range len(raw) {
		_, err := decodeWitness(raw[:n])
		require.ErrorIs(t, err, errInvalidRecord)
	}
	bad := bytes.Clone(raw)
	bad[len(bad)-32-2-1] = familyWitness
	_, err = decodeWitness(bad)
	require.ErrorIs(t, err, errInvalidRecord)
	_, err = decodeWitness(append(bytes.Clone(raw), 0))
	require.ErrorIs(t, err, errInvalidRecord)
}

func TestContractWitnessEncoding(t *testing.T) {
	w := Witness{Input: HeadDigest{1}, Recipe: HeadDigest{2}, Profile: FullProfile, Stage: StageDerived,
		Unsupported: d64Unsupported[:]}
	old, err := encodeWitness(w)
	require.NoError(t, err)
	require.Equal(t, []byte{5, 4}, old[:2])
	got, err := decodeWitness(old)
	require.NoError(t, err)
	require.Equal(t, w, got)
	w.Stage = StageSearch
	old, err = encodeWitness(w)
	require.NoError(t, err)
	require.Equal(t, []byte{5, 5}, old[:2])
	got, err = decodeWitness(old)
	require.NoError(t, err)
	require.Equal(t, w, got)
	old[1] = byte(StageContracts)
	_, err = decodeWitness(old)
	require.ErrorIs(t, err, errInvalidRecord, "a legacy layout cannot declare contracts")

	w.Stage, w.Unsupported = StageContracts, nil
	w.PlatformCatalogRevision = 7
	w.Relations = &RelationCoverage{DeferredLanguages: []string{"python"}}
	for _, omissions := range []uint64{0, 2} {
		w.Contracts = &ContractCoverage{OmittedRecords: omissions}
		raw, err := encodeWitness(w)
		require.NoError(t, err)
		require.Equal(t, []byte{witnessContractsFormat, byte(omissions), witnessCatalogFormat}, raw[:3])
		got, err := decodeWitness(raw)
		require.NoError(t, err)
		require.Equal(t, w, got)
		for n := range len(raw) {
			_, err := decodeWitness(raw[:n])
			require.ErrorIs(t, err, errInvalidRecord)
		}
		_, err = decodeWitness(append(bytes.Clone(raw[:2]), raw...))
		require.ErrorIs(t, err, errInvalidRecord, "the contract coverage envelope cannot be nested")
	}
}

func TestContractStageWitnessOrder(t *testing.T) {
	ctx := t.Context()
	const inc = testHeadIncarnation
	h, err := OpenHead(filepath.Join(t.TempDir(), "head"), inc, FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	stage := StageTables{Incarnation: inc, Dir: t.TempDir(), Witness: Witness{
		Input: HeadDigest{1}, Recipe: HeadDigest{2}, Stage: StageManifest, Profile: FullProfile,
	}}
	for _, label := range []Stage{StageManifest, StageFileGraph, StageRelations} {
		stage.Witness.Stage = label
		stage.Base, err = h.PublishStage(ctx, stage)
		require.NoError(t, err)
	}
	old := stageRead(t, h)
	stage.Witness.Stage = StageContracts
	stage.Witness.Contracts = &ContractCoverage{OmittedRecords: 1}
	base, err := h.PublishStage(ctx, stage)
	require.NoError(t, err)
	_, err = h.PublishStage(ctx, stage)
	require.ErrorIs(t, err, ErrStageOrder)
	require.ErrorAs(t, old.RequireStage(StageContracts), new(*ErrStagePending))
	cur := stageRead(t, h)
	require.NoError(t, cur.RequireStage(StageContracts))
	require.ErrorAs(t, cur.RequireStage(StageDerived), new(*ErrStagePending))

	derived, err := PrepareDeferredDerived(t.TempDir(), stage)
	require.NoError(t, err)
	require.Equal(t, base, derived.Base)
	require.Equal(t, stage.Witness.Contracts, derived.Witness.Contracts)
	require.Len(t, derived.Witness.Unsupported, len(d64Unsupported)-1)
	for _, f := range derived.Witness.Unsupported {
		require.NotEqual(t, "contracts", f.Family)
	}
	derived.Witness.Contracts.OmittedRecords++
	_, err = h.PublishStage(ctx, derived)
	require.ErrorIs(t, err, errInvalidRecord)
	derived.Witness.Contracts.OmittedRecords--
	_, err = h.PublishStage(ctx, derived)
	require.NoError(t, err)
	require.NoError(t, stageRead(t, h).RequireStage(StageDerived))
	require.ErrorAs(t, cur.RequireStage(StageDerived), new(*ErrStagePending))
}

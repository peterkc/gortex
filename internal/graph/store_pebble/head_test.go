package store_pebble

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
)

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
	version := w.Version()
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

	h, err := OpenHead(filepath.Join(t.TempDir(), "head"), 1, w.Profile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	stage, err := PrepareManifest(ctx, t.TempDir(), nil, w, 1)
	require.NoError(t, err)
	base, err := h.PublishStage(ctx, stage)
	require.NoError(t, err)
	got, err = stageRead(t, h).Publication()
	require.NoError(t, err)
	require.Equal(t, w.FrozenInputs, got.FrozenInputs)

	got.Stage, got.FrozenInputs = StageFileGraph, HeadDigest{4}
	_, err = h.PublishStage(ctx, StageTables{Dir: t.TempDir(), Base: base, Witness: got, Incarnation: 1})
	require.ErrorIs(t, err, ErrStaleBase)
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

	h, err := OpenHead(filepath.Join(t.TempDir(), "head"), 1, w.Profile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	stage, err := PrepareManifest(ctx, t.TempDir(), nil, w, 1)
	require.NoError(t, err)
	base, err := h.PublishStage(ctx, stage)
	require.NoError(t, err)
	got, err = stageRead(t, h).Publication()
	require.NoError(t, err)
	require.True(t, got.UsesGitIgnorePolicy)
	got.Stage, got.UsesGitIgnorePolicy = StageFileGraph, false
	_, err = h.PublishStage(ctx, StageTables{Dir: t.TempDir(), Base: base, Witness: got, Incarnation: 1})
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
	h, err := OpenHead(filepath.Join(t.TempDir(), "head"), 7, FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	require.ErrorIs(t, stageRead(t, h).RequireStage(StageManifest), ErrNotPublished)

	dir := t.TempDir()
	w := Witness{Input: HeadDigest{1}, Recipe: HeadDigest{2}, Profile: FullProfile, Stage: StageFileGraph}
	input := StageTables{Dir: dir, Incarnation: 7, Witness: w}
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
	input.Files = nil
	input.Incarnation = 6
	_, err = h.PublishStage(ctx, input)
	require.ErrorIs(t, err, ErrStaleBase)
	input.Incarnation = 7
	input.Base = HeadDigest{99}
	_, err = h.PublishStage(ctx, input)
	require.ErrorIs(t, err, ErrStaleBase)
	require.ErrorAs(t, stageRead(t, h).RequireStage(StageFileGraph), &pending)

	fileKey, fileValue := []byte("Nname"), []byte("new declaration")
	makeStageTable(t, h, dir, "file.sst", fileKey, fileValue)
	input.Files = []string{"file.sst"}
	input.Witness.Families = append(input.Witness.Families, FamilyWitness{Family: fileKey[0], Count: 1, Digest: blake3.Sum256(fileValue)})
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

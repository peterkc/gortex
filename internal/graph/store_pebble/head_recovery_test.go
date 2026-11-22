package store_pebble

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/graph"
)

func TestHeadReconcilePublicationAfterMissedRecord(t *testing.T) {
	const incarnation = testHeadIncarnation
	path := filepath.Join(t.TempDir(), "head")
	var fileRecord, relationsRecord LocalPublicationRecord
	var relationsWitness Witness
	var relationsRaw []byte
	linked := &graph.Edge{From: "a.go::F", To: "a.go::G", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 3}
	mark := uint64(41)

	if !t.Run("ingest before catalog update", func(t *testing.T) {
		ctx := t.Context()
		h, err := OpenHead(path, incarnation, FullProfile)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, h.Close()) })
		worker, err := OpenBuild(filepath.Join(t.TempDir(), "worker"), FullProfile)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, worker.Close()) })
		manifest, err := PrepareManifest(ctx, t.TempDir(), []ManifestFile{{
			Path: "a.go", Language: "go", Size: 64, Kind: 'f', Content: HeadDigest{3}, Extractor: HeadDigest{4},
		}}, Witness{Input: HeadDigest{1}, Recipe: HeadDigest{2}, Profile: FullProfile, Stage: StageManifest}, incarnation)
		require.NoError(t, err)
		_, err = h.PublishStage(ctx, manifest)
		require.NoError(t, err)
		raw := *linked
		raw.To = "unresolved::G"
		require.NoError(t, worker.AddParsedFile("", ParseFacts{
			Nodes: []*graph.Node{
				{ID: linked.From, Name: "F", QualName: "a.F", Kind: graph.KindFunction, FilePath: "a.go", Language: "go"},
				{ID: linked.To, Name: "G", QualName: "a.G", Kind: graph.KindFunction, FilePath: "a.go", Language: "go"},
			},
			Edges: []*graph.Edge{&raw},
			File:  graph.FileMetaRow{FilePath: "a.go", ContentHash: "digest", Size: 64, NodeCount: 2},
		}))
		files, err := worker.PrepareFileGraph(ctx, t.TempDir(), manifest.Witness, incarnation)
		require.NoError(t, err)
		require.NotEmpty(t, files.Files)
		base, err := worker.RetainFileGraph(ctx, files)
		require.NoError(t, err)
		fileDigest, err := h.PublishStage(ctx, files)
		require.NoError(t, err)
		fileRecord = LocalPublicationRecord{
			Version: files.Witness.Version(), Incarnation: incarnation, WitnessDigest: fileDigest, EventID: &mark,
		}
		pinned := stageRead(t, h)

		worker.ReindexEdge(linked, raw.To)
		relations, err := worker.PrepareRelations(ctx, t.TempDir(), base, nil)
		require.NoError(t, err)
		require.NotEmpty(t, relations.Files)
		digest, err := h.PublishStage(ctx, relations)
		require.NoError(t, err)
		require.NotEqual(t, fileDigest, digest)
		require.Equal(t, fileRecord.Version, relations.Witness.Version())
		relationsWitness = relations.Witness
		relationsRecord = LocalPublicationRecord{Version: relationsWitness.Version(), Incarnation: incarnation, WitnessDigest: digest}
		relationsRaw, err = point(stageRead(t, h), publicationWitnessKey, copyValue)
		require.NoError(t, err)
		require.NotEmpty(t, relationsRaw)

		oldWitness, oldRecord, err := pinned.reconcilePublication(h.incarnation, &fileRecord)
		require.NoError(t, err)
		require.Equal(t, StageFileGraph, oldWitness.Stage)
		require.Equal(t, files.Witness, oldWitness)
		require.Equal(t, fileRecord, oldRecord)
		require.Equal(t, 2, pinned.NodeCount())
		require.Zero(t, pinned.EdgeCount())
		require.NoError(t, pinned.ReadError())
	}) {
		return
	}

	t.Run("replacement checkout incarnation", func(t *testing.T) {
		h, err := OpenHead(path, testReplacementIncarnation, FullProfile)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, h.Close()) })
		stale := relationsRecord
		stale.EventID = &mark
		next := relationsWitness
		next.Stage = StageDerived
		_, err = h.PublishStage(t.Context(), StageTables{
			Dir: t.TempDir(), Base: stale.WitnessDigest, Incarnation: stale.Incarnation, Witness: next,
		})
		require.ErrorIs(t, err, ErrStaleBase)
		w, record, err := h.ReconcilePublication(&stale)
		require.NoError(t, err)
		require.Equal(t, relationsWitness, w)
		require.Equal(t, LocalPublicationRecord{
			Version: stale.Version, Incarnation: testReplacementIncarnation, WitnessDigest: stale.WitnessDigest,
		}, record, "only the incarnation differs; the old watcher mark must not be kept")
		raw, err := point(stageRead(t, h), publicationWitnessKey, copyValue)
		require.NoError(t, err)
		require.Equal(t, relationsRaw, raw)
	})

	// Reopen the same durable head without a worker, stage files, or a new record.
	h, err := OpenHead(path, incarnation, FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	view := stageRead(t, h)
	raw, err := point(view, publicationWitnessKey, copyValue)
	require.NoError(t, err)
	require.Equal(t, relationsRaw, raw)
	require.Equal(t, 1, view.EdgeCount())
	require.Equal(t, []*graph.Edge{linked}, view.GetOutEdges(linked.From))
	require.NoError(t, view.ReadError())

	exact := relationsRecord
	exact.EventID = &mark
	wrongVersion, wrongIncarnation, wrongDigest := exact, exact, exact
	wrongVersion.Version[0] ^= 1
	wrongIncarnation.Incarnation = testReplacementIncarnation
	wrongDigest.WitnessDigest[0] ^= 1
	for _, tc := range []struct {
		name  string
		saved *LocalPublicationRecord
		want  LocalPublicationRecord
	}{
		{"missed update", &fileRecord, relationsRecord},
		{"no record", nil, relationsRecord},
		{"different version", &wrongVersion, relationsRecord},
		{"different incarnation", &wrongIncarnation, relationsRecord},
		{"different digest", &wrongDigest, relationsRecord},
		{"exact record", &exact, exact},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, record, err := h.ReconcilePublication(tc.saved)
			require.NoError(t, err)
			require.Equal(t, StageRelations, w.Stage)
			require.Equal(t, relationsWitness, w)
			require.Equal(t, tc.want, record)
		})
	}
	require.Equal(t, uint64(41), *fileRecord.EventID)
}

func TestHeadReconcilePublicationRefusesUnreadableWitness(t *testing.T) {
	h, err := OpenHead(filepath.Join(t.TempDir(), "head"), testHeadIncarnation, FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	malformed := []byte{witnessFormat, byte(StageRelations)} // truncated identity
	mark := uint64(41)
	saved := LocalPublicationRecord{Version: HeadDigest{1}, Incarnation: testHeadIncarnation, WitnessDigest: blake3.Sum256(malformed), EventID: &mark}
	w, record, err := h.ReconcilePublication(&saved)
	require.ErrorIs(t, err, ErrNotPublished)
	require.Zero(t, w)
	require.Zero(t, record)

	file, err := h.writeWitness(t.TempDir(), malformed)
	require.NoError(t, err)
	require.NoError(t, h.db.Ingest(t.Context(), []string{file}))
	w, record, err = h.ReconcilePublication(&saved)
	require.ErrorIs(t, err, ErrPublicationWitness)
	require.ErrorIs(t, err, errInvalidRecord)
	require.Zero(t, w)
	require.Zero(t, record)
}

package store_pebble

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildIsolationCoverageStages(t *testing.T) {
	ctx := t.Context()
	headDir := filepath.Join(t.TempDir(), "head")
	head, err := OpenHead(headDir, testHeadIncarnation, FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, head.Close()) })
	worker, err := OpenBuild(filepath.Join(t.TempDir(), "worker"), FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, worker.Close()) })
	manifest := ownerTestManifest(t, routeTestSource)
	_, err = head.PublishStage(ctx, manifest)
	require.NoError(t, err)
	require.NoError(t, worker.AddParsedFile("", ParseFacts{
		Nodes: []*graph.Node{{ID: "a.go::F", Name: "F", Kind: graph.KindFunction, FilePath: "a.go", Language: "go"}},
		File:  graph.FileMetaRow{FilePath: "a.go", Size: len(routeTestSource), ContentHash: fmt.Sprintf("%x", blake3.Sum256([]byte(routeTestSource))), NodeCount: 1},
	}))
	fileGraph, err := worker.PrepareFileGraph(ctx, t.TempDir(), manifest.Witness, testHeadIncarnation)
	require.NoError(t, err)
	base, err := worker.RetainFileGraph(ctx, fileGraph)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, fileGraph)
	require.NoError(t, err)

	file := RebuildIsolationAffectedScope{FilePath: "a.go", SupportedLanguage: "go", DeferredLanguage: "python", Cause: "held-endpoint"}
	corpus := RebuildIsolationAffectedScope{FilePath: "z.ts", SupportedLanguage: "typescript", DeferredLanguage: "python", Cause: "clone-corpus"}
	for _, event := range []string{"edge-2", "edge-1", "edge-2"} {
		require.NoError(t, worker.MarkRebuildIsolationAffected(ctx, file, event))
	}
	require.NoError(t, worker.MarkRebuildIsolationAffected(ctx, corpus, "body-1"))
	require.NoError(t, worker.CheckRebuildIsolationCoverage([]string{"python"}))
	relations, err := worker.PrepareRelations(ctx, t.TempDir(), base, nil)
	require.NoError(t, err)
	require.True(t, relations.Witness.Relations.IsolationChecked)
	require.Equal(t, []RebuildIsolationDependency{{"go", "python"}, {"typescript", "python"}}, relations.Witness.Relations.IsolationDependencies)
	require.EqualValues(t, 2, isolationFamily(relations.Witness).Count)
	var missing *ErrLinksNotAvailable
	require.ErrorAs(t, relations.Witness.RequireRelationCoverage([]string{"go"}), &missing)
	require.Equal(t, []string{"python"}, missing.Languages)
	require.NoError(t, relations.Witness.RequireRelationCoverage([]string{"javascript"}))

	bad := relations
	bad.Files = nil
	_, err = head.PublishStage(ctx, bad)
	require.ErrorIs(t, err, errInvalidRecord)
	bad = relations
	bad.Witness.Families = append([]FamilyWitness(nil), relations.Witness.Families...)
	for i := range bad.Witness.Families {
		if bad.Witness.Families[i].Family == familyIsolation {
			bad.Witness.Families[i].Digest[0] ^= 1
		}
	}
	_, err = head.PublishStage(ctx, bad)
	require.ErrorIs(t, err, errInvalidRecord)

	// Retain before publication consumes the outward files.
	retained, err := worker.RetainRelations(ctx, base, relations)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, relations)
	require.NoError(t, err)
	file.Count, corpus.Count = 2, 1
	assertCoverage := func(w Witness) {
		t.Helper()
		snap := head.OpenSnapshot()
		defer func() { require.NoError(t, snap.Close()) }()
		got, err := snap.Publication()
		require.NoError(t, err)
		require.Equal(t, w, got)
		require.Equal(t, w.Families, relationsSavedWitness(t, snap))
		require.ErrorAs(t, snap.RequireRelationCoverageContext(ctx, []string{"go"}, []string{"a.go"}), &missing)
		require.EqualError(t, missing, "links not available for python")
		require.Equal(t, []RebuildIsolationAffectedScope{file}, missing.Affected)
		require.NoError(t, snap.RequireRelationCoverageContext(ctx, []string{"go"}, []string{"other.go"}))
		require.NoError(t, snap.RequireRelationCoverageContext(ctx, []string{"javascript"}, nil))
		require.ErrorAs(t, snap.RequireRelationCoverageContext(ctx, []string{"typescript"}, []string{"z.ts"}), &missing)
		require.Equal(t, []RebuildIsolationAffectedScope{corpus}, missing.Affected)
		require.NoError(t, snap.RequireRelationCoverageContext(ctx, []string{"typescript"}, []string{"any.ts"}))
		require.ErrorAs(t, snap.RequireRelationCoverageContext(ctx, nil, nil), &missing)
		require.Equal(t, []RebuildIsolationAffectedScope{file, corpus}, missing.Affected)
	}
	assertCoverage(relations.Witness)
	contracts, err := worker.PrepareContracts(ctx, t.TempDir(), retained, contractBatches(), ContractCoverage{})
	require.NoError(t, err)
	bad = contracts
	bad.Witness.Relations = &RelationCoverage{DeferredLanguages: []string{"python"}}
	_, err = head.PublishStage(ctx, bad)
	require.ErrorIs(t, err, errInvalidRecord)
	_, err = head.PublishStage(ctx, contracts)
	require.NoError(t, err)
	assertCoverage(contracts.Witness)
	derived, err := PrepareDeferredDerived(t.TempDir(), contracts)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, derived)
	require.NoError(t, err)
	assertCoverage(derived.Witness)
	search, err := worker.PrepareSearch(ctx, t.TempDir(), derived.Witness, testHeadIncarnation, func(n *graph.Node) (string, bool) { return n.Name, true })
	require.NoError(t, err)
	digest, err := head.PublishStage(ctx, search)
	require.NoError(t, err)
	assertCoverage(search.Witness)
	bad = StageTables{Dir: t.TempDir(), Incarnation: testHeadIncarnation, Base: digest, Witness: search.Witness}
	bad.Witness.Relations = &RelationCoverage{IsolationChecked: true}
	bad.Witness.Families = nil
	_, err = head.PublishDelta(ctx, bad)
	require.ErrorIs(t, err, ErrCoverageRegression)
	assertCoverage(search.Witness)
	checkpoint := filepath.Join(t.TempDir(), "checkpoint")
	require.NoError(t, head.Checkpoint(checkpoint))
	copy, err := OpenHead(checkpoint, testHeadIncarnation, FullProfile)
	require.NoError(t, err)
	defer func() { require.NoError(t, copy.Close()) }()
	snap := copy.OpenSnapshot()
	defer func() { require.NoError(t, snap.Close()) }()
	require.ErrorAs(t, snap.RequireRelationCoverageContext(ctx, nil, nil), &missing)
	require.Equal(t, []RebuildIsolationAffectedScope{file, corpus}, missing.Affected)
}

func TestRebuildIsolationWitnessCompatibility(t *testing.T) {
	w := Witness{Stage: StageRelations, Profile: FullProfile, Input: HeadDigest{1}, Recipe: HeadDigest{2}, Relations: &RelationCoverage{DeferredLanguages: []string{"python"}}}
	raw, err := encodeWitness(w)
	require.NoError(t, err)
	got, err := decodeWitness(raw)
	require.NoError(t, err)
	require.ErrorAs(t, got.RequireRelationCoverage([]string{"go"}), new(*ErrLinksNotAvailable))
	w.Relations = &RelationCoverage{}
	require.NoError(t, w.RequireRelationCoverage([]string{"go"}))
	w.Relations = &RelationCoverage{IsolationChecked: true, DeferredLanguages: []string{"python"}, IsolationDependencies: []RebuildIsolationDependency{{"go", "python"}}}
	w.Families = []FamilyWitness{{Family: familyIsolation, Count: 1, Digest: HeadDigest{3}}}
	raw, err = encodeWitness(w)
	require.NoError(t, err)
	require.Equal(t, witnessIsolationFormat, raw[0])
	got, err = decodeWitness(raw)
	require.NoError(t, err)
	require.Equal(t, w, got)
	again, err := encodeWitness(got)
	require.NoError(t, err)
	require.Equal(t, raw, again)
	var wire bytes.Buffer
	require.NoError(t, gob.NewEncoder(&wire).Encode(w))
	got = Witness{}
	require.NoError(t, gob.NewDecoder(&wire).Decode(&got))
	require.Equal(t, w, got)
	for n := range len(raw) {
		_, err := decodeWitness(raw[:n])
		require.Error(t, err)
	}
	for _, pairs := range [][]RebuildIsolationDependency{
		{{"go", "python"}, {"go", "python"}},
		{{"typescript", "python"}, {"go", "python"}},
		{{"go", "ruby"}}, {{"python", "python"}},
	} {
		bad := w
		bad.Relations = &RelationCoverage{IsolationChecked: true, DeferredLanguages: []string{"python"}, IsolationDependencies: pairs}
		bad.Families = []FamilyWitness{{Family: familyIsolation, Count: 2, Digest: HeadDigest{3}}}
		_, err := encodeWitness(bad)
		require.ErrorIs(t, err, errInvalidRecord)
	}
}

func TestRebuildIsolationRejectsInvalidScope(t *testing.T) {
	for _, scope := range []RebuildIsolationAffectedScope{
		{SupportedLanguage: "go", DeferredLanguage: "python", Cause: "held-endpoint"},
		{SupportedLanguage: "go", DeferredLanguage: "python", Cause: "clone-corpus"},
		{FilePath: "a.go", SupportedLanguage: "go", DeferredLanguage: "python", Cause: "unknown"},
		{FilePath: "a.go", SupportedLanguage: "python", DeferredLanguage: "ruby", Cause: "held-endpoint"},
		{FilePath: "a.go", SupportedLanguage: "go", DeferredLanguage: "go", Cause: "held-endpoint"},
	} {
		_, err := isolationScopeKey(scope)
		require.ErrorIs(t, err, errInvalidRecord)
	}
	key, err := isolationScopeKey(RebuildIsolationAffectedScope{FilePath: "a.go", SupportedLanguage: "go", DeferredLanguage: "python", Cause: "held-endpoint"})
	require.NoError(t, err)
	_, err = decodeIsolationScope(key, make([]byte, 8))
	require.ErrorIs(t, err, errInvalidRecord)
}

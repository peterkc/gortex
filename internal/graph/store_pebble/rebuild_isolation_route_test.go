package store_pebble

import (
	"context"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/graph"
)

// Exercise the real seals; a nil scope list represents the old mixed witness.
func ownerIsolationStages(t *testing.T, source string, scopes []RebuildIsolationAffectedScope) []StageTables {
	t.Helper()
	ctx := t.Context()
	worker, err := OpenBuild(filepath.Join(t.TempDir(), "worker"), FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, worker.Close()) })
	var manifestFiles []ManifestFile
	for _, file := range []struct{ path, language string }{{"a.go", "go"}, {"b.ts", "typescript"}, {"c.py", "python"}} {
		digest := blake3.Sum256([]byte(source + file.path))
		manifestFiles = append(manifestFiles, ManifestFile{
			Path: file.path, Language: file.language, Kind: 'f', Size: int64(len(source) + len(file.path)), Content: digest, Extractor: HeadDigest{3},
		})
		require.NoError(t, worker.AddParsedFile("", ParseFacts{
			Nodes: []*graph.Node{{ID: file.path + "::F", Name: "F", Kind: graph.KindFunction, FilePath: file.path, Language: file.language}},
			File:  graph.FileMetaRow{FilePath: file.path, Size: len(source) + len(file.path), ContentHash: fmt.Sprintf("%x", digest), NodeCount: 1},
		}))
	}
	manifest, err := PrepareManifest(ctx, t.TempDir(), manifestFiles, Witness{
		Input: blake3.Sum256([]byte(source)), Recipe: HeadDigest{1}, Stage: StageManifest, Profile: FullProfile,
	}, testHeadIncarnation)
	require.NoError(t, err)
	files, err := worker.PrepareFileGraph(ctx, t.TempDir(), manifest.Witness, manifest.Incarnation)
	require.NoError(t, err)
	fileBase, err := worker.RetainFileGraph(ctx, files)
	require.NoError(t, err)
	deferred := []string{"python"}
	if scopes != nil {
		for _, scope := range scopes {
			require.NoError(t, worker.MarkRebuildIsolationAffected(ctx, scope, "event"))
		}
		require.NoError(t, worker.CheckRebuildIsolationCoverage(deferred))
	}
	relations, err := worker.PrepareRelations(ctx, t.TempDir(), fileBase, nil)
	require.NoError(t, err)
	if scopes == nil {
		relations.Witness.Relations = &RelationCoverage{DeferredLanguages: deferred}
	}
	relationBase, err := worker.RetainRelations(ctx, fileBase, relations)
	require.NoError(t, err)
	contracts, err := worker.PrepareContracts(ctx, t.TempDir(), relationBase, contractBatches(), ContractCoverage{})
	require.NoError(t, err)
	derived, err := PrepareDeferredDerived(t.TempDir(), contracts)
	require.NoError(t, err)
	search, err := worker.PrepareSearch(ctx, t.TempDir(), derived.Witness, derived.Incarnation, func(n *graph.Node) (string, bool) {
		return n.Name, true
	})
	require.NoError(t, err)
	return []StageTables{manifest, files, relations, contracts, derived, search}
}

func TestRouteOwnerIsolationCoverageReplacement(t *testing.T) {
	ctx := t.Context()
	o := ownerTestInit(t)
	first := RebuildIsolationAffectedScope{FilePath: "b.ts", SupportedLanguage: "typescript", DeferredLanguage: "python", Cause: "clone-corpus"}
	added := RebuildIsolationAffectedScope{FilePath: "a.go", SupportedLanguage: "go", DeferredLanguage: "python", Cause: "clone-corpus"}
	oldStages := ownerIsolationStages(t, routeTestSource, []RebuildIsolationAffectedScope{first})
	old, err := o.begin(ctx, routeTestOldLease, nil, oldStages[0].Witness)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, old.head.Close()) })
	ownerTestPublish(t, old, oldStages)
	oldRow := ownerTestRow(t, o)

	newStages := ownerIsolationStages(t, routeTestSource+"// edit\n", []RebuildIsolationAffectedScope{first, added})
	next, err := o.begin(ctx, routeTestNewLease, old.head, newStages[0].Witness)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, next.head.Close()) })
	ownerTestPublish(t, next, newStages[:5])
	reserved := oldRow
	reserved.Candidate = next.locator
	require.Equal(t, reserved, ownerTestRow(t, o))
	ownerTestPublish(t, next, newStages[5:])
	row := ownerTestRow(t, o)
	require.Equal(t, next.locator, row.Serving)
	require.Zero(t, row.Candidate)
	require.NotEqual(t, oldRow.Publication.Version, row.Publication.Version)
	snap := stageRead(t, next.head)
	added.Count = 1
	var missing *ErrLinksNotAvailable
	require.ErrorAs(t, snap.RequireRelationCoverageContext(ctx, []string{"go"}, []string{"a.go"}), &missing)
	require.Equal(t, []RebuildIsolationAffectedScope{added}, missing.Affected)
	require.NoError(t, snap.RequireRelationCoverageContext(ctx, []string{"go"}, []string{"other.go"}))
	unchanged, err := o.begin(ctx, ownerThirdLease, next.head, newStages[0].Witness)
	require.NoError(t, err)
	require.Nil(t, unchanged, "a checked complete version still skips the rebuild")

	legacy := ownerIsolationStages(t, routeTestSource+"// later\n", nil)
	weaker, err := o.begin(ctx, ownerThirdLease, next.head, legacy[0].Witness)
	require.NoError(t, err)
	t.Cleanup(func() {
		if weaker.head != nil {
			require.NoError(t, weaker.head.Close())
		}
	})
	ownerTestPublish(t, weaker, legacy[:5])
	require.ErrorIs(t, weaker.publishStage(ctx, legacy[5]), ErrCoverageRegression)
	row.Candidate = weaker.locator
	require.Equal(t, row, ownerTestRow(t, o))
}

func TestRouteOwnerLegacyMixedRecovery(t *testing.T) {
	ctx := t.Context()
	o := ownerTestInit(t)
	legacy := ownerIsolationStages(t, routeTestSource, nil)
	old, err := o.begin(ctx, routeTestOldLease, nil, legacy[0].Witness)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, old.head.Close()) })
	ownerTestPublish(t, old, legacy)
	require.NoError(t, o.retainCheckpoint(ctx, old.head, ownerThirdLease))
	retained := ownerTestRow(t, o).Retained

	scope := RebuildIsolationAffectedScope{FilePath: "a.go", SupportedLanguage: "go", DeferredLanguage: "python", Cause: "held-endpoint"}
	checked := ownerIsolationStages(t, routeTestSource, []RebuildIsolationAffectedScope{scope})
	next, err := o.begin(ctx, routeTestNewLease, old.head, checked[0].Witness)
	require.NoError(t, err)
	require.NotNil(t, next, "legacy mixed version cannot skip a fresh rebuild")
	t.Cleanup(func() {
		if next.head != nil {
			require.NoError(t, next.head.Close())
		}
	})
	ownerTestPublish(t, next, checked)
	require.Equal(t, next.locator, ownerTestRow(t, o).Serving)
	key, err := isolationScopeKey(scope)
	require.NoError(t, err)
	err = next.head.db.Set(key, binary.BigEndian.AppendUint64(nil, 2), pebble.Sync)
	require.NoError(t, err)
	closeErr := next.head.Close()
	next.head = nil
	require.NoError(t, closeErr)

	// The external validator is intentionally permissive. The owner must reject
	// the changed L digest before calling it, then present only the fallback.
	var validated []Witness
	r, err := o.recover(ctx, func(_ context.Context, _ *Snapshot, w Witness) error {
		validated = append(validated, w)
		return nil
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Head.Close()) })
	require.Equal(t, []Witness{legacy[5].Witness}, validated)
	require.ErrorContains(t, r.Failure, "isolation rows differ from witness")
	require.True(t, r.LastGood)
	require.True(t, r.Requeue)
	require.Equal(t, retained.Locator, ownerTestRow(t, o).Serving)
	require.Equal(t, retained.Publication, r.Publication)
	require.Equal(t, retained.Publication, ownerTestWitness(t, r.Head, legacy[5].Witness))
	require.ErrorAs(t, stageRead(t, r.Head).RequireRelationCoverageContext(ctx, []string{"go"}, []string{"a.go"}), new(*ErrLinksNotAvailable))
}

func TestRouteOwnerDeltaCannotEstablishIsolationProof(t *testing.T) {
	ctx := t.Context()
	o := ownerTestInit(t)
	legacy := ownerIsolationStages(t, routeTestSource, nil)
	old, err := o.begin(ctx, routeTestOldLease, nil, legacy[0].Witness)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, old.head.Close()) })
	ownerTestPublish(t, old, legacy)
	row := ownerTestRow(t, o)
	w := legacy[5].Witness
	delta := StageTables{Dir: t.TempDir(), Incarnation: o.incarnation, Base: row.Publication.WitnessDigest, Witness: w}
	for _, nonempty := range []bool{false, true} {
		bad := delta
		bad.Witness.Relations = &RelationCoverage{IsolationChecked: true, DeferredLanguages: []string{"python"}}
		if nonempty {
			bad.Witness.Relations.IsolationDependencies = []RebuildIsolationDependency{{"go", "python"}}
			bad.Witness.Families = append(slices.Clone(w.Families), FamilyWitness{Family: familyIsolation, Count: 1, Digest: HeadDigest{3}})
			slices.SortFunc(bad.Witness.Families, func(a, b FamilyWitness) int { return int(a.Family) - int(b.Family) })
		}
		raw, err := encodeWitness(bad.Witness)
		require.NoError(t, err)
		require.Equal(t, witnessIsolationFormat, raw[0])
		require.ErrorIs(t, o.publishDelta(ctx, old.head, bad), ErrCoverageRegression)
		require.Equal(t, row, ownerTestRow(t, o))
		require.Equal(t, row.Publication, ownerTestWitness(t, old.head, w))
	}
	require.NoError(t, o.publishDelta(ctx, old.head, delta), "unchanged legacy coverage remains admissible")
	require.Equal(t, row, ownerTestRow(t, o))
}

func TestRouteOwnerDeltaPreservesIsolationRows(t *testing.T) {
	ctx := t.Context()
	o := ownerTestInit(t)
	scope := RebuildIsolationAffectedScope{FilePath: "a.go", SupportedLanguage: "go", DeferredLanguage: "python", Cause: "held-endpoint"}
	stages := ownerIsolationStages(t, routeTestSource, []RebuildIsolationAffectedScope{scope})
	b, err := o.begin(ctx, routeTestOldLease, nil, stages[0].Witness)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.head.Close()) })
	ownerTestPublish(t, b, stages)
	row := ownerTestRow(t, o)
	w := stages[5].Witness
	delta := StageTables{Dir: t.TempDir(), Incarnation: o.incarnation, Base: row.Publication.WitnessDigest, Witness: w}
	key, err := isolationScopeKey(scope)
	require.NoError(t, err)
	for _, tc := range []struct {
		name string
		want string
	}{
		{"set", "publication changes sealed isolation family"},
		{"delete", "isolation stage row is not a set"},
		{"range-delete", "publication stage uses range deletion"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := delta
			bad.Dir = t.TempDir()
			bad.Files = []string{"isolation.sst"}
			writer, err := newTable(filepath.Join(bad.Dir, bad.Files[0]), b.head.options.MakeWriterOptions(goldLevel, b.head.db.FormatMajorVersion().MaxTableFormat()))
			require.NoError(t, err)
			var writeErr error
			switch tc.name {
			case "set":
				writeErr = writer.Set(key, binary.BigEndian.AppendUint64(nil, 2))
			case "delete":
				writeErr = writer.Delete(key)
			case "range-delete":
				writeErr = writer.DeleteRange([]byte{familyIsolation}, prefixEnd([]byte{familyIsolation}))
			}
			require.NoError(t, writer.Close())
			require.NoError(t, writeErr)
			err = o.publishDelta(ctx, b.head, bad)
			require.ErrorIs(t, err, errInvalidRecord)
			require.ErrorContains(t, err, tc.want)
			require.Equal(t, row, ownerTestRow(t, o))
			require.Equal(t, row.Publication, ownerTestWitness(t, b.head, w))
		})
	}
	require.NoError(t, o.publishDelta(ctx, b.head, delta), "unchanged checked coverage remains admissible")
	require.Equal(t, row, ownerTestRow(t, o))
	require.Equal(t, row.Publication, ownerTestWitness(t, b.head, w))
	var missing *ErrLinksNotAvailable
	require.ErrorAs(t, stageRead(t, b.head).RequireRelationCoverageContext(ctx, []string{"go"}, []string{"a.go"}), &missing)
	scope.Count = 1
	require.Equal(t, []RebuildIsolationAffectedScope{scope}, missing.Affected)
}

func TestCoversRelationsAtLeastCheckedCensus(t *testing.T) {
	unchecked := &RelationCoverage{DeferredLanguages: []string{"python"}}
	checked := &RelationCoverage{DeferredLanguages: []string{"python"}, IsolationChecked: true}
	larger := &RelationCoverage{DeferredLanguages: []string{"python", "ruby"}, IsolationChecked: true}
	complete := &RelationCoverage{IsolationChecked: true}
	for _, tc := range []struct {
		name        string
		prior, next *RelationCoverage
		want        bool
	}{
		{"legacy without declaration", nil, checked, true},
		{"cannot lose declaration", checked, nil, false},
		{"cannot lose checked proof", checked, unchecked, false},
		{"legacy mixed has no checked supported coverage", unchecked, larger, true},
		{"cannot grow checked deferred census", checked, larger, false},
		{"can reduce checked deferred census", larger, checked, true},
		{"complete cannot become mixed", complete, checked, false},
		{"can become complete", checked, complete, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prior, next := Witness{Relations: tc.prior}, Witness{Relations: tc.next}
			require.Equal(t, tc.want, next.CoversRelationsAtLeast(prior))
		})
	}
}

func TestRebuildIsolationCensusRefusals(t *testing.T) {
	scope := RebuildIsolationAffectedScope{FilePath: "a.go", SupportedLanguage: "go", DeferredLanguage: "python", Cause: "held-endpoint"}
	t.Run("outside census", func(t *testing.T) {
		ctx := t.Context()
		worker, files := prepareRouteTestFileGraph(t, ownerTestManifest(t, routeTestSource))
		base, err := worker.RetainFileGraph(ctx, files)
		require.NoError(t, err)
		require.NoError(t, worker.MarkRebuildIsolationAffected(ctx, scope, "event"))
		require.NoError(t, worker.CheckRebuildIsolationCoverage([]string{"ruby"}))
		tables, err := worker.PrepareRelations(ctx, t.TempDir(), base, nil)
		require.ErrorIs(t, err, ErrRelationsSeal)
		require.ErrorContains(t, err, "isolation event outside deferred census")
		require.Zero(t, tables)
	})
	for _, tc := range []struct {
		name string
		ids  []string
	}{
		{"empty ID", []string{""}},
		{"unordered", []string{"ruby", "python"}},
		{"duplicate", []string{"python", "python"}},
		{"linked", []string{"go"}},
		{"NUL", []string{"py\x00thon"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker, err := OpenBuild(filepath.Join(t.TempDir(), "worker"), FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, worker.Close()) })
			require.ErrorContains(t, worker.CheckRebuildIsolationCoverage(tc.ids), "invalid isolation deferred census")
		})
	}
	for _, mode := range []string{"already checked", "cut open"} {
		t.Run(mode, func(t *testing.T) {
			worker, err := OpenBuild(filepath.Join(t.TempDir(), "worker"), FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, worker.Close()) })
			if mode == "already checked" {
				require.NoError(t, worker.CheckRebuildIsolationCoverage([]string{"python"}))
			} else {
				require.NoError(t, worker.IsolateRebuildLinks(t.Context()))
			}
			require.ErrorContains(t, worker.CheckRebuildIsolationCoverage([]string{"ruby"}), "isolation census already checked or cut still open")
		})
	}
	t.Run("late event", func(t *testing.T) {
		worker, err := OpenBuild(filepath.Join(t.TempDir(), "worker"), FullProfile)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, worker.Close()) })
		require.NoError(t, worker.CheckRebuildIsolationCoverage([]string{"python"}))
		require.ErrorContains(t, worker.MarkRebuildIsolationAffected(t.Context(), scope, "event"), "isolation event after census or invalid event")
	})
}

func TestRebuildIsolationContractsRefusesChangedScope(t *testing.T) {
	ctx := t.Context()
	worker, files := prepareRouteTestFileGraph(t, ownerTestManifest(t, routeTestSource))
	fileBase, err := worker.RetainFileGraph(ctx, files)
	require.NoError(t, err)
	scope := RebuildIsolationAffectedScope{FilePath: "a.go", SupportedLanguage: "go", DeferredLanguage: "python", Cause: "held-endpoint"}
	require.NoError(t, worker.MarkRebuildIsolationAffected(ctx, scope, "event"))
	require.NoError(t, worker.CheckRebuildIsolationCoverage([]string{"python"}))
	relations, err := worker.PrepareRelations(ctx, t.TempDir(), fileBase, nil)
	require.NoError(t, err)
	require.EqualValues(t, 1, isolationFamily(relations.Witness).Count)
	base, err := worker.RetainRelations(ctx, fileBase, relations)
	require.NoError(t, err)

	// A well-formed but different count reaches the Contracts diff guard.
	key, err := isolationScopeKey(scope)
	require.NoError(t, err)
	require.NoError(t, worker.pending.Set(key, binary.BigEndian.AppendUint64(nil, 2), pebble.NoSync))
	tables, err := worker.PrepareContracts(ctx, t.TempDir(), base, contractBatches(), ContractCoverage{})
	require.ErrorIs(t, err, ErrContractsSeal)
	require.ErrorContains(t, err, "contracts changed sealed isolation scope")
	require.Zero(t, tables)
}

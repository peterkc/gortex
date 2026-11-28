package store_pebble

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/graph"
)

const ownerThirdLease = "0198abcd-0123-7abc-8def-0123456789b0"
const ownerFourthLease = "0198abcd-0123-7abc-8def-0123456789b1"

func ownerTestCatalog(t *testing.T, root string) *routeOwner {
	t.Helper()
	catalog, err := openRouteCatalog(filepath.Join(root, "catalog"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	return &routeOwner{catalog: catalog, checkout: root, root: root, incarnation: testHeadIncarnation, profile: FullProfile}
}

func ownerTestInit(t *testing.T) *routeOwner {
	t.Helper()
	o := ownerTestCatalog(t, t.TempDir())
	require.NoError(t, o.catalog.initCheckout(t.Context(), o.checkout, o.incarnation))
	return o
}

func ownerTestRow(t *testing.T, o *routeOwner) routeCheckout {
	t.Helper()
	row, err := o.lookup()
	require.NoError(t, err)
	return row
}

func ownerTestManifest(t *testing.T, source string) StageTables {
	t.Helper()
	digest := blake3.Sum256([]byte(source))
	manifest, err := PrepareManifest(t.Context(), t.TempDir(), []ManifestFile{{
		Path: "a.go", Language: "go", Kind: 'f', Size: int64(len(source)), Content: digest, Extractor: HeadDigest{3},
	}}, Witness{Input: digest, Recipe: HeadDigest{1}, Stage: StageManifest, Profile: FullProfile}, testHeadIncarnation)
	require.NoError(t, err)
	return manifest
}

// One ordinary declaration, no contract facts. Exercise the real store seals
// through all six stages; this is not the C3 worker or its election fixture.
func ownerTestStages(t *testing.T, source string, stop Stage, deferred []string) []StageTables {
	t.Helper()
	ctx := t.Context()
	manifest := ownerTestManifest(t, source)
	stages := []StageTables{manifest}
	if stop == StageManifest {
		return stages
	}
	worker, err := OpenBuild(filepath.Join(t.TempDir(), "worker"), FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, worker.Close()) })
	require.NoError(t, worker.AddParsedFile("", ParseFacts{
		Nodes: []*graph.Node{{ID: "a.go::F", Name: "F", Kind: graph.KindFunction, FilePath: "a.go", Language: "go"}},
		File:  graph.FileMetaRow{FilePath: "a.go", Size: len(source), ContentHash: fmt.Sprintf("%x", blake3.Sum256([]byte(source))), NodeCount: 1},
	}))
	files, err := worker.PrepareFileGraph(ctx, t.TempDir(), manifest.Witness, manifest.Incarnation)
	require.NoError(t, err)
	stages = append(stages, files)
	if stop == StageFileGraph {
		return stages
	}
	fileBase, err := worker.RetainFileGraph(ctx, files)
	require.NoError(t, err)
	relations, err := worker.PrepareRelations(ctx, t.TempDir(), fileBase, nil)
	require.NoError(t, err)
	relations.Witness.Relations = &RelationCoverage{DeferredLanguages: deferred}
	stages = append(stages, relations)
	if stop == StageRelations {
		return stages
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
	return append(stages, contracts, derived, search)
}

func ownerTestWitness(t *testing.T, h *Head, want Witness) LocalPublicationRecord {
	t.Helper()
	snap := h.OpenSnapshot()
	defer func() { require.NoError(t, snap.Close()) }()
	w, receipt, err := snap.reconcilePublication(h.incarnation, nil)
	require.NoError(t, err)
	require.Equal(t, want, w)
	require.Equal(t, want.Families, relationsSavedWitness(t, snap))
	require.NoError(t, snap.ReadError())
	return receipt
}

func ownerTestPublish(t *testing.T, b *routeBuild, stages []StageTables) {
	t.Helper()
	for _, tables := range stages {
		require.NoError(t, b.publishStage(t.Context(), tables))
		receipt := ownerTestWitness(t, b.head, tables.Witness)
		row := ownerTestRow(t, b.owner)
		if row.Serving == b.locator {
			require.Equal(t, receipt, row.Publication)
			require.Zero(t, row.Candidate)
		}
	}
}

func TestRouteOwnerBeginRefusesStaleOrOccupiedBase(t *testing.T) {
	ctx := t.Context()
	o := ownerTestInit(t)
	manifest := ownerTestManifest(t, routeTestSource)
	fresh := ownerTestRow(t, o)
	request := manifest.Witness
	request.Profile = DefaultProfile
	b, err := o.begin(ctx, routeTestOldLease, nil, request)
	require.ErrorIs(t, err, ErrStaleBase, "profile mismatch")
	require.Nil(t, b)
	require.NoDirExists(t, o.locator(routeTestOldLease).Directory)
	require.Equal(t, fresh, ownerTestRow(t, o))

	// An existing directory is not a catalog reservation and is never reused.
	occupied := o.locator(ownerThirdLease).Directory
	require.NoError(t, os.Mkdir(occupied, 0o700))
	marker := filepath.Join(occupied, "keep")
	require.NoError(t, os.WriteFile(marker, []byte("keep"), 0o600))
	b, err = o.begin(ctx, ownerThirdLease, nil, manifest.Witness)
	require.ErrorIs(t, err, ErrStaleBase, "occupied lease directory")
	require.Nil(t, b)
	require.FileExists(t, marker)
	require.Equal(t, fresh, ownerTestRow(t, o))

	other, err := OpenHead(o.locator(ownerFourthLease).Directory, o.incarnation, o.profile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close()) })
	_, err = other.PublishStage(ctx, ownerTestManifest(t, routeTestSource+"// unrelated\n"))
	require.NoError(t, err)
	b, err = o.begin(ctx, routeTestOldLease, other, manifest.Witness)
	require.ErrorIs(t, err, ErrStaleBase, "prior without a serving locator")
	require.Nil(t, b)
	require.Equal(t, fresh, ownerTestRow(t, o))

	serving, err := o.begin(ctx, routeTestOldLease, nil, manifest.Witness)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, serving.head.Close()) })
	reserved := ownerTestRow(t, o)
	b, err = o.begin(ctx, routeTestNewLease, nil, manifest.Witness)
	require.ErrorIs(t, err, ErrStaleBase, "another candidate is already registered")
	require.Nil(t, b)
	require.NoDirExists(t, o.locator(routeTestNewLease).Directory)
	require.Equal(t, reserved, ownerTestRow(t, o))

	ownerTestPublish(t, serving, []StageTables{manifest})
	selected := ownerTestRow(t, o)
	b, err = o.begin(ctx, routeTestNewLease, nil, manifest.Witness)
	require.ErrorIs(t, err, ErrStaleBase, "serving locator without its prior handle")
	require.Nil(t, b)
	require.Equal(t, selected, ownerTestRow(t, o))
	b, err = o.begin(ctx, routeTestNewLease, other, manifest.Witness)
	require.ErrorIs(t, err, ErrStaleBase, "prior handle from an unrelated directory")
	require.Nil(t, b)
	require.NoDirExists(t, o.locator(routeTestNewLease).Directory)
	require.Equal(t, selected, ownerTestRow(t, o))
}

func TestRouteOwnerPublishStageFencesSupersededWorker(t *testing.T) {
	ctx := t.Context()
	o := ownerTestInit(t)
	stages := ownerTestStages(t, routeTestSource, StageFileGraph, nil)
	old, err := o.begin(ctx, routeTestOldLease, nil, stages[0].Witness)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, old.head.Close()) })
	ownerTestPublish(t, old, stages[:1])
	selected := ownerTestRow(t, o)
	next, err := o.begin(ctx, routeTestNewLease, old.head, ownerTestManifest(t, routeTestSource+"// replacement\n").Witness)
	require.NoError(t, err)
	t.Cleanup(func() {
		if next.head != nil {
			require.NoError(t, next.head.Close())
		}
	})
	reserved := ownerTestRow(t, o)
	require.ErrorIs(t, old.publishStage(ctx, stages[1]), ErrStaleBase)
	require.Equal(t, selected.Publication, ownerTestWitness(t, old.head, stages[0].Witness))
	require.Equal(t, reserved, ownerTestRow(t, o))

	// The same FileGraph offer is admissible once there is no replacement.
	failure := errors.New("replacement failed before first publication")
	require.ErrorIs(t, next.failBuild(ctx, failure), failure)
	require.Equal(t, selected, ownerTestRow(t, o))
	ownerTestPublish(t, old, stages[1:])
}

func TestRouteOwnerPublishStageRequiresContractCoverageInOrder(t *testing.T) {
	ctx := t.Context()
	o := ownerTestInit(t)
	stages := ownerTestStages(t, routeTestSource, StageSearch, nil)
	b, err := o.begin(ctx, routeTestOldLease, nil, stages[0].Witness)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.head.Close()) })
	for i, tables := range stages[:3] {
		before := ownerTestRow(t, o)
		early := tables
		early.Witness.Contracts = &ContractCoverage{}
		require.ErrorContains(t, b.publishStage(ctx, early), "publication contract coverage stage", tables.Witness.Stage)
		require.Equal(t, before, ownerTestRow(t, o))
		if i == 0 {
			require.ErrorIs(t, stageRead(t, b.head).RequireStage(StageManifest), ErrNotPublished)
		} else {
			require.Equal(t, before.Publication, ownerTestWitness(t, b.head, stages[i-1].Witness))
		}
		ownerTestPublish(t, b, []StageTables{tables})
	}
	before := ownerTestRow(t, o)
	contracts := stages[3]
	contracts.Witness.Contracts = nil
	require.ErrorIs(t, b.publishStage(ctx, contracts), ErrStageOrder, "contracts without coverage")
	legacy, err := PrepareDeferredDerived(t.TempDir(), stages[2])
	require.NoError(t, err)
	require.ErrorIs(t, b.publishStage(ctx, legacy), ErrStageOrder, "derived without coverage")
	require.Equal(t, before.Publication, ownerTestWitness(t, b.head, stages[2].Witness))
	require.Equal(t, before, ownerTestRow(t, o))

	// The low-level Head supports legacy stages. Advance it directly so only
	// missing coverage, not the Head's base or stage checks, can reject Search.
	base, err := b.head.PublishStage(ctx, legacy)
	require.NoError(t, err)
	search := stages[5]
	search.Base = base
	search.Witness.Contracts = nil
	search.Witness.Unsupported = legacy.Witness.Unsupported
	require.ErrorIs(t, b.publishStage(ctx, search), ErrStageOrder, "search without coverage")
	ownerTestWitness(t, b.head, legacy.Witness)
	require.Equal(t, before, ownerTestRow(t, o))
	_, err = b.head.PublishStage(ctx, search)
	require.NoError(t, err, "the legacy Search offer is otherwise admissible")
}

func TestRouteOwnerReplacementAndNoChange(t *testing.T) {
	ctx := t.Context()
	o := ownerTestInit(t)
	oldStages := ownerTestStages(t, routeTestSource, StageSearch, nil)
	old, err := o.begin(ctx, routeTestOldLease, nil, oldStages[0].Witness)
	require.NoError(t, err)
	require.NotNil(t, old)
	t.Cleanup(func() { require.NoError(t, old.head.Close()) })
	require.Equal(t, old.locator, ownerTestRow(t, o).Candidate)
	reserved := ownerTestRow(t, o)
	require.Zero(t, reserved.Serving)
	wrong := oldStages[0]
	wrong.Witness.Input[0] ^= 1
	require.ErrorIs(t, old.publishStage(ctx, wrong), ErrStaleBase)
	wrong = oldStages[0]
	wrong.Witness.FrozenInputs[0] ^= 1
	require.Equal(t, old.requested.Version(), wrong.Witness.Version())
	require.ErrorIs(t, old.publishStage(ctx, wrong), ErrStaleBase, "frozen inputs must match even at the same version")
	require.ErrorIs(t, stageRead(t, old.head).RequireStage(StageManifest), ErrNotPublished)
	require.Equal(t, reserved, ownerTestRow(t, o))
	ownerTestPublish(t, old, oldStages[:1])
	require.Equal(t, old.locator, ownerTestRow(t, o).Serving)
	pinnedManifest := stageRead(t, old.head)
	ownerTestPublish(t, old, oldStages[1:])
	require.ErrorAs(t, pinnedManifest.RequireStage(StageFileGraph), new(*ErrStagePending))
	oldRow := ownerTestRow(t, o)
	oldSnap := stageRead(t, old.head)
	oldRows := relationsSavedRows(t, oldSnap)

	// S20: select the same already-complete version from independently
	// prepared inputs. No worker callback, build directory, ingest or CAS.
	same := Witness{Input: blake3.Sum256([]byte(routeTestSource)), Recipe: HeadDigest{1}, Stage: StageManifest, Profile: FullProfile}
	ingests := old.head.db.Metrics().Ingest.Count
	catalogWrites := o.catalog.db.Metrics().WAL.BytesWritten
	unchanged, err := o.begin(ctx, routeTestNewLease, old.head, same)
	require.NoError(t, err)
	require.Nil(t, unchanged)
	require.NoFileExists(t, o.locator(routeTestNewLease).Directory)
	require.NoDirExists(t, o.locator(routeTestNewLease).Directory)
	require.Equal(t, ingests, old.head.db.Metrics().Ingest.Count)
	require.Equal(t, catalogWrites, o.catalog.db.Metrics().WAL.BytesWritten)
	require.Equal(t, oldRow, ownerTestRow(t, o))
	require.Equal(t, oldRow.Publication, ownerTestWitness(t, old.head, oldStages[len(oldStages)-1].Witness))
	require.Equal(t, oldRows, relationsSavedRows(t, stageRead(t, old.head)))

	newStages := ownerTestStages(t, routeTestSource+"// edit\n", StageSearch, nil)
	next, err := o.begin(ctx, routeTestNewLease, old.head, newStages[0].Witness)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, next.head.Close()) })
	beforeSwitch := oldRow
	beforeSwitch.Candidate = next.locator
	for _, tables := range newStages[:len(newStages)-1] {
		ownerTestPublish(t, next, []StageTables{tables})
		require.Equal(t, beforeSwitch, ownerTestRow(t, o), tables.Witness.Stage)
	}
	ownerTestPublish(t, next, newStages[len(newStages)-1:])
	newRow := ownerTestRow(t, o)
	require.Equal(t, next.locator, newRow.Serving)
	require.Zero(t, newRow.Candidate)
	require.NotEqual(t, oldRow.Publication.Version, newRow.Publication.Version)
	require.Equal(t, oldRows, relationsSavedRows(t, oldSnap))
	require.ErrorIs(t, next.publishStage(ctx, newStages[len(newStages)-1]), ErrStageOrder)
	require.ErrorIs(t, old.publishStage(ctx, oldStages[len(oldStages)-1]), ErrStaleBase)
	require.Equal(t, newRow, ownerTestRow(t, o))
}

func TestRouteOwnerPartialCoverageAndFailure(t *testing.T) {
	ctx := t.Context()
	o := ownerTestInit(t)
	stages := ownerTestStages(t, routeTestSource, StageRelations, nil)
	old, err := o.begin(ctx, routeTestOldLease, nil, stages[0].Witness)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, old.head.Close()) })
	ownerTestPublish(t, old, stages)
	oldRow := ownerTestRow(t, o)
	legacy, err := PrepareDeferredDerived(t.TempDir(), stages[2])
	require.NoError(t, err)
	require.ErrorIs(t, old.publishStage(ctx, legacy), ErrStageOrder)
	require.Equal(t, oldRow, ownerTestRow(t, o))
	pinned := stageRead(t, old.head)

	// A seal refusal after first route does not delete or advance that head.
	failed := errors.New("worker seal refused")
	require.ErrorIs(t, old.failBuild(ctx, failed), failed)
	require.Equal(t, oldRow, ownerTestRow(t, o))
	require.DirExists(t, old.locator.Directory)
	require.Equal(t, oldRow.Publication, ownerTestWitness(t, old.head, stages[len(stages)-1].Witness))

	regressedStages := ownerTestStages(t, routeTestSource+"// edit\n", StageRelations, []string{"go"})
	regressed, err := o.begin(ctx, routeTestNewLease, old.head, regressedStages[0].Witness)
	require.NoError(t, err)
	ownerTestPublish(t, regressed, regressedStages)
	require.Equal(t, old.locator, ownerTestRow(t, o).Serving, "equal stage does not excuse a coverage regression")
	marker := filepath.Join(o.root, "unowned")
	require.NoError(t, os.WriteFile(marker, []byte("keep"), 0o600))
	require.ErrorIs(t, regressed.failBuild(ctx, failed), failed)
	require.NoDirExists(t, regressed.locator.Directory)
	require.FileExists(t, marker)
	require.Equal(t, oldRow, ownerTestRow(t, o))

	// Failure before any offer is also safe and only cleans owned paths.
	fresh := ownerTestManifest(t, routeTestSource+"// latest\n")
	empty, err := o.begin(ctx, ownerThirdLease, old.head, fresh.Witness)
	require.NoError(t, err)
	require.ErrorIs(t, empty.failBuild(ctx, failed), failed)
	require.NoDirExists(t, empty.locator.Directory)
	require.Equal(t, oldRow, ownerTestRow(t, o))

	betterStages := ownerTestStages(t, routeTestSource+"// latest\n", StageRelations, nil)
	better, err := o.begin(ctx, ownerFourthLease, old.head, betterStages[0].Witness)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, better.head.Close()) })
	ownerTestPublish(t, better, betterStages[:2])
	require.Equal(t, old.locator, ownerTestRow(t, o).Serving)
	ownerTestPublish(t, better, betterStages[2:])
	require.Equal(t, better.locator, ownerTestRow(t, o).Serving)
	w, err := pinned.Publication()
	require.NoError(t, err)
	require.Equal(t, stages[2].Witness, w)
	require.NoError(t, pinned.ReadError())
}

func TestRouteOwnerCancelledFailureClearsCandidate(t *testing.T) {
	ctx := t.Context()
	o := ownerTestInit(t)
	manifest := ownerTestManifest(t, routeTestSource)
	b, err := o.begin(ctx, routeTestOldLease, nil, manifest.Witness)
	require.NoError(t, err)
	t.Cleanup(func() {
		if b.head != nil {
			require.NoError(t, b.head.Close())
		}
	})
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, b.failBuild(cancelled, cancelled.Err()), context.Canceled)
	require.NoDirExists(t, b.locator.Directory)
	require.Equal(t, routeCheckout{Incarnation: o.incarnation}, ownerTestRow(t, o))

	// A later build can reserve the same directory, including its LOCK path.
	next, err := o.begin(ctx, routeTestOldLease, nil, manifest.Witness)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, next.head.Close()) })
	require.Equal(t, next.locator, ownerTestRow(t, o).Candidate)
}

func TestRouteOwnerStaleFailureClosesOnlyDetachedHead(t *testing.T) {
	ctx := t.Context()
	o := ownerTestInit(t)
	manifest := ownerTestManifest(t, routeTestSource)
	b, err := o.begin(ctx, routeTestOldLease, nil, manifest.Witness)
	require.NoError(t, err)
	t.Cleanup(func() {
		if b.head != nil {
			require.NoError(t, b.head.Close())
		}
	})
	next := o.locator(routeTestNewLease)
	require.NoError(t, o.catalog.registerCandidate(ctx, o.checkout, o.incarnation, routeLocator{}, next))
	nextHead, err := OpenHead(next.Directory, o.incarnation, o.profile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, nextHead.Close()) })
	before := ownerTestRow(t, o)
	failure := errors.New("worker failed after losing its reservation")
	for range 2 {
		err = b.failBuild(ctx, failure)
		require.ErrorIs(t, err, failure)
		require.ErrorIs(t, err, ErrStaleBase)
		require.Equal(t, before, ownerTestRow(t, o))
		require.DirExists(t, b.locator.Directory)
	}
	// Opening the detached DB needs its original LOCK to be released. The
	// new candidate still owns its own handle and can ingest normally.
	detached, err := OpenHead(b.locator.Directory, o.incarnation, o.profile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, detached.Close()) })
	_, err = nextHead.PublishStage(ctx, manifest)
	require.NoError(t, err)
}

// The full logical row check here is the existing independent test oracle.
// The daemon's cheaper startup counter validation is not implemented in B2.
func ownerTestValidator(t *testing.T) func(context.Context, *Snapshot, Witness) error {
	t.Helper()
	return func(ctx context.Context, snap *Snapshot, w Witness) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !slices.Equal(w.Families, relationsSavedWitness(t, snap)) {
			return invalid("startup family witness differs from rows")
		}
		return nil
	}
}

func TestRouteOwnerRecoversAcrossCatalogSwitch(t *testing.T) {
	for _, phase := range []string{"before-switch", "after-switch", headCrashAfterSelectedIngest} {
		t.Run(phase, func(t *testing.T) {
			lease := t.TempDir()
			row, stage := prepareRouteCrashFixture(t, lease)
			killHeadCrashChild(t, lease, phase)
			o := ownerTestCatalog(t, lease)
			r, err := o.recover(t.Context(), ownerTestValidator(t))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, r.Head.Close()) })
			require.True(t, r.Requeue, "partial cut or abandoned candidate must requeue")
			require.False(t, r.LastGood)
			got := ownerTestRow(t, o)
			require.Zero(t, got.Candidate)
			require.Equal(t, r.Publication, got.Publication)
			switch phase {
			case "before-switch":
				require.Equal(t, row.Serving, got.Serving)
				require.Equal(t, row.Publication, r.Publication)
				require.Equal(t, StageManifest, r.Witness.Stage)
				require.NoDirExists(t, row.Candidate.Directory)
			case "after-switch":
				require.Equal(t, row.Candidate, got.Serving)
				require.Equal(t, stage.Witness, r.Witness)
				require.DirExists(t, row.Serving.Directory)
			case headCrashAfterSelectedIngest:
				require.Equal(t, row.Serving, got.Serving)
				require.Equal(t, stage.Witness, r.Witness)
				require.Nil(t, r.Publication.EventID)
				require.NotEqual(t, row.Publication.WitnessDigest, r.Publication.WitnessDigest)
				require.NoDirExists(t, row.Candidate.Directory)
			}
		})
	}
}

func TestRouteOwnerDiscardsUnexposedManifest(t *testing.T) {
	ctx := t.Context()
	lease := t.TempDir()
	manifest := ownerTestManifest(t, routeTestSource)
	func() {
		catalog, err := openRouteCatalog(filepath.Join(lease, "catalog"))
		require.NoError(t, err)
		defer func() { require.NoError(t, catalog.Close()) }()
		o := &routeOwner{catalog: catalog, checkout: lease, root: lease, incarnation: testHeadIncarnation, profile: FullProfile}
		require.NoError(t, catalog.initCheckout(ctx, lease, o.incarnation))
		b, err := o.begin(ctx, routeTestOldLease, nil, manifest.Witness)
		require.NoError(t, err)
		require.NoError(t, b.head.Close())
	}()
	writeOwnerCrashRequest(t, lease, manifest)
	killHeadCrashChild(t, lease, headCrashBeforeFirstRoute)
	o := ownerTestCatalog(t, lease)
	loc := o.locator(routeTestOldLease)
	require.DirExists(t, loc.Directory)
	require.Zero(t, ownerTestRow(t, o).Serving)
	recovered, err := o.recover(ctx, ownerTestValidator(t))
	require.NoError(t, err)
	require.Nil(t, recovered.Head)
	require.True(t, recovered.Requeue)
	require.NoDirExists(t, loc.Directory)
	require.Zero(t, ownerTestRow(t, o).Candidate)
}

func TestRouteOwnerStartupRestoresOnlyCheckedCheckpoint(t *testing.T) {
	for _, mode := range []string{"invalid rows", "invalid checkpoint", "missing witness", "missing DB", "missing directory", "not a directory", "corrupt manifest marker"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			lease := t.TempDir()
			oldStages := ownerTestStages(t, routeTestSource, StageSearch, nil)
			newStages := ownerTestStages(t, routeTestSource+"// edit\n", StageSearch, nil)
			var oldRow, newRow routeCheckout
			func() {
				catalog, err := openRouteCatalog(filepath.Join(lease, "catalog"))
				require.NoError(t, err)
				defer func() { require.NoError(t, catalog.Close()) }()
				o := &routeOwner{catalog: catalog, checkout: lease, root: lease, incarnation: testHeadIncarnation, profile: FullProfile}
				require.NoError(t, catalog.initCheckout(ctx, lease, o.incarnation))
				old, err := o.begin(ctx, routeTestOldLease, nil, oldStages[0].Witness)
				require.NoError(t, err)
				defer func() { require.NoError(t, old.head.Close()) }()
				ownerTestPublish(t, old, oldStages)
				require.NoError(t, o.retainCheckpoint(ctx, old.head, ownerThirdLease))
				oldRow = ownerTestRow(t, o)
				next, err := o.begin(ctx, routeTestNewLease, old.head, newStages[0].Witness)
				require.NoError(t, err)
				defer func() { require.NoError(t, next.head.Close()) }()
				ownerTestPublish(t, next, newStages)
				newRow = ownerTestRow(t, o)
				switch mode {
				case "invalid rows", "invalid checkpoint":
					// Leave the witness intact so the integrity check must refuse.
					require.NoError(t, next.head.db.Delete(pendingNodeKey("a.go::F"), pebble.Sync))
				case "missing witness":
					require.NoError(t, next.head.db.Delete(publicationWitnessKey, pebble.Sync))
				}
				if mode == "invalid checkpoint" {
					checkpoint, err := OpenHead(oldRow.Retained.Locator.Directory, o.incarnation, FullProfile)
					require.NoError(t, err)
					require.NoError(t, checkpoint.db.Delete(pendingNodeKey("a.go::F"), pebble.Sync))
					require.NoError(t, checkpoint.Close())
				}
			}()
			o := ownerTestCatalog(t, lease)
			serving := newRow.Serving.Directory
			switch mode {
			case "missing DB", "missing directory", "not a directory":
				require.NoError(t, os.RemoveAll(serving))
				switch mode {
				case "missing DB":
					require.NoError(t, os.Mkdir(serving, 0o700))
				case "not a directory":
					require.NoError(t, os.WriteFile(serving, []byte("not a DB"), 0o600))
				}
			case "corrupt manifest marker":
				markers, err := filepath.Glob(filepath.Join(serving, "marker.manifest.*"))
				require.NoError(t, err)
				require.Len(t, markers, 1)
				// Preserve the marker's sequence number, but make the selected
				// manifest filename unparsable to Pebble.
				require.NoError(t, os.Rename(markers[0], markers[0]+".bad"))
			}
			r, err := o.recover(ctx, ownerTestValidator(t))
			if mode == "invalid checkpoint" {
				require.ErrorIs(t, err, errInvalidRecord)
				require.Nil(t, r.Head)
				require.Equal(t, newRow, ownerTestRow(t, o))
				return
			}
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, r.Head.Close()) })
			require.True(t, r.LastGood)
			require.True(t, r.Requeue)
			switch mode {
			case "missing witness":
				require.ErrorIs(t, r.Failure, ErrNotPublished)
			case "missing DB":
				require.ErrorIs(t, r.Failure, pebble.ErrDBDoesNotExist)
			case "missing directory":
				require.ErrorIs(t, r.Failure, os.ErrNotExist)
			case "corrupt manifest marker":
				require.True(t, pebble.IsCorruptionError(r.Failure), "%v", r.Failure)
			default:
				require.ErrorIs(t, r.Failure, errInvalidRecord)
			}
			require.Equal(t, oldRow.Publication, r.Publication)
			require.Equal(t, oldStages[len(oldStages)-1].Witness, r.Witness)
			row := ownerTestRow(t, o)
			require.Equal(t, oldRow.Retained.Locator, row.Serving)
			require.True(t, row.LastGood)
			require.Zero(t, row.Retained)
			switch mode {
			case "missing directory":
				require.NoDirExists(t, serving)
			case "not a directory":
				require.FileExists(t, serving)
			default:
				require.DirExists(t, serving, "fallback does not delete the failed head")
			}
		})
	}
}

func TestRouteOwnerRecoveryKeepsSelectionOnTransientFailure(t *testing.T) {
	ctx := t.Context()
	o := ownerTestInit(t)
	stages := ownerTestStages(t, routeTestSource, StageSearch, nil)
	validate := ownerTestValidator(t)
	b, err := o.begin(ctx, routeTestOldLease, nil, stages[0].Witness)
	require.NoError(t, err)
	func() {
		defer func() { require.NoError(t, b.head.Close()) }()
		ownerTestPublish(t, b, stages)
		require.NoError(t, o.retainCheckpoint(ctx, b.head, routeTestNewLease))
		before := ownerTestRow(t, o)

		r, err := o.recover(ctx, validate)
		if r.Head != nil {
			t.Cleanup(func() { require.NoError(t, r.Head.Close()) })
		}
		require.Error(t, err, "the serving LOCK is held")
		require.Nil(t, r.Head)
		require.Equal(t, before, ownerTestRow(t, o))
	}()
	before := ownerTestRow(t, o)
	for _, failure := range []error{syscall.EIO, syscall.EMFILE, syscall.EACCES} {
		t.Run(failure.Error(), func(t *testing.T) {
			checks := 0
			r, err := o.recover(ctx, func(ctx context.Context, snap *Snapshot, w Witness) error {
				checks++
				if checks == 1 {
					if failure == syscall.EIO {
						snap.retain(failure)
						return invalid("partial read made a family look incomplete")
					}
					return failure
				}
				return validate(ctx, snap, w)
			})
			if r.Head != nil {
				t.Cleanup(func() { require.NoError(t, r.Head.Close()) })
			}
			require.ErrorIs(t, err, failure)
			require.Nil(t, r.Head)
			require.Equal(t, before, ownerTestRow(t, o))
		})
	}
	r, err := o.recover(ctx, validate)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Head.Close()) })
	require.Equal(t, before.Publication, r.Publication)
	require.False(t, r.LastGood)
	require.Equal(t, before, ownerTestRow(t, o))
}

func writeOwnerCrashRequest(t *testing.T, lease string, stage StageTables) {
	t.Helper()
	raw, err := json.Marshal(stage)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(lease, "stage.json"), raw, 0o600))
}

func TestRouteOwnerEditIngestRecovery(t *testing.T) {
	for _, phase := range []string{"ordinary", headCrashEditDuring, headCrashEditAfter} {
		t.Run(phase, func(t *testing.T) {
			ctx := t.Context()
			lease := t.TempDir()
			source := routeTestSource + "// edit\n"
			oldStages := ownerTestStages(t, routeTestSource, StageSearch, nil)
			newStages := ownerTestStages(t, source, StageSearch, nil)
			oldWitness, newWitness := oldStages[len(oldStages)-1].Witness, newStages[len(newStages)-1].Witness
			// Only two families change in this body-only fixture. The exact
			// result witness comes from independently preparing all six seals.
			unchangedFamilies := func(w Witness) []FamilyWitness {
				return slices.DeleteFunc(slices.Clone(w.Families), func(f FamilyWitness) bool {
					return f.Family == familyManifest || f.Family == familyFileMeta
				})
			}
			require.Equal(t, unchangedFamilies(oldWitness), unchangedFamilies(newWitness))
			var saved routeCheckout
			var delta StageTables
			func() {
				catalog, err := openRouteCatalog(filepath.Join(lease, "catalog"))
				require.NoError(t, err)
				defer func() { require.NoError(t, catalog.Close()) }()
				o := &routeOwner{catalog: catalog, checkout: lease, root: lease, incarnation: testHeadIncarnation, profile: FullProfile}
				require.NoError(t, catalog.initCheckout(ctx, lease, o.incarnation))
				b, err := o.begin(ctx, routeTestOldLease, nil, oldStages[0].Witness)
				require.NoError(t, err)
				defer func() { require.NoError(t, b.head.Close()) }()
				ownerTestPublish(t, b, oldStages)
				saved = ownerTestRow(t, o)
				mark := uint64(41)
				saved.Publication.EventID = &mark
				require.NoError(t, catalog.save(ctx, lease, saved))
				delta = newStages[0] // real sealed manifest, plus the changed file metadata
				value, err := encodeFileMeta(graph.FileMetaRow{FilePath: "a.go", Size: len(source), ContentHash: fmt.Sprintf("%x", blake3.Sum256([]byte(source))), NodeCount: 1})
				require.NoError(t, err)
				makeStageTable(t, b.head, delta.Dir, "edit-meta.sst", fileMetaKey("", "a.go"), value)
				delta.Files = append(delta.Files, "edit-meta.sst")
				delta.Base, delta.Witness = saved.Publication.WitnessDigest, newWitness
				bad := delta
				bad.Base[0] ^= 1
				require.ErrorIs(t, o.publishDelta(ctx, b.head, bad), ErrStaleBase)
				require.Equal(t, saved, ownerTestRow(t, o))
				if phase == "ordinary" {
					candidate, err := o.begin(ctx, routeTestNewLease, b.head, newWitness)
					require.NoError(t, err)
					t.Cleanup(func() {
						if candidate.head != nil {
							require.NoError(t, candidate.head.Close())
						}
					})
					reserved := ownerTestRow(t, o)
					require.ErrorIs(t, o.publishDelta(ctx, b.head, delta), ErrStaleBase, "registered candidate fences a valid edit delta")
					require.Equal(t, reserved, ownerTestRow(t, o))
					require.Equal(t, saved.Publication.WitnessDigest, ownerTestWitness(t, b.head, oldWitness).WitnessDigest)
					failed := errors.New("candidate failed before publication")
					require.ErrorIs(t, candidate.failBuild(ctx, failed), failed)
					require.Equal(t, saved, ownerTestRow(t, o))

					pinned := b.head.OpenSnapshot()
					defer func() { require.NoError(t, pinned.Close()) }()
					require.NoError(t, o.publishDelta(ctx, b.head, delta))
					require.Equal(t, ownerTestWitness(t, b.head, newWitness), ownerTestRow(t, o).Publication)
					w, err := pinned.Publication()
					require.NoError(t, err)
					require.Equal(t, oldWitness, w)
					require.ErrorIs(t, o.publishDelta(ctx, b.head, delta), ErrStaleBase)
				}
			}()
			if phase != "ordinary" {
				writeOwnerCrashRequest(t, lease, delta)
				killHeadCrashChild(t, lease, phase)
			}
			o := ownerTestCatalog(t, lease)
			r, err := o.recover(ctx, ownerTestValidator(t))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, r.Head.Close()) })
			require.Equal(t, saved.Serving, ownerTestRow(t, o).Serving)
			require.Equal(t, r.Publication, ownerTestRow(t, o).Publication)
			want, wantSource := newWitness, source
			if phase == headCrashEditDuring {
				want, wantSource = oldWitness, routeTestSource
				require.Equal(t, saved.Publication, r.Publication)
			} else {
				require.Nil(t, r.Publication.EventID)
				require.NotEqual(t, saved.Publication.Version, r.Publication.Version)
			}
			require.True(t, r.Requeue, "restart must check the input diff even if the old watcher mark still matches")
			require.Equal(t, want, r.Witness)
			snap := stageRead(t, r.Head)
			file, err := snap.ManifestFile("a.go")
			require.NoError(t, err)
			require.Equal(t, HeadDigest(blake3.Sum256([]byte(wantSource))), file.Content)
			meta, err := snap.FileMetadata("", "a.go")
			require.NoError(t, err)
			require.Equal(t, fmt.Sprintf("%x", file.Content), meta.ContentHash)
			require.Equal(t, len(wantSource), meta.Size)
			require.Equal(t, want.Families, relationsSavedWitness(t, snap))
		})
	}
}

func TestRouteOwnerRecoveryRefusesUnownedCandidate(t *testing.T) {
	o := ownerTestInit(t)
	other := routeLocator{Lease: routeTestOldLease, Directory: t.TempDir()}
	marker := filepath.Join(other.Directory, "keep")
	require.NoError(t, os.WriteFile(marker, []byte("other owner"), 0o600))
	require.NoError(t, o.catalog.registerCandidate(t.Context(), o.checkout, o.incarnation, routeLocator{}, other))
	r, err := o.recover(t.Context(), ownerTestValidator(t))
	require.ErrorIs(t, err, errInvalidRecord)
	require.Nil(t, r.Head)
	require.FileExists(t, marker)
	row, err := o.catalog.lookup(o.checkout)
	require.NoError(t, err)
	require.Equal(t, other, row.Candidate)
}

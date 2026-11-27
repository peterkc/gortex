package store_pebble

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/graph"
)

const (
	routeTestOldLease = "0198abcd-0123-7abc-8def-0123456789ad"
	routeTestNewLease = "0198abcd-0123-7abc-8def-0123456789ae"
	routeTestSource   = "package p\n\nfunc F() {}\n"
)

func TestRouteCatalogSwitchHead(t *testing.T) {
	ctx := t.Context()
	checkout := t.TempDir()
	inc := testHeadIncarnation
	path := filepath.Join(t.TempDir(), "catalog")
	catalog, err := openRouteCatalog(path)
	require.NoError(t, err)
	t.Cleanup(func() {
		if catalog != nil {
			require.NoError(t, catalog.Close())
		}
	})
	reopen := func() {
		t.Helper()
		err := catalog.Close()
		catalog = nil
		require.NoError(t, err)
		catalog, err = openRouteCatalog(path)
		require.NoError(t, err)
	}
	assertRoute := func(want routeCheckout) {
		t.Helper()
		got, err := catalog.lookup(checkout)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}

	require.NoError(t, catalog.initCheckout(ctx, checkout, inc))
	want := routeCheckout{Incarnation: inc}
	assertRoute(want)
	oldLoc, old, oldManifest := newRouteTestHead(t, checkout, routeTestOldLease, 1)
	oldManifest.Witness.PlatformCatalogRevision = ^uint32(0) // unknown to this binary
	var unrouted routeLocator
	require.NoError(t, catalog.registerCandidate(ctx, checkout, inc, unrouted, oldLoc))
	want.Candidate = oldLoc
	assertRoute(want)

	// A receipt for the exact sealed witness is not proof that ingest occurred.
	raw, err := encodeWitness(oldManifest.Witness)
	require.NoError(t, err)
	event := uint64(101)
	oldReceipt := LocalPublicationRecord{
		Version: oldManifest.Witness.Version(), Incarnation: inc,
		WitnessDigest: blake3.Sum256(raw), EventID: &event,
	}
	err = catalog.switchHead(ctx, checkout, inc, unrouted, oldLoc, nil, old, oldReceipt)
	require.ErrorIs(t, err, ErrNotPublished)
	assertRoute(want)

	require.Equal(t, oldReceipt.WitnessDigest, publishRouteTestStage(t, old, oldManifest).WitnessDigest)
	wrongWitness := oldReceipt
	wrongWitness.WitnessDigest[0] ^= 1
	err = catalog.switchHead(ctx, checkout, inc, unrouted, oldLoc, nil, old, wrongWitness)
	require.ErrorIs(t, err, ErrStaleBase)
	assertRoute(want)
	require.NoError(t, catalog.switchHead(ctx, checkout, inc, unrouted, oldLoc, nil, old, oldReceipt))
	want = routeCheckout{Incarnation: inc, Serving: oldLoc, Publication: oldReceipt}
	reopen()
	assertRoute(want)

	pinned := stageRead(t, old)
	_, oldGraph := prepareRouteTestFileGraph(t, oldManifest)
	oldLive := publishRouteTestStage(t, old, oldGraph)
	require.NotEqual(t, oldReceipt.WitnessDigest, oldLive.WitnessDigest)
	live := stageRead(t, old)
	require.NoError(t, live.RequireStage(StageFileGraph))
	require.NotNil(t, live.GetNode("a.go::F"))
	assertRoute(want) // The catalog's record still refers to the old manifest.

	nextLoc, next, nextManifest := newRouteTestHead(t, checkout, routeTestNewLease, 1)
	nextManifest.Witness.PlatformCatalogRevision = graph.PlatformCatalogRevision
	require.NoError(t, catalog.registerCandidate(ctx, checkout, inc, oldLoc, nextLoc))
	want.Candidate = nextLoc
	manifestReceipt := publishRouteTestStage(t, next, nextManifest)
	err = catalog.switchHead(ctx, checkout, inc, oldLoc, nextLoc, old, next, manifestReceipt)
	require.ErrorIs(t, err, ErrStageOrder, "compare against the live prior, not its catalog receipt")
	assertRoute(want)

	_, nextGraph := prepareRouteTestFileGraph(t, nextManifest)
	nextReceipt := publishRouteTestStage(t, next, nextGraph)
	require.NotEqual(t, oldReceipt.Version, nextReceipt.Version)
	wrongDir := filepath.Join(t.TempDir(), "same-publication")
	require.NoError(t, next.Checkpoint(wrongDir))
	wrongHead, err := OpenHead(wrongDir, inc, FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, wrongHead.Close()) })
	_, sameReceipt, err := wrongHead.ReconcilePublication(&nextReceipt)
	require.NoError(t, err)
	require.Equal(t, nextReceipt, sameReceipt)

	staleOld := oldLoc
	staleOld.Lease = "0198abcd-0123-7abc-8def-0123456789af"
	for _, tc := range []struct {
		name, incarnation string
		base              routeLocator
		head              *Head
		receipt           LocalPublicationRecord
	}{
		{"stale old locator", inc, staleOld, next, nextReceipt},
		{"wrong incarnation", testReplacementIncarnation, oldLoc, next, nextReceipt},
		{"wrong open head directory", inc, oldLoc, wrongHead, nextReceipt},
		{"superseded receipt", inc, oldLoc, next, manifestReceipt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := catalog.switchHead(t.Context(), checkout, tc.incarnation, tc.base, nextLoc, old, tc.head, tc.receipt)
			require.ErrorIs(t, err, ErrStaleBase)
			got, err := catalog.lookup(checkout)
			require.NoError(t, err)
			require.Equal(t, want, got)
		})
	}

	require.NoError(t, catalog.switchHead(ctx, checkout, inc, oldLoc, nextLoc, old, next, nextReceipt))
	want = routeCheckout{Incarnation: inc, Serving: nextLoc, Publication: nextReceipt}
	assertRoute(want)
	pw, err := pinned.Publication()
	require.NoError(t, err)
	require.Equal(t, StageManifest, pw.Stage)
	require.Equal(t, oldReceipt.Version, pw.Version())
	require.Equal(t, ^uint32(0), pw.PlatformCatalogRevision)
	require.Nil(t, pinned.GetNode("a.go::F"))

	require.ErrorIs(t, catalog.registerCandidate(ctx, checkout, inc, oldLoc, nextLoc), ErrStaleBase)
	require.ErrorIs(t, catalog.switchHead(ctx, checkout, inc, oldLoc, nextLoc, old, next, nextReceipt), ErrStaleBase)
	require.ErrorIs(t, catalog.switchHead(ctx, checkout, inc, nextLoc, oldLoc, next, old, oldLive), ErrStaleBase,
		"the selected route has no registered replacement")
	reopen()
	assertRoute(want)
}

func TestRouteCatalogRejectsSupersededCandidate(t *testing.T) {
	ctx := t.Context()
	checkout := t.TempDir()
	inc := testHeadIncarnation
	catalog, err := openRouteCatalog(filepath.Join(t.TempDir(), "catalog"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	require.NoError(t, catalog.initCheckout(ctx, checkout, inc))

	oldLoc, old, oldManifest := newRouteTestHead(t, checkout, routeTestOldLease, 1)
	var unrouted routeLocator
	require.NoError(t, catalog.registerCandidate(ctx, checkout, inc, unrouted, oldLoc))
	oldRecord := publishRouteTestStage(t, old, oldManifest)
	require.NoError(t, catalog.switchHead(ctx, checkout, inc, unrouted, oldLoc, nil, old, oldRecord))

	firstLoc, first, firstManifest := newRouteTestHead(t, checkout, routeTestNewLease, 2)
	require.NoError(t, catalog.registerCandidate(ctx, checkout, inc, oldLoc, firstLoc))
	firstRecord := publishRouteTestStage(t, first, firstManifest)

	const replacementLease = "0198abcd-0123-7abc-8def-0123456789b0"
	nextLoc, next, nextManifest := newRouteTestHead(t, checkout, replacementLease, 2)
	require.NoError(t, catalog.registerCandidate(ctx, checkout, inc, oldLoc, nextLoc))
	nextRecord := publishRouteTestStage(t, next, nextManifest)
	require.Equal(t, firstRecord, nextRecord, "the lease and directory must fence even byte-identical publications")

	err = catalog.switchHead(ctx, checkout, inc, oldLoc, firstLoc, old, first, firstRecord)
	require.ErrorIs(t, err, ErrStaleBase)
	got, err := catalog.lookup(checkout)
	require.NoError(t, err)
	require.Equal(t, routeCheckout{Incarnation: inc, Serving: oldLoc, Candidate: nextLoc, Publication: oldRecord}, got)

	require.NoError(t, catalog.switchHead(ctx, checkout, inc, oldLoc, nextLoc, old, next, nextRecord))
	got, err = catalog.lookup(checkout)
	require.NoError(t, err)
	require.Equal(t, routeCheckout{Incarnation: inc, Serving: nextLoc, Publication: nextRecord}, got)
}

func TestRouteCatalogRefusesCoverageRegression(t *testing.T) {
	ctx := t.Context()
	checkout := t.TempDir()
	inc := testHeadIncarnation
	catalog, err := openRouteCatalog(filepath.Join(t.TempDir(), "catalog"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	require.NoError(t, catalog.initCheckout(ctx, checkout, inc))

	publishRelations := func(h *Head, manifest StageTables, coverage RelationCoverage) LocalPublicationRecord {
		t.Helper()
		worker, fileGraph := prepareRouteTestFileGraph(t, manifest)
		base, err := worker.RetainFileGraph(ctx, fileGraph)
		require.NoError(t, err)
		publishRouteTestStage(t, h, fileGraph)
		relations, err := worker.PrepareRelations(ctx, t.TempDir(), base, nil)
		require.NoError(t, err)
		relations.Witness.Relations = &coverage
		return publishRouteTestStage(t, h, relations)
	}
	oldLoc, old, oldManifest := newRouteTestHead(t, checkout, routeTestOldLease, 1)
	var unrouted routeLocator
	require.NoError(t, catalog.registerCandidate(ctx, checkout, inc, unrouted, oldLoc))
	oldReceipt := publishRouteTestStage(t, old, oldManifest)
	require.NoError(t, catalog.switchHead(ctx, checkout, inc, unrouted, oldLoc, nil, old, oldReceipt))
	publishRelations(old, oldManifest, RelationCoverage{})

	nextLoc, next, nextManifest := newRouteTestHead(t, checkout, routeTestNewLease, 2)
	require.NoError(t, catalog.registerCandidate(ctx, checkout, inc, oldLoc, nextLoc))
	publishRouteTestStage(t, next, nextManifest)
	regressed := publishRelations(next, nextManifest, RelationCoverage{DeferredLanguages: []string{"go"}})
	err = catalog.switchHead(ctx, checkout, inc, oldLoc, nextLoc, old, next, regressed)
	require.ErrorIs(t, err, ErrCoverageRegression)
	got, err := catalog.lookup(checkout)
	require.NoError(t, err)
	require.Equal(t, routeCheckout{Incarnation: inc, Serving: oldLoc, Candidate: nextLoc, Publication: oldReceipt}, got)
}

func newRouteTestHead(t *testing.T, checkout, lease string, recipe byte) (routeLocator, *Head, StageTables) {
	t.Helper()
	loc := routeLocator{Lease: lease, Directory: filepath.Join(t.TempDir(), lease)}
	h, err := OpenHead(loc.Directory, testHeadIncarnation, FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	source := []byte(routeTestSource)
	require.NoError(t, os.WriteFile(filepath.Join(checkout, "a.go"), source, 0o600))
	content := blake3.Sum256(source)
	manifest, err := PrepareManifest(t.Context(), t.TempDir(), []ManifestFile{{
		Path: "a.go", Language: "go", Size: int64(len(source)), Kind: 'f', Content: content, Extractor: HeadDigest{3},
	}}, Witness{Input: content, Recipe: HeadDigest{recipe}, Profile: FullProfile, Stage: StageManifest}, testHeadIncarnation)
	require.NoError(t, err)
	return loc, h, manifest
}

func prepareRouteTestFileGraph(t *testing.T, manifest StageTables) (*BuildStore, StageTables) {
	t.Helper()
	worker, err := OpenBuild(filepath.Join(t.TempDir(), "worker"), FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, worker.Close()) })
	decl := &graph.Node{ID: "a.go::F", Kind: graph.KindFunction, Name: "F", FilePath: "a.go", Language: "go"}
	require.NoError(t, worker.AddParsedFile("", ParseFacts{
		Nodes: []*graph.Node{decl},
		File: graph.FileMetaRow{FilePath: "a.go", Size: len(routeTestSource),
			ContentHash: fmt.Sprintf("%x", blake3.Sum256([]byte(routeTestSource))), NodeCount: 1},
	}))
	fileGraph, err := worker.PrepareFileGraph(t.Context(), t.TempDir(), manifest.Witness, testHeadIncarnation)
	require.NoError(t, err)
	return worker, fileGraph
}

func publishRouteTestStage(t *testing.T, h *Head, tables StageTables) LocalPublicationRecord {
	t.Helper()
	digest, err := h.PublishStage(t.Context(), tables)
	require.NoError(t, err)
	event := uint64(100) + uint64(tables.Witness.Stage)
	return LocalPublicationRecord{
		Version: tables.Witness.Version(), Incarnation: tables.Incarnation, WitnessDigest: digest, EventID: &event,
	}
}

package store_pebble

import (
	"bytes"
	"encoding/gob"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildLanguageCoverageStages(t *testing.T) {
	for _, tc := range []struct {
		name     string
		admitted []string
		deferred []string
		edges    int
	}{
		{"supported", []string{"go", "javascript", "typescript"}, nil, 1},
		{"mixed", []string{"c", "go", "python"}, []string{"c", "python"}, 1},
		{"deferred", []string{"c"}, []string{"c"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			headDir := filepath.Join(t.TempDir(), "head")
			head, err := OpenHead(headDir, testHeadIncarnation, FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() {
				if head != nil {
					require.NoError(t, head.Close())
				}
			})
			worker, err := OpenBuild(filepath.Join(t.TempDir(), "working"), FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, worker.Close()) })

			var files []ManifestFile
			var declarations []*graph.Node
			for _, id := range tc.admitted {
				path := id + ".input" // use the admitted ID, not the suffix
				file := ManifestFile{Path: path, Language: id, Kind: 'f', Size: 10, Content: HeadDigest{3}, Extractor: HeadDigest{4}}
				files = append(files, file)
				node := &graph.Node{ID: path + "::declaration", Name: "decl_" + id, Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: path, Language: id}
				declarations = append(declarations, node)
				var edges []*graph.Edge
				if id == "go" {
					edges = []*graph.Edge{{From: node.ID, To: "unresolved::Fixture", Kind: graph.EdgeCalls, FilePath: path, Line: 1}}
				}
				require.NoError(t, worker.AddParsedFile("repo", ParseFacts{Nodes: []*graph.Node{node}, Edges: edges,
					File: graph.FileMetaRow{FilePath: path, Size: 10, ContentHash: "content", NodeCount: 1}}))
			}
			w := Witness{Input: HeadDigest{1}, Recipe: HeadDigest{2}, FrozenInputs: HeadDigest{9}, Profile: FullProfile, Stage: StageManifest}
			w.PlatformCatalogRevision = graph.PlatformCatalogRevision
			manifest, err := PrepareManifest(ctx, t.TempDir(), files, w, testHeadIncarnation)
			require.NoError(t, err)
			_, err = head.PublishStage(ctx, manifest)
			require.NoError(t, err)
			fileGraph, err := worker.PrepareFileGraph(ctx, t.TempDir(), manifest.Witness, testHeadIncarnation)
			require.NoError(t, err)
			base, err := worker.RetainFileGraph(ctx, fileGraph)
			require.NoError(t, err)
			bad := fileGraph
			bad.Witness.Relations = &RelationCoverage{}
			_, err = head.PublishStage(ctx, bad)
			require.ErrorIs(t, err, errInvalidRecord)
			before, _, err := head.ReconcilePublication(nil)
			require.NoError(t, err)
			require.Equal(t, manifest.Witness, before)
			_, err = head.PublishStage(ctx, fileGraph)
			require.NoError(t, err)

			pinned := head.OpenSnapshot()
			t.Cleanup(func() {
				if pinned != nil {
					require.NoError(t, pinned.Close())
				}
			})
			assertFileGraph := func() {
				t.Helper()
				got, err := pinned.Publication()
				require.NoError(t, err)
				require.Equal(t, fileGraph.Witness, got)
				require.ErrorAs(t, got.RequireRelationCoverage(nil), new(*ErrStagePending))
				require.Equal(t, len(declarations), pinned.NodeCount())
				require.Zero(t, pinned.EdgeCount())
				for i, n := range declarations {
					require.Equal(t, n, pinned.GetNode(n.ID))
					f, err := pinned.ManifestFile(n.FilePath)
					require.NoError(t, err)
					require.Equal(t, files[i], f)
					meta, err := pinned.FileMetadata(n.RepoPrefix, n.FilePath)
					require.NoError(t, err)
					require.EqualValues(t, 1, meta.NodeCount)
				}
			}
			assertFileGraph()

			require.NoError(t, worker.CheckRebuildIsolationCoverage(tc.deferred))
			relations, err := worker.PrepareRelations(ctx, t.TempDir(), base, nil)
			require.NoError(t, err)
			bad = relations
			bad.Witness.Recipe = HeadDigest{99}
			_, err = head.PublishStage(ctx, bad)
			require.ErrorIs(t, err, ErrStaleBase)
			for _, ids := range [][]string{{""}, {"c", "c"}, {"python", "c"}} {
				bad = relations
				bad.Witness.Relations = &RelationCoverage{DeferredLanguages: ids}
				_, err = head.PublishStage(ctx, bad)
				require.ErrorIs(t, err, errInvalidRecord)
			}
			assertState := func(snapshot *Snapshot, want Witness, edges int) {
				t.Helper()
				got, err := snapshot.Publication()
				require.NoError(t, err)
				require.Equal(t, want, got)
				require.Equal(t, want.Families, relationsSavedWitness(t, snapshot))
				require.Equal(t, len(declarations), snapshot.NodeCount())
				require.Equal(t, edges, snapshot.EdgeCount())
				require.NoError(t, snapshot.ReadError())
			}
			assertHead := func(want Witness, edges int) {
				t.Helper()
				s := head.OpenSnapshot()
				defer func() { require.NoError(t, s.Close()) }()
				assertState(s, want, edges)
			}
			assertHead(fileGraph.Witness, 0)
			digest, err := head.PublishStage(ctx, relations)
			require.NoError(t, err)
			assertHead(relations.Witness, tc.edges)
			assertFileGraph()
			require.NoError(t, pinned.Close())
			pinned = nil

			derived, err := PrepareDeferredDerived(t.TempDir(), relations)
			require.NoError(t, err)
			require.Equal(t, digest, derived.Base)
			bad = derived
			bad.Witness.Relations = nil
			_, err = head.PublishStage(ctx, bad)
			require.ErrorIs(t, err, errInvalidRecord)
			assertHead(relations.Witness, tc.edges)
			wantUnsupported := []UnsupportedFamily{
				{"analysis_cache", "D64"}, {"blame", "D64"}, {"churn", "D64"},
				{"content_search", "D64"}, {"contracts", "D64"}, {"coverage", "D64"}, {"releases", "D64"},
			}
			require.Equal(t, wantUnsupported, derived.Witness.Unsupported)
			require.Equal(t, relations.Witness.Families, derived.Witness.Families)
			bad = derived
			bad.Witness.Unsupported = nil
			_, err = head.PublishStage(ctx, bad)
			require.ErrorIs(t, err, errInvalidRecord)
			bad = derived
			bad.Witness.Families = append(slices.Clone(bad.Witness.Families), FamilyWitness{Family: 'Z'})
			_, err = head.PublishStage(ctx, bad)
			require.ErrorIs(t, err, errInvalidRecord, "even an empty complete family is a claim")
			bad = derived
			bad.Files = []string{"not-needed.sst"}
			_, err = head.PublishStage(ctx, bad)
			require.ErrorIs(t, err, errInvalidRecord, "a no-data transition must not ingest data tables")
			assertHead(relations.Witness, tc.edges)
			_, err = head.PublishStage(ctx, derived)
			require.NoError(t, err)
			search, err := worker.PrepareSearch(ctx, t.TempDir(), derived.Witness, testHeadIncarnation, func(n *graph.Node) (string, bool) {
				return n.Name, !slices.Contains(tc.deferred, n.Language)
			})
			require.NoError(t, err)
			bad = search
			bad.Witness.Relations = &RelationCoverage{DeferredLanguages: []string{"unexpected"}}
			_, err = head.PublishStage(ctx, bad)
			require.ErrorIs(t, err, errInvalidRecord)
			assertHead(derived.Witness, tc.edges)
			bad = search
			bad.Witness.Unsupported = nil
			_, err = head.PublishStage(ctx, bad)
			require.ErrorIs(t, err, errInvalidRecord)
			assertHead(derived.Witness, tc.edges)
			digest, err = head.PublishStage(ctx, search)
			require.NoError(t, err)
			require.NoError(t, head.Close())
			head = nil

			head, err = OpenHead(headDir, testHeadIncarnation, FullProfile)
			require.NoError(t, err)
			assertHead(search.Witness, tc.edges)
			s := stageRead(t, head)
			got, err := s.Publication()
			require.NoError(t, err)
			require.Equal(t, graph.PlatformCatalogRevision, got.PlatformCatalogRevision)
			require.Equal(t, wantUnsupported, got.Unsupported)
			hits, err := s.SearchContent("", "decl", 10)
			require.ErrorIs(t, err, errors.ErrUnsupported)
			require.Nil(t, hits)
			if tc.edges > 0 {
				require.NoError(t, got.RequireRelationCoverage([]string{"go"}))
				require.Len(t, s.GetOutEdges("go.input::declaration"), 1)
			}
			stats, err := point(s, searchKey("stats"), searchReadStats)
			require.NoError(t, err)
			require.EqualValues(t, len(tc.admitted)-len(tc.deferred), stats.Rows)
			if len(tc.deferred) == 0 {
				require.NoError(t, got.RequireRelationCoverage(nil))
			} else {
				var refusal *ErrLinksNotAvailable
				require.ErrorAs(t, got.RequireRelationCoverage([]string{"go", "c"}), &refusal)
				require.Equal(t, []string{"c"}, refusal.Languages)
				require.EqualError(t, refusal, "links not available for c")
				require.ErrorAs(t, got.RequireRelationCoverage(nil), &refusal)
				require.Equal(t, tc.deferred, refusal.Languages)
			}

			// A validly encoded delta with a worsening or unproved claim must
			// refuse even when it references the exact current base.
			bad = StageTables{Dir: t.TempDir(), Incarnation: testHeadIncarnation, Base: digest, Witness: search.Witness}
			bad.Witness.PlatformCatalogRevision++
			_, err = head.PublishDelta(ctx, bad)
			require.ErrorIs(t, err, ErrStaleBase, "a new catalog needs a fresh build")
			bad.Witness = search.Witness
			bad.Witness.Input = HeadDigest{42}
			for _, coverage := range []*RelationCoverage{nil, {DeferredLanguages: []string{"rust"}}} {
				bad.Witness.Relations = coverage
				_, err = head.PublishDelta(ctx, bad)
				require.ErrorIs(t, err, ErrCoverageRegression)
			}
			bad.Witness.Relations = search.Witness.Relations
			bad.Witness.Unsupported = nil
			_, err = head.PublishDelta(ctx, bad)
			require.ErrorIs(t, err, errInvalidRecord)
			assertHead(search.Witness, tc.edges)
		})
	}
}

func TestRebuildLanguageCoverageWitness(t *testing.T) {
	w := Witness{Input: HeadDigest{1}, Recipe: HeadDigest{2}, Profile: FullProfile, Stage: StageRelations}
	legacy := []byte{1, byte(StageRelations), 4, 'f', 'u', 'l', 'l'}
	legacy = append(legacy, w.Input[:]...)
	legacy = append(legacy, w.Recipe[:]...)
	version := w.Version()
	legacy = append(legacy, version[:]...)
	legacy = append(legacy, 0, 0) // no dependencies or families
	for _, frozen := range []HeadDigest{{}, {3}} {
		w.FrozenInputs = frozen
		want := bytes.Clone(legacy)
		if frozen != (HeadDigest{}) {
			want[0] = 2
			want = append(want, frozen[:]...)
		}
		raw, err := encodeWitness(w)
		require.NoError(t, err)
		require.Equal(t, want, raw)
		got, err := decodeWitness(want)
		require.NoError(t, err)
		require.Equal(t, w, got)
		var refusal *ErrLinksNotAvailable
		require.ErrorAs(t, got.RequireRelationCoverage(nil), &refusal)
		require.Nil(t, refusal.Languages)
	}

	// The worker sends Witness using gob. An explicitly empty declaration
	// must survive that hop and stay distinct from a legacy witness.
	w.Relations = &RelationCoverage{}
	var transport bytes.Buffer
	require.NoError(t, gob.NewEncoder(&transport).Encode(w))
	var got Witness
	require.NoError(t, gob.NewDecoder(&transport).Decode(&got))
	require.Equal(t, w, got)
	raw, err := encodeWitness(got)
	require.NoError(t, err)
	prefix := append(bytes.Clone(legacy), w.FrozenInputs[:]...)
	prefix[0] = 3
	require.Equal(t, append(bytes.Clone(prefix), 0), raw)
	for _, broken := range [][]byte{
		{},                       // absent count
		{0, 0},                   // trailing bytes
		{0x80, 0},                // noncanonical count
		{1, 3, 'c'},              // truncated ID
		{2, 0, 2, 't', 's'},      // empty ID
		{2, 1, 'c', 1, 'c'},      // duplicate
		{2, 2, 't', 's', 1, 'c'}, // unsorted
	} {
		_, err := decodeWitness(append(bytes.Clone(prefix), broken...))
		require.ErrorIs(t, err, errInvalidRecord, "coverage payload %v", broken)
	}
	raw[1] = byte(StageManifest)
	_, err = decodeWitness(raw)
	require.ErrorIs(t, err, errInvalidRecord)
}

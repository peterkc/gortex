package store_pebble

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestParsedFileGraphPublishesDeclarationsWithoutRelations(t *testing.T) {
	ctx := t.Context()
	w := Witness{Input: HeadDigest{1}, Recipe: HeadDigest{2}, Profile: FullProfile, Stage: StageManifest}
	manifest, err := PrepareManifest(ctx, t.TempDir(), []ManifestFile{{
		Path: "a.go", Language: "go", Size: 64, Kind: 'f', Content: HeadDigest{3}, Extractor: HeadDigest{4},
	}}, w, testHeadIncarnation)
	require.NoError(t, err)
	h, err := OpenHead(filepath.Join(t.TempDir(), "head"), testHeadIncarnation, FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	base, err := h.PublishStage(ctx, manifest)
	require.NoError(t, err)
	pinned := stageRead(t, h)
	pending, err := pinned.ManifestFile("a.go")
	require.NoError(t, err)
	require.Equal(t, HeadDigest{3}, pending.Content)

	worker, err := OpenBuild(filepath.Join(t.TempDir(), "worker"), FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, worker.Close()) })
	dir := t.TempDir()
	unsaved, err := worker.PrepareFileGraph(ctx, dir, manifest.Witness, testHeadIncarnation)
	require.ErrorContains(t, err, "not every admitted file")
	require.Empty(t, unsaved.Files)
	decl := &graph.Node{ID: "a.go::F", Name: "F", FilePath: "a.go", Language: "go", Kind: graph.KindFunction}
	importNode := &graph.Node{ID: "a.go::import::other", Name: "other", FilePath: "a.go", Language: "go", Kind: graph.KindImport}
	receipt := graph.FileMetaRow{FilePath: "a.go", ContentHash: "digest", Size: 64, NodeCount: 2, Errors: "[2:3]"}
	facts := ParseFacts{
		Nodes: []*graph.Node{decl, importNode},
		Edges: []*graph.Edge{{From: decl.ID, To: "unresolved::G", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 3}},
		File:  receipt,
	}
	require.NoError(t, worker.AddParsedFile("", facts))
	stage, err := worker.PrepareFileGraph(ctx, dir, manifest.Witness, testHeadIncarnation)
	require.NoError(t, err)
	require.Equal(t, base, stage.Base)
	require.NotEmpty(t, stage.Files)
	_, err = h.PublishStage(ctx, stage)
	require.NoError(t, err)
	current := stageRead(t, h)
	require.NoError(t, current.RequireStage(StageFileGraph))
	seen, err := current.FileMetadata("", "a.go")
	require.NoError(t, err)
	require.Equal(t, receipt, seen)
	require.Equal(t, 2, current.NodeCount())
	require.Len(t, current.FindNodesByName("F"), 1)
	require.Equal(t, graph.KindImport, current.GetNode(importNode.ID).Kind)
	require.Zero(t, current.EdgeCount())
	require.Empty(t, current.GetOutEdges(decl.ID))
	private, err := worker.pendingEdge(pendingEdgeKey(facts.Edges[0]))
	require.NoError(t, err)
	require.NotNil(t, private, "the raw parse edge must still exist for later stages")
	require.Nil(t, pinned.GetNode(decl.ID))
	var stagePending *ErrStagePending
	_, err = pinned.FileMetadata("", "a.go")
	require.ErrorAs(t, err, &stagePending)
	require.Equal(t, StageManifest, stagePending.Published)
	require.ErrorContains(t, worker.AddParsedFile("", facts), "repeated parse facts")
}

func TestParsedFileGraphKeepsFirstEdgePayload(t *testing.T) {
	const file = "a.go"
	// Sink(http.StatusCreated, http.StatusCreated) emits the same ArgOf
	// identity for both arguments, with different positional metadata.
	first := &graph.Edge{
		From: "unresolved::extern::net/http::StatusCreated", To: "unresolved::Sink",
		Kind: graph.EdgeArgOf, FilePath: file, Line: 3, Origin: graph.OriginASTInferred,
		Meta: map[string]any{"arg_position": 0},
	}
	second := *first
	second.Meta = map[string]any{"arg_position": 1}
	edges := []*graph.Edge{first, &second}
	native, err := store_sqlite.Open(filepath.Join(t.TempDir(), "oracle.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, native.Close()) })
	native.AddBatch(nil, edges)
	want := native.GetOutEdges(first.From)
	require.Len(t, want, 1)

	worker := newBuild(t, FullProfile)
	require.NoError(t, worker.AddParsedFile("", ParseFacts{
		EffectiveLanguage: "go", Edges: edges, File: graph.FileMetaRow{FilePath: file},
	}))
	var got []*graph.Edge
	require.NoError(t, worker.RebuildView().VisitEdges(t.Context(), func(e *graph.Edge) error {
		got = append(got, e)
		return nil
	}))
	require.Len(t, got, 1)
	position, err := json.Marshal(want[0].Meta["arg_position"])
	require.NoError(t, err)
	require.Equal(t, "0", string(position))
	wantJSON, err := json.Marshal(want)
	require.NoError(t, err)
	gotJSON, err := json.Marshal(got)
	require.NoError(t, err)
	require.Equal(t, wantJSON, gotJSON)
	requireRebuildLinkCustody(t, worker, first, rebuildEdgeCustody{proof: rebuildProofGoDataflowSource})
}

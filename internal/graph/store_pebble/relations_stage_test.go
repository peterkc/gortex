package store_pebble

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/graph"
)

func TestRelationsStagePublishesExactDiff(t *testing.T) {
	ctx := t.Context()
	worker, head, base, declared, raw, file, receipt := relationsStageFixture(t, FullProfile)
	pinned := stageRead(t, head)
	prior, err := pinned.Publication()
	require.NoError(t, err)
	require.Equal(t, StageFileGraph, prior.Stage)
	require.Zero(t, pinned.EdgeCount())
	relationsStageLevels(t, head, "file_graph")

	replacement := *declared
	replacement.Name, replacement.QualName, replacement.RepoPrefix, replacement.FilePath = "NewName", "new.NewName", "new", "new.go"
	replacement.Kind = graph.KindType
	synth := &graph.Node{ID: "synth::F", Name: "F", Kind: graph.KindFunction, RepoPrefix: "new", FilePath: "synth.go", Language: "go"}
	worker.AddBatch([]*graph.Node{&replacement, synth}, nil)
	linked := *raw
	linked.To = synth.ID
	worker.ReindexEdge(&linked, raw.To)
	unresolved := linked
	unresolved.To, unresolved.Line = "unresolved::StillExternal", linked.Line+1
	worker.AddEdge(&unresolved)

	tables, err := worker.PrepareRelations(ctx, t.TempDir(), base)
	require.NoError(t, err)
	require.NotEmpty(t, tables.Files)
	_, err = head.PublishStage(ctx, tables)
	require.NoError(t, err)
	relationsStageLevels(t, head, "relations")
	current := stageRead(t, head)
	w, err := current.Publication()
	require.NoError(t, err)
	require.Equal(t, StageRelations, w.Stage)
	require.Equal(t, prior.Version(), w.Version())
	require.Equal(t, 2, current.NodeCount())
	require.Equal(t, 2, current.EdgeCount())
	require.Equal(t, &replacement, current.GetNode(declared.ID))
	require.Equal(t, synth, current.GetNode(synth.ID))
	require.Len(t, current.GetOutEdges(declared.ID), 2)
	require.Equal(t, []*graph.Edge{&linked}, current.GetInEdges(synth.ID))
	require.Equal(t, []*graph.Edge{&unresolved}, current.GetInEdges(unresolved.To))
	require.Empty(t, current.GetInEdges(raw.To))
	require.Empty(t, current.FindNodesByName(declared.Name))
	require.Nil(t, current.GetNodeByQualName(declared.QualName))
	require.Empty(t, current.GetRepoNodes(declared.RepoPrefix))
	require.Empty(t, current.GetFileNodes(declared.FilePath))
	require.Empty(t, slices.Collect(current.NodesByKind(graph.KindImport)))
	require.Equal(t, []*graph.Node{&replacement}, current.FindNodesByName(replacement.Name))
	require.Equal(t, &replacement, current.GetNodeByQualName(replacement.QualName))
	require.Len(t, current.GetRepoNodes(replacement.RepoPrefix), 2)
	require.Equal(t, []*graph.Node{&replacement}, current.GetFileNodes(replacement.FilePath))
	require.Equal(t, []*graph.Node{&replacement}, slices.Collect(current.NodesByKind(replacement.Kind)))
	seenFile, err := current.ManifestFile(file.Path)
	require.NoError(t, err)
	require.Equal(t, file, seenFile)
	seenReceipt, err := current.FileMetadata(declared.RepoPrefix, file.Path)
	require.NoError(t, err)
	require.Equal(t, receipt, seenReceipt)

	// Compare every witness family with the actual owner-ingested bytes.
	// In particular an old secondary key surviving ingest changes a digest.
	require.Equal(t, w.Families, relationsSavedWitness(t, current))
	require.Len(t, w.Families, 9)
	oldWitness, err := pinned.Publication()
	require.NoError(t, err)
	require.Equal(t, prior, oldWitness)
	require.Equal(t, prior.Families, relationsSavedWitness(t, pinned))
	require.Equal(t, []*graph.Node{declared}, pinned.FindNodesByName(declared.Name))
	require.Nil(t, pinned.GetNode(synth.ID))
	require.Zero(t, pinned.EdgeCount())
	_, err = head.PublishStage(ctx, tables)
	require.ErrorIs(t, err, ErrStageOrder)
}

func TestRelationsStageRefusesDanglingOrProxyEndpoint(t *testing.T) {
	for _, tc := range []struct {
		label     string
		from      string
		to        string
		proxyNode bool
		message   string
	}{
		{label: "unknown source", from: "missing.go::Missing", message: "dangling relations endpoint"},
		{label: "unknown target", to: "missing.go::Missing", message: "dangling relations endpoint"},
		{label: "proxy ID", to: graph.ProxyNodeID("remote", "pkg.go::F"), message: "proxy edge cannot be published"},
		{label: "proxy node", to: "local.go::Proxy", proxyNode: true, message: "proxy relations endpoint"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			worker, head, base, declared, raw, _, _ := relationsStageFixture(t, FullProfile)
			pinned := stageRead(t, head)
			prior, err := pinned.Publication()
			require.NoError(t, err)
			priorRows := relationsSavedRows(t, pinned)
			require.NotEmpty(t, priorRows)
			if tc.proxyNode {
				worker.AddNode(&graph.Node{ID: tc.to, Name: "Proxy", Kind: graph.KindFunction, Origin: "remote:remote", Stub: true})
			}
			linked := *raw
			if tc.from == "" {
				linked.To = tc.to
				worker.ReindexEdge(&linked, raw.To)
			} else {
				linked.From = tc.from
				linked.To = declared.ID
				worker.AddEdge(&linked)
			}
			tables, err := worker.PrepareRelations(t.Context(), t.TempDir(), base)
			require.ErrorContains(t, err, tc.message)
			require.ErrorIs(t, err, ErrRelationsSeal)
			require.ErrorIs(t, err, errInvalidRecord)
			require.Empty(t, tables)

			current := stageRead(t, head)
			w, err := current.Publication()
			require.NoError(t, err)
			require.Equal(t, prior, w)
			require.Equal(t, priorRows, relationsSavedRows(t, current))
			require.Equal(t, prior.Families, relationsSavedWitness(t, current))
			require.Equal(t, 1, current.NodeCount())
			require.Equal(t, declared, current.GetNode(declared.ID))
			require.Zero(t, current.EdgeCount())
		})
	}
}

func TestRelationsStageKeepsProfileOmittedEndpointAccounting(t *testing.T) {
	ctx := t.Context()
	worker, head, base, declared, raw, _, _ := relationsStageFixture(t, DefaultProfile)
	param := &graph.Node{ID: "old.go::OldName#param:p", Name: "p", Kind: graph.KindParam, FilePath: declared.FilePath, RepoPrefix: declared.RepoPrefix, Language: "go"}
	edge := &graph.Edge{From: param.ID, To: declared.ID, Kind: graph.EdgeTypedAs, FilePath: declared.FilePath, Line: raw.Line}
	worker.AddBatch([]*graph.Node{param}, []*graph.Edge{edge})
	require.True(t, worker.RemoveEdge(raw.From, raw.To, raw.Kind))

	tables, err := worker.PrepareRelations(ctx, t.TempDir(), base)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, tables)
	require.NoError(t, err)
	current := stageRead(t, head)
	w, err := current.Publication()
	require.NoError(t, err)
	require.Equal(t, StageRelations, w.Stage)
	require.Equal(t, w.Families, relationsSavedWitness(t, current))
	require.Equal(t, 1, current.NodeCount())
	require.Equal(t, 1, current.EdgeCount())
	require.Nil(t, current.GetNode(param.ID))
	require.Equal(t, []*graph.Edge{edge}, current.GetInEdges(declared.ID))
	require.Equal(t, 1, current.count("out total", param.ID))
	require.Equal(t, 1, current.count("in total", declared.ID))
	require.Equal(t, 1, current.count("off incoming owner", declared.ID, param.RepoPrefix))
	require.Equal(t, 1, current.count("repo edges", ""))
	require.Zero(t, current.count("repo edges", param.RepoPrefix))
}

func TestRelationsStageRefusesIncompleteRetainedBase(t *testing.T) {
	worker, head, base, _, _, _, _ := relationsStageFixture(t, FullProfile)
	prior, err := stageRead(t, head).Publication()
	require.NoError(t, err)
	require.NotEmpty(t, base.paths)
	base.paths = base.paths[:len(base.paths)-1]
	tables, err := worker.PrepareRelations(t.Context(), t.TempDir(), base)
	require.ErrorIs(t, err, ErrRelationsSeal)
	require.ErrorContains(t, err, "retained file-graph rows differ from their witness")
	require.Empty(t, tables)
	current := stageRead(t, head)
	require.Equal(t, prior.Families, relationsSavedWitness(t, current))
	require.Zero(t, current.EdgeCount())
}

func relationsStageFixture(t *testing.T, profile Profile) (*BuildStore, *Head, FileGraphBase, *graph.Node, *graph.Edge, ManifestFile, graph.FileMetaRow) {
	t.Helper()
	ctx := t.Context()
	dir := t.TempDir()
	worker, err := OpenBuild(filepath.Join(dir, "working"), profile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, worker.Close()) })
	head, err := OpenHead(filepath.Join(dir, "head"), testHeadIncarnation, profile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, head.Close()) })
	file := ManifestFile{Path: "old.go", Language: "go", Size: 17, Kind: 'f', Content: HeadDigest{3}, Extractor: HeadDigest{4}}
	w := Witness{Input: HeadDigest{1}, Recipe: HeadDigest{2}, Profile: profile, Stage: StageManifest}
	manifest, err := PrepareManifest(ctx, t.TempDir(), []ManifestFile{file}, w, testHeadIncarnation)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, manifest)
	require.NoError(t, err)
	declared := &graph.Node{ID: "old.go::OldName", Name: "OldName", QualName: "old.OldName", Kind: graph.KindImport, RepoPrefix: "old", FilePath: file.Path, Language: "go"}
	raw := &graph.Edge{From: declared.ID, To: "unresolved::Raw", Kind: graph.EdgeCalls, FilePath: file.Path, Line: 2}
	receipt := graph.FileMetaRow{FilePath: file.Path, Size: 17, ContentHash: "content", NodeCount: 1, Errors: "[1:7]"}
	require.NoError(t, worker.AddParsedFile(declared.RepoPrefix, ParseFacts{Nodes: []*graph.Node{declared}, Edges: []*graph.Edge{raw}, File: receipt}))
	stage, err := worker.PrepareFileGraph(ctx, t.TempDir(), manifest.Witness, testHeadIncarnation)
	require.NoError(t, err)
	require.NotEmpty(t, stage.Files)
	base, err := worker.RetainFileGraph(ctx, stage)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, stage)
	require.NoError(t, err)
	for _, name := range stage.Files {
		_, err := os.Stat(filepath.Join(stage.Dir, name))
		require.ErrorIs(t, err, os.ErrNotExist, "owner ingest must consume the outward name")
	}
	return worker, head, base, declared, raw, file, receipt
}

func relationsSavedRows(t *testing.T, snap *Snapshot) []row {
	t.Helper()
	var rows []row
	require.NoError(t, scan(t.Context(), snap, nil, func(key, value []byte) (bool, error) {
		rows = append(rows, row{bytes.Clone(key), bytes.Clone(value)})
		return true, nil
	}))
	return rows
}

func relationsSavedWitness(t *testing.T, snap *Snapshot) []FamilyWitness {
	t.Helper()
	var families []FamilyWitness
	hash := blake3.New()
	err := scan(t.Context(), snap, nil, func(key, value []byte) (bool, error) {
		if len(key) == 1 && key[0] == familyWitness {
			return true, nil
		}
		if len(families) == 0 || key[0] != families[len(families)-1].Family {
			families = append(families, FamilyWitness{Family: key[0]})
			hash.Reset()
		}
		writeWitnessRow(hash, key, value)
		last := &families[len(families)-1]
		last.Count++
		last.Digest = HeadDigest(hash.Sum(nil))
		return true, nil
	})
	require.NoError(t, err)
	return families
}

func relationsStageLevels(t *testing.T, head *Head, stage string) {
	t.Helper()
	var tables []int64
	for _, level := range head.db.Metrics().Levels {
		tables = append(tables, int64(level.TablesCount))
	}
	t.Logf("%s: Pebble tables by level: %v", stage, tables)
}

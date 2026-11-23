package indexer

import (
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/indexer/merkle"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

func TestRebuildFileGraphSharedClaims(t *testing.T) {
	const first, last = "a.go", "z.go"
	column := func(name, path string, line int) *graph.Node {
		return &graph.Node{
			ID: "col::postgres::public.users." + name, Kind: graph.KindColumn,
			Name: name, FilePath: path, Language: "go", StartLine: line,
			Meta: map[string]any{"table": "users", "column": name, "dialect": "postgres", "schema": "public"},
		}
	}
	for _, tc := range []struct {
		name                         string
		duplicateNode, duplicateEdge bool
	}{
		{name: "stamped_path_wins"},
		{name: "unexplained_node", duplicateNode: true},
		{name: "duplicate_edge_identity", duplicateEdge: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			reg := parser.NewRegistry()
			reg.Register(languages.NewGoExtractor())
			idx := New(graph.New(), reg, config.Default().Index, zap.NewNop())
			idx.installRebuildParsePolicy(rebuildParsePolicy{})
			t.Cleanup(idx.Close)

			// One winner comes from each receipt. Always keeping the first
			// or last receipt would pick the wrong stamped path for one column.
			email, phone := column("email", first, 29), column("phone", first, 37)
			a := &graph.Node{ID: first + "::use", Kind: graph.KindFunction, Name: "use", FilePath: first, Language: "go"}
			z := &graph.Node{ID: last + "::use", Kind: graph.KindFunction, Name: "use", FilePath: last, Language: "go"}
			if tc.duplicateNode {
				z.ID = a.ID
			}
			aSite := &graph.Edge{From: a.ID, To: email.ID, Kind: graph.EdgeReadsCol, FilePath: first, Line: 6, Origin: "first"}
			zSite := *aSite
			zSite.FilePath, zSite.Origin = last, "last"
			if tc.duplicateEdge {
				zSite.FilePath = aSite.FilePath // Origin is not part of the exact identity.
			}

			build, workDir := openRebuildTestFileStore(t)
			recipe := rebuildDigest{5}
			job := rebuildFileStages{
				workDir: workDir, incarnation: rebuildTestIncarnation,
				recipes: map[string]rebuildDigest{"go": recipe},
			}
			store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
			var meta []graph.FileMetaRow
			for _, f := range []struct {
				path  string
				nodes []*graph.Node
				site  *graph.Edge
			}{
				{first, []*graph.Node{a, email, column("phone", last, 3)}, aSite},
				{last, []*graph.Node{z, column("email", last, 5), phone}, &zSite},
			} {
				input := rebuildInput{Path: f.path, Language: "go", Kind: merkle.RegularFile, Size: 1, Content: rebuildDigest{1}}
				job.inputs = append(job.inputs, input)
				row := graph.FileMetaRow{FilePath: f.path, ContentHash: "hash:" + f.path, Size: 1, NodeCount: len(f.nodes)}
				meta = append(meta, row)
				payload, err := store_pebble.EncodeParseFacts(store_pebble.ParseFacts{
					File: row, Nodes: f.nodes, Edges: []*graph.Edge{f.site}, EffectiveLanguage: "go",
				})
				require.NoError(t, err)
				require.NoError(t, store.write(input.parseKey(idx.rebuildExtractorRecipe(recipe, input)), payload))
			}
			root, err := rebuildInputRoot(job.inputs)
			require.NoError(t, err)
			digest, err := hex.DecodeString(root)
			require.NoError(t, err)
			job.witness = store_pebble.Witness{
				Input: store_pebble.HeadDigest(digest), Recipe: store_pebble.HeadDigest{8},
				Profile: store_pebble.FullProfile, Stage: store_pebble.StageManifest,
			}

			head, err := store_pebble.OpenHead(filepath.Join(t.TempDir(), "head"), job.incarnation, job.witness.Profile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, head.Close()) })
			var offered []store_pebble.Stage
			// Only the two preseeded receipts are available; no source can be read.
			counts, err := idx.buildRebuildFileStages(ctx, nil, store, build, job, func(tables store_pebble.StageTables) error {
				offered = append(offered, tables.Witness.Stage)
				_, err := head.PublishStage(ctx, tables)
				return err
			})
			snapshot := head.OpenSnapshot()
			t.Cleanup(func() { require.NoError(t, snapshot.Close()) })
			var manifest []string
			require.NoError(t, snapshot.ManifestFiles(ctx, func(f store_pebble.ManifestFile) error {
				manifest = append(manifest, f.Path)
				return nil
			}))
			require.Equal(t, []string{first, last}, manifest)

			if tc.duplicateNode || tc.duplicateEdge {
				var refusal *store_pebble.RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refusal)
				require.EqualValues(t, 1, refusal.Count)
				require.EqualValues(t, 2, counts.claims.AcceptedColumns)
				require.Equal(t, []store_pebble.Stage{store_pebble.StageManifest}, offered)
				var pending *store_pebble.ErrStagePending
				require.ErrorAs(t, snapshot.RequireStage(store_pebble.StageFileGraph), &pending)
				requireRebuildTestFileRows(t, build, nil, nil)
				return
			}
			require.NoError(t, err)
			require.Zero(t, counts.parsed)
			require.EqualValues(t, 2, counts.reused)
			require.EqualValues(t, 2, counts.claims.Shadowed)
			require.EqualValues(t, 2, counts.claims.AcceptedColumns)
			require.Zero(t, counts.claims.UnexplainedNodes)
			require.Zero(t, counts.claims.ShadowedEdges)
			require.Equal(t, []store_pebble.Stage{store_pebble.StageManifest, store_pebble.StageFileGraph}, offered)
			requireRebuildTestFileRows(t, build, []*graph.Node{a, z, email, phone}, []*graph.Edge{aSite, &zSite})
			require.Equal(t, email, snapshot.GetNode(email.ID))
			require.Equal(t, phone, snapshot.GetNode(phone.ID))
			require.ElementsMatch(t, []*graph.Node{a, email, phone}, snapshot.GetFileNodes(first))
			require.Equal(t, []*graph.Node{z}, snapshot.GetFileNodes(last))
			for _, want := range meta {
				got, err := snapshot.FileMetadata("", want.FilePath)
				require.NoError(t, err)
				require.Equal(t, want, got)
			}
		})
	}
}

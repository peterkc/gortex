package indexer

import (
	"iter"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestPrepareRebuildContractsChecksActualSeal(t *testing.T) {
	const path, contractID, privateID, seedID, methodID = "a.go", "env::KEPT", "env::PRIVATE", "dep::example.org/seed", "a.go::Method"
	for _, mode := range []string{"equal", "private mismatch", "unemitted selected ID", "retained extra seed", "wrong Call status", "legacy catalog"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			b, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "working"), store_pebble.FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, b.Close()) })
			head, err := store_pebble.OpenHead(filepath.Join(t.TempDir(), "head"), rebuildTestIncarnation, store_pebble.FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, head.Close()) })
			revision := graph.PlatformCatalogRevision
			if mode == "legacy catalog" {
				revision = 0
			}
			manifest, err := store_pebble.PrepareManifest(ctx, t.TempDir(), []store_pebble.ManifestFile{{Path: path, Kind: 'f', Language: "go"}}, store_pebble.Witness{
				Input: store_pebble.HeadDigest{1}, Recipe: store_pebble.HeadDigest{2}, Stage: store_pebble.StageManifest,
				Profile: store_pebble.FullProfile, PlatformCatalogRevision: revision,
			}, rebuildTestIncarnation)
			require.NoError(t, err)
			_, err = head.PublishStage(ctx, manifest)
			require.NoError(t, err)
			file := &graph.Node{ID: path, Name: path, FilePath: path, Kind: graph.KindFile, Language: "go"}
			method := &graph.Node{ID: methodID, Name: "Method", FilePath: path, Kind: graph.KindMethod, Language: "go"}
			require.NoError(t, b.AddParsedFile("", store_pebble.ParseFacts{
				Nodes: []*graph.Node{file, method}, File: graph.FileMetaRow{FilePath: path, NodeCount: 2}, EffectiveLanguage: "go",
			}))
			files, err := b.PrepareFileGraph(ctx, t.TempDir(), manifest.Witness, rebuildTestIncarnation)
			require.NoError(t, err)
			base, err := b.RetainFileGraph(ctx, files)
			require.NoError(t, err)
			_, err = head.PublishStage(ctx, files)
			require.NoError(t, err)
			if mode == "retained extra seed" {
				b.AddNode(&graph.Node{ID: seedID, Kind: graph.KindContract, Name: seedID, Language: "contract", FilePath: "go.mod",
					Meta: map[string]any{"type": "dependency", "role": "consumer"}})
			}
			relations, err := b.PrepareRelations(ctx, t.TempDir(), base, nil)
			require.NoError(t, err)
			retained, err := b.RetainRelations(ctx, base, relations)
			require.NoError(t, err)
			digest, err := head.PublishStage(ctx, relations)
			require.NoError(t, err)

			early := rebuildEarlyContractIDs{contractID: path, privateID: path}
			complete := rebuildEarlyContractIDs{contractID: path, privateID: path}
			selected := rebuildEarlyContractIDs{contractID: path}
			batch := store_pebble.RebuildContractBatch{
				Nodes: []*graph.Node{{ID: contractID, Kind: graph.KindContract, Name: contractID, Language: "contract", FilePath: path}},
				Edges: []*graph.Edge{{From: path, To: methodID, Kind: graph.EdgeCalls, FilePath: path, Line: 1,
					Meta: map[string]any{"via": "spring.Bean", "bean_of": "Example"}}},
				Omitted: []store_pebble.RebuildContractOmission{{
					FilePath: path, ID: privateID, Type: contracts.ContractEnv, RecordKey: store_pebble.HeadDigest{1},
				}},
			}
			switch mode {
			case "private mismatch":
				delete(complete, privateID)
			case "unemitted selected ID":
				selected[privateID] = path
			case "wrong Call status":
				batch.Edges[0].Meta[graph.MetaLinkStatus] = string(graph.LinkStatusResolved)
			case "legacy catalog":
				batch.Edges[0].To = graph.StubID("", graph.StubKindBuiltin, "go", "len")
				_, exists := graph.LookupPlatformBuiltin(graph.PlatformCatalogRevision, batch.Edges[0].To)
				require.True(t, exists, "current catalog recognizes the probe")
			}
			consumed := false
			var batches iter.Seq2[store_pebble.RebuildContractBatch, error] = func(yield func(store_pebble.RebuildContractBatch, error) bool) {
				consumed = true
				yield(batch, nil)
			}
			tables, err := prepareRebuildContracts(ctx, b, t.TempDir(), retained, early, orderedEarlyContractIDs(complete),
				orderedEarlyContractIDs(selected), batches, store_pebble.ContractCoverage{OmittedRecords: 1}, func(string) string { return "go" })
			switch mode {
			case "equal":
				require.NoError(t, err)
				require.Equal(t, digest, tables.Base)
				require.Equal(t, store_pebble.StageContracts, tables.Witness.Stage)
				require.EqualValues(t, 1, tables.Witness.Contracts.OmittedRecords)
				_, err = head.PublishStage(ctx, tables)
				require.NoError(t, err)
				return
			case "private mismatch":
				require.ErrorIs(t, err, errRebuildContractIDs)
				require.ErrorContains(t, err, "compare complete private contract IDs")
				require.False(t, consumed, "the stage writer must not start on a private mismatch")
			case "unemitted selected ID":
				require.ErrorIs(t, err, errRebuildContractIDs)
				require.ErrorContains(t, err, `missing id "env::PRIVATE"`)
			case "retained extra seed":
				require.ErrorIs(t, err, errRebuildContractIDs)
				require.ErrorContains(t, err, `extra id "dep::example.org/seed"`)
			case "wrong Call status":
				require.ErrorIs(t, err, store_pebble.ErrContractsSeal)
				require.ErrorContains(t, err, "link_status outside a builtin Call")
			case "legacy catalog":
				require.ErrorIs(t, err, store_pebble.ErrContractsSeal)
				// Revision zero cannot reach the Go catalog's validation branch.
				require.ErrorContains(t, err, "builtin target recording language is not admitted")
			}
			require.Zero(t, tables)
			snapshot := head.OpenSnapshot()
			defer snapshot.Close()
			require.NoError(t, snapshot.RequireStage(store_pebble.StageRelations))
			require.Error(t, snapshot.RequireStage(store_pebble.StageContracts))
		})
	}
}

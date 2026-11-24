package store_pebble

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildSearchStageMatchesLegacySeal(t *testing.T) {
	for _, profile := range []Profile{DefaultProfile, FullProfile} {
		t.Run(string(profile), func(t *testing.T) {
			ctx := t.Context()
			worker := newBuild(t, profile)
			head, err := OpenHead(filepath.Join(t.TempDir(), "head"), testHeadIncarnation, profile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, head.Close()) })
			const path, repo, language = "new/src.py", "new", "python"
			manifest, err := PrepareManifest(ctx, t.TempDir(), []ManifestFile{{Path: path, Kind: 'f', Language: language}}, Witness{
				Input: HeadDigest{1}, Recipe: HeadDigest{2}, Stage: StageManifest, Profile: profile,
			}, testHeadIncarnation)
			require.NoError(t, err)
			_, err = head.PublishStage(ctx, manifest)
			require.NoError(t, err)
			retained := &graph.Node{ID: path + "::Retained", Name: "Retained", Kind: graph.KindFunction, RepoPrefix: repo, FilePath: path, Language: language}
			noTerms := &graph.Node{ID: path + "::NoTerms", Name: "NoTerms", Kind: graph.KindFunction, RepoPrefix: repo, FilePath: path, Language: language}
			param := &graph.Node{ID: path + "::Retained#param:p", Name: "p", Kind: graph.KindParam, RepoPrefix: repo, FilePath: path, Language: language}
			require.NoError(t, worker.AddParsedFile(repo, ParseFacts{
				Nodes: []*graph.Node{retained, noTerms, param}, File: graph.FileMetaRow{FilePath: path, NodeCount: 3},
			}))
			fileGraph, err := worker.PrepareFileGraph(ctx, t.TempDir(), manifest.Witness, manifest.Incarnation)
			require.NoError(t, err)
			base, err := worker.RetainFileGraph(ctx, fileGraph)
			require.NoError(t, err)
			_, err = head.PublishStage(ctx, fileGraph)
			require.NoError(t, err)

			module := &graph.Node{ID: "external-call::dep::example.org/lib", Name: "Lib", Kind: graph.KindModule}
			linked := &graph.Node{ID: "excluded/src.py::LinkAlias", Name: "LinkAlias", Kind: graph.KindFunction, RepoPrefix: "excluded", FilePath: "excluded/src.py", Language: language}
			worker.AddBatch([]*graph.Node{module, linked}, nil)
			derived := publishRebuildSearchDerived(t, worker, head, base)
			pinned := stageRead(t, head)
			require.NoError(t, worker.UpsertSymbolFTS(linked.ID, "linkalias"))
			// Declare the corpus explicitly, rather than classifying by language.
			wantTokens := map[string]string{retained.ID: "retained arg", noTerms.ID: "...", param.ID: "arg", module.ID: "lib"}
			stage, err := worker.PrepareSearch(ctx, t.TempDir(), derived.Witness, derived.Incarnation, func(n *graph.Node) (string, bool) {
				text, ok := wantTokens[n.ID]
				return text, ok
			})
			require.NoError(t, err)
			require.NotEmpty(t, stage.Files)
			_, err = worker.OpenSnapshot()
			require.ErrorIs(t, err, ErrUnpublishedBuild)
			_, err = head.PublishStage(ctx, stage)
			require.NoError(t, err)
			current := stageRead(t, head)
			w, err := current.Publication()
			require.NoError(t, err)
			require.Equal(t, StageSearch, w.Stage)
			require.Equal(t, derived.Witness.Version(), w.Version())
			require.Equal(t, w.Families, relationsSavedWitness(t, current))
			before, err := pinned.Publication()
			require.NoError(t, err)
			require.Equal(t, derived.Witness, before)
			var pending *ErrStagePending
			require.ErrorAs(t, pinned.RequireStage(StageSearch), &pending)
			require.Equal(t, StageDerived, pending.Published)
			require.Empty(t, rebuildSearchRows(t, pinned, []byte{familySymbol}))

			require.NotNil(t, current.GetNode(linked.ID), "the link-derived candidate is a graph node, not a search document")
			for _, tc := range []struct {
				query, id string
				exact     bool
			}{
				{query: "Retained", id: retained.ID, exact: true},
				{query: "retained retained", id: retained.ID}, // Two terms bypass exact lookup.
				{query: "NoTerms", id: noTerms.ID, exact: true},
				{query: "noterms noterms"},
				{query: "LinkAlias"},
				{query: "linkalias linkalias"},
			} {
				hits, err := current.SearchSymbols(tc.query, 10)
				require.NoError(t, err, tc.query)
				if tc.id == "" {
					require.Empty(t, hits, tc.query)
					continue
				}
				require.Len(t, hits, 1, tc.query)
				require.Equal(t, tc.id, hits[0].NodeID, tc.query)
				if tc.exact {
					require.Equal(t, float64(100), hits[0].Score, tc.query)
				} else {
					require.Positive(t, hits[0].Score, tc.query)
					require.Less(t, hits[0].Score, float64(100), tc.query)
				}
			}

			// The legacy oracle contains exactly the declared corpus.
			legacy := newBuild(t, profile)
			for _, n := range []*graph.Node{module, param, noTerms, retained} {
				legacy.AddNode(n)
				require.NoError(t, legacy.UpsertSymbolFTS(n.ID, wantTokens[n.ID]))
			}
			require.NoError(t, legacy.Publish(ctx))
			oracle, err := legacy.OpenSnapshot()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, oracle.Close()) })
			saved := rebuildSearchRows(t, current, []byte{familySymbol})
			require.NotEmpty(t, saved)
			require.Equal(t, rebuildSearchRows(t, oracle, []byte{familySymbol}), saved)
			stats, err := point(current, searchKey("stats"), searchReadStats)
			require.NoError(t, err)
			require.Equal(t, searchStats{Rows: 4, Tokens: 4, Count: 4, Repos: []string{"", repo}}, stats)
			docs := make(map[string]bool)
			for _, r := range rebuildSearchRows(t, current, searchKey("doc")) {
				d, err := searchReadDoc(r.value)
				require.NoError(t, err)
				docs[d.ID] = d.NodePresent
			}
			require.Equal(t, map[string]bool{retained.ID: true, noTerms.ID: true, param.ID: profile == FullProfile, module.ID: true}, docs)
		})
	}
}

func TestRebuildSearchStageCorruptNodeDoesNotChangeHead(t *testing.T) {
	worker, head, base, declaration, _, _, _ := relationsStageFixture(t, FullProfile)
	derived := publishRebuildSearchDerived(t, worker, head, base)
	prior := relationsSavedRows(t, stageRead(t, head))
	require.NotEmpty(t, prior)
	require.NoError(t, worker.pending.Set(pendingNodeKey(declaration.ID), []byte{0xff}, pebble.NoSync))
	dir := t.TempDir()
	stage, err := worker.PrepareSearch(t.Context(), dir, derived.Witness, derived.Incarnation, func(*graph.Node) (string, bool) {
		return "not published", true
	})
	require.ErrorIs(t, err, ErrSearchSeal)
	require.ErrorIs(t, err, ErrRebuildView)
	require.ErrorIs(t, err, errInvalidRecord)
	require.Empty(t, stage)
	require.Empty(t, rebuildSearchRows(t, stageRead(t, head), []byte{familySymbol}))
	require.Equal(t, prior, relationsSavedRows(t, stageRead(t, head)))
}

func publishRebuildSearchDerived(t *testing.T, worker *BuildStore, head *Head, base FileGraphBase) StageTables {
	t.Helper()
	ctx := t.Context()
	relations, err := worker.PrepareRelations(ctx, t.TempDir(), base, nil)
	require.NoError(t, err)
	digest, err := head.PublishStage(ctx, relations)
	require.NoError(t, err)
	derived, err := PrepareDeferredDerived(t.TempDir(), relations)
	require.NoError(t, err)
	require.Equal(t, digest, derived.Base)
	_, err = head.PublishStage(ctx, derived)
	require.NoError(t, err)
	return derived
}

func rebuildSearchRows(t *testing.T, s *Snapshot, prefix []byte) []row {
	t.Helper()
	var rows []row
	require.NoError(t, scan(t.Context(), s, prefix, func(key, value []byte) (bool, error) {
		rows = append(rows, row{bytes.Clone(key), bytes.Clone(value)})
		return true, nil
	}))
	return rows
}

package store_pebble

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
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
			decls := []*graph.Node{retained, noTerms}
			wantTokens := map[string]string{retained.ID: "ctx path ctx", noTerms.ID: "..."}
			for i, kind := range []graph.NodeKind{graph.KindLocal, graph.KindParam, graph.KindGenericParam, graph.KindClosure} {
				n := &graph.Node{ID: path + "::" + string(kind), Name: "ctx", Kind: kind, RepoPrefix: repo, FilePath: path, Language: language}
				decls = append(decls, n)
				wantTokens[n.ID] = strings.TrimSpace(strings.Repeat("ctx ", i+4))
			}
			require.NoError(t, worker.AddParsedFile(repo, ParseFacts{
				Nodes: decls, File: graph.FileMetaRow{FilePath: path, NodeCount: len(decls)},
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
			wantTokens[module.ID] = "lib"
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
				{query: "path path", id: retained.ID}, // Two terms bypass exact lookup.
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

			// Select the SQLite corpus before inserting any FTS document. Use
			// fixture membership, not the Pebble profile predicate, for the oracle.
			projected := append([]*graph.Node{module}, decls[:2]...)
			wantStats := searchStats{Rows: 3, Tokens: 4, Count: 3, Repos: []string{"", repo}}
			if profile == FullProfile {
				projected = append(projected, decls[2:]...)
				wantStats.Rows, wantStats.Tokens, wantStats.Count = 7, 26, 7
			}
			native, err := store_sqlite.Open(filepath.Join(t.TempDir(), "oracle.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, native.Close()) })
			legacy := newBuild(t, profile)
			for _, n := range projected {
				native.AddNode(n)
				require.NoError(t, native.UpsertSymbolFTS(n.ID, wantTokens[n.ID]))
				legacy.AddNode(n)
				require.NoError(t, legacy.UpsertSymbolFTS(n.ID, wantTokens[n.ID]))
			}
			require.NoError(t, legacy.Publish(ctx))
			oracle := snapshot(t, legacy)
			saved := rebuildSearchRows(t, current, []byte{familySymbol})
			require.NotEmpty(t, saved)
			require.Equal(t, rebuildSearchRows(t, oracle, []byte{familySymbol}), saved)
			stats, err := point(current, searchKey("stats"), searchReadStats)
			require.NoError(t, err)
			require.Equal(t, wantStats, stats)
			for _, query := range []string{"ctx ctx", "path path"} {
				want, err := native.SearchSymbols(query, 10)
				require.NoError(t, err)
				if profile == FullProfile && query == "ctx ctx" {
					require.Len(t, want, 5)
					require.NotEqual(t, retained.ID, want[0].NodeID)
				} else {
					require.Len(t, want, 1)
					require.Equal(t, retained.ID, want[0].NodeID)
				}
				got, err := current.SearchSymbols(query, 10)
				require.NoError(t, err)
				require.Equal(t, want, got)
			}
			docs := make(map[string]bool)
			for _, r := range rebuildSearchRows(t, current, searchKey("doc")) {
				d, err := searchReadDoc(r.value)
				require.NoError(t, err)
				docs[d.ID] = d.NodePresent
			}
			wantDocs := make(map[string]bool)
			for _, n := range projected {
				wantDocs[n.ID] = true
			}
			require.Equal(t, wantDocs, docs)
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

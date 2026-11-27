package indexer

import (
	"iter"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildCloneInputsReplay(t *testing.T) {
	// An identical seven-hash set does not imply an identical raw token count.
	shingles := []uint64{1, 3, 5, 7, 11, 13, 1<<63 + 1}
	want := []rebuildCloneInput{
		{ID: "a.go::F", FilePath: "a.go", Language: "go", Line: 3, Tokens: 79, Shingles: shingles},
		{ID: "b.tsx::F", FilePath: "b.tsx", Language: "typescript", Line: 7, Tokens: 80, Shingles: shingles},
		{ID: "c.rs::F", FilePath: "c.rs", Language: "rust", Line: 11, Tokens: 97, Shingles: []uint64{2, 4, 6, 8, 10, 12, 14, 16}},
	}
	goFacts := rebuildCloneInputFixture(want[0], "go")
	// Give the synthetic TSX node a different label from its effective extractor.
	tsFacts := rebuildCloneInputFixture(want[1], "tsx")
	rustFacts := rebuildCloneInputFixture(want[2], "rust")
	build, _ := openRebuildTestFileStore(t)
	receipts := seedRebuildCloneInputReceipts(t, build, goFacts, tsFacts, rustFacts)
	for range 2 {
		var got []rebuildCloneInput
		require.NoError(t, visitRebuildCloneInputs(t.Context(), build.RebuildView(), receipts, func(row rebuildCloneInput) error {
			row.Shingles = slices.Clone(row.Shingles)
			got = append(got, row)
			return nil
		}))
		require.ElementsMatch(t, want, got, "raw input includes deferred extractors before projection")
	}
}

func TestRebuildCloneInputsAcceptsCompleteEmpty(t *testing.T) {
	for _, name := range []string{"disabled", "body too short"} {
		t.Run(name, func(t *testing.T) {
			const path = "empty.go"
			node := &graph.Node{ID: path + "::F", Kind: graph.KindFunction, Name: "F", FilePath: path, Language: "go", StartLine: 1, EndLine: 1}
			if name == "body too short" {
				node.Meta = map[string]any{cloneBodyMetaKey: cloneBodyTextIdentity("func F() {}")}
			}
			build, _ := openRebuildTestFileStore(t)
			receipts := seedRebuildCloneInputReceipts(t, build, store_pebble.ParseFacts{
				EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: path, NodeCount: 1}, Nodes: []*graph.Node{node},
			})
			foreign := rebuildCloneInputFixture(rebuildCloneInput{
				ID: "foreign.go::F", FilePath: "foreign.go", Language: "go", Line: 1, Tokens: 79, Shingles: []uint64{5, 7, 9, 11, 13, 15, 17},
			}, "go")
			foreign.Nodes[0].RepoPrefix = "foreign"
			require.NoError(t, build.AddParsedFile("foreign", foreign))
			require.NoError(t, visitRebuildCloneInputs(t.Context(), build.RebuildView(), receipts, func(row rebuildCloneInput) error {
				t.Fatalf("complete empty local input yielded %+v", row)
				return nil
			}))
		})
	}
}

func TestRebuildCloneInputsRejectsUnprovedWorkingRows(t *testing.T) {
	for _, name := range []string{"changed node", "changed shingles", "missing shingles", "orphan shingles"} {
		t.Run(name, func(t *testing.T) {
			const id = "a.go::F"
			facts := rebuildCloneInputFixture(rebuildCloneInput{
				ID: id, FilePath: "a.go", Language: "go", Line: 3, Tokens: 79, Shingles: []uint64{5, 7, 9, 11, 13, 15, 17},
			}, "go")
			build, _ := openRebuildTestFileStore(t)
			receipts := seedRebuildCloneInputReceipts(t, build, facts)
			view := build.RebuildView()
			require.NoError(t, visitRebuildCloneInputs(t.Context(), view, receipts, func(rebuildCloneInput) error { return nil }))
			switch name {
			case "changed node":
				changed := *facts.Nodes[0]
				changed.StartLine++
				build.AddNode(&changed)
				saved, err := view.Node(t.Context(), id)
				require.NoError(t, err)
				require.NotNil(t, saved)
				require.Equal(t, changed.StartLine, saved.StartLine)
			case "changed shingles":
				require.NoError(t, build.BulkSetCloneShingles("", map[string][]uint64{id: {5, 8, 9, 11, 13, 15, 17}}))
			case "missing shingles":
				require.NoError(t, build.DeleteCloneShingles([]string{id}))
			case "orphan shingles":
				// This valid working clone row has no corresponding checked receipt.
				orphan := rebuildCloneInputFixture(rebuildCloneInput{
					ID: "z.go::F", FilePath: "z.go", Language: "go", Line: 7, Tokens: 80, Shingles: []uint64{5, 7, 9, 11, 13, 15, 17},
				}, "go")
				require.NoError(t, build.AddParsedFile("", orphan))
			}
			// A valid row can be yielded before a later failure; nil alone completes the replay.
			err := visitRebuildCloneInputs(t.Context(), view, receipts, func(rebuildCloneInput) error { return nil })
			require.Error(t, err)
		})
	}
}

func rebuildCloneInputFixture(row rebuildCloneInput, nodeLanguage string) store_pebble.ParseFacts {
	shingles := slices.Clone(row.Shingles)
	return store_pebble.ParseFacts{
		EffectiveLanguage: row.Language,
		File:              graph.FileMetaRow{FilePath: row.FilePath, NodeCount: 1},
		Nodes: []*graph.Node{{
			ID: row.ID, FilePath: row.FilePath, Language: nodeLanguage, Kind: graph.KindFunction, Name: "F",
			StartLine: row.Line, EndLine: row.Line + 2,
			Meta: map[string]any{cloneTokensMetaKey: row.Tokens, cloneShinglesMetaKey: shingles, cloneBodyMetaKey: cloneBodyIdentity(shingles)},
		}},
		Shingles: map[string][]uint64{row.ID: shingles},
	}
}

func seedRebuildCloneInputReceipts(t *testing.T, build *store_pebble.BuildStore, facts ...store_pebble.ParseFacts) iter.Seq2[store_pebble.ParseFacts, error] {
	t.Helper()
	idx := &Indexer{}
	idx.installRebuildParsePolicy(rebuildParsePolicy{})
	store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	var job rebuildFileStages
	census := &rebuildExtractorCensus{byPath: make(map[string]string, len(facts))}
	for _, f := range facts {
		input := rebuildInput{Path: f.File.FilePath, Language: f.EffectiveLanguage}
		recipe := idx.rebuildExtractorRecipe(job.recipes[input.Language], input)
		raw := encodeRebuildTestParseFacts(t, f)
		require.NoError(t, store.write(input.parseKey(recipe), raw))
		checked := decodeRebuildTestParseFacts(t, raw)
		require.NoError(t, build.AddParsedFile("", checked))
		job.inputs = append(job.inputs, input)
		census.byPath[input.Path] = f.EffectiveLanguage
	}
	return idx.rebuildDataflowReceipts(t.Context(), job, store, census)
}

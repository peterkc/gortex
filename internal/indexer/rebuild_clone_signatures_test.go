package indexer

import (
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/clones"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildCloneSignaturesMatchNative(t *testing.T) {
	// Only the deferred Rust body shares a hash with either Go body. At the
	// corpus floor the Go bodies have seven and eight surviving hashes.
	inputs := []rebuildCloneInput{
		{ID: "a.go::F", FilePath: "a.go", Language: "go", Line: 3, Tokens: 79, Shingles: []uint64{1, 2, 3, 4, 5, 6, 7, 8}},
		{ID: "b.go::F", FilePath: "b.go", Language: "go", Line: 7, Tokens: 80, Shingles: []uint64{21, 22, 23, 24, 25, 26, 27, 28, 29}},
		{ID: "c.rs::F", FilePath: "c.rs", Language: "rust", Line: 11, Tokens: 97, Shingles: []uint64{1, 21, 101, 102, 103, 104, 105}},
	}
	var facts []store_pebble.ParseFacts
	for _, input := range inputs {
		facts = append(facts, rebuildCloneInputFixture(input, input.Language))
	}
	build, _ := openRebuildTestFileStore(t)
	receipts := seedRebuildCloneInputReceipts(t, build, facts...)
	prev := cmsMinCorpus
	t.Cleanup(func() { cmsMinCorpus = prev })

	for _, tc := range []struct {
		name  string
		floor int
		kept  []string
	}{
		{"below_floor", 4, []string{"a.go::F", "b.go::F", "c.rs::F"}},
		{"at_floor", 3, []string{"b.go::F"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmsMinCorpus = tc.floor
			oracle := graph.New()
			for f, err := range receipts {
				require.NoError(t, err)
				for _, n := range f.Nodes {
					// Parse facts carry the raw set in the sidecar. The native
					// node finalizer consumes it from Meta and stamps Meta in place.
					clone := *n
					clone.Meta = maps.Clone(n.Meta)
					clone.Meta[cloneShinglesMetaKey] = slices.Clone(f.Shingles[n.ID])
					oracle.AddNode(&clone)
				}
			}
			items, corpus := finaliseCloneSignaturesFromNodes(oracle, "")
			require.Equal(t, len(inputs), corpus)
			require.Len(t, items, len(tc.kept))
			want, err := oracle.CloneCorpusPage("", "", corpus+1)
			require.NoError(t, err)
			require.Len(t, want, len(inputs))

			var got []graph.CloneCorpusRow
			var gotInputs []rebuildCloneInput
			var keptIDs []string
			counts, err := visitRebuildCloneSignatures(t.Context(), build.RebuildView(), receipts, rebuildClonePredecessors{}, func(row rebuildCloneInput, sig clones.Signature, kept bool) error {
				stamp := ""
				if kept {
					stamp = clones.EncodeSignature(sig)
					keptIDs = append(keptIDs, row.ID)
				}
				row.Shingles = slices.Clone(row.Shingles)
				gotInputs = append(gotInputs, row)
				got = append(got, graph.CloneCorpusRow{
					NodeID: row.ID, TokenCount: row.Tokens, Shingles: row.Shingles, Signature: stamp, Finalized: true,
				})
				return nil
			})
			require.NoError(t, err)
			require.Equal(t, rebuildCloneSignatureCounts{Corpus: len(inputs), Kept: len(tc.kept)}, counts)
			require.ElementsMatch(t, tc.kept, keptIDs)
			require.ElementsMatch(t, inputs, gotInputs, "every input, including each drop, is finalized once")
			require.ElementsMatch(t, want, got, "native corpus rows include finalized empty stamps")
		})
	}
}

func TestRebuildCloneSignaturesAcceptsCompleteEmpty(t *testing.T) {
	build, _ := openRebuildTestFileStore(t)
	receipts := seedRebuildCloneInputReceipts(t, build, store_pebble.ParseFacts{
		EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: "empty.go"},
	})
	calls := 0
	counts, err := visitRebuildCloneSignatures(t.Context(), build.RebuildView(), receipts, rebuildClonePredecessors{}, func(rebuildCloneInput, clones.Signature, bool) error {
		calls++
		return nil
	})
	require.NoError(t, err)
	require.Zero(t, counts)
	require.Zero(t, calls)
}

func TestRebuildCloneSignaturesRejectsIncompletePass(t *testing.T) {
	build, _ := openRebuildTestFileStore(t)
	const secondPath = "b.go"
	shingles := []uint64{1, 3, 5, 7, 11, 13, 17, 19}
	a := rebuildCloneInputFixture(rebuildCloneInput{ID: "a.go::F", FilePath: "a.go", Language: "go", Line: 3, Tokens: 79, Shingles: shingles}, "go")
	b := rebuildCloneInputFixture(rebuildCloneInput{ID: "b.go::F", FilePath: secondPath, Language: "go", Line: 7, Tokens: 80, Shingles: shingles}, "go")
	receipts := seedRebuildCloneInputReceipts(t, build, a, b)
	receiptErr := errors.New("receipt read failed")
	callbackErr := errors.New("signature sink failed")
	for _, tc := range []struct {
		name     string
		failPass int
		wantErr  error
		calls    int
	}{
		{"receipt_first_pass", 1, receiptErr, 0},
		{"receipt_second_pass", 2, receiptErr, 1},
		{"working_span_first_pass", 1, nil, 0},
		{"working_span_second_pass", 2, nil, 1},
		{"callback", 0, callbackErr, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pass, calls := 0, 0
			faulty := func(yield func(store_pebble.ParseFacts, error) bool) {
				pass++
				for f, err := range receipts {
					if err == nil && pass == tc.failPass && f.File.FilePath == secondPath {
						if tc.wantErr == receiptErr {
							yield(store_pebble.ParseFacts{}, receiptErr)
							return
						}
						// Mismatch the exact saved working span without mutating the view.
						n := *f.Nodes[0]
						n.EndLine++
						f.Nodes = []*graph.Node{&n}
					}
					if !yield(f, err) {
						return
					}
				}
			}
			counts, err := visitRebuildCloneSignatures(t.Context(), build.RebuildView(), faulty, rebuildClonePredecessors{}, func(rebuildCloneInput, clones.Signature, bool) error {
				calls++
				if tc.wantErr == callbackErr {
					return callbackErr
				}
				return nil
			})
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			} else {
				require.ErrorContains(t, err, "clone input node differs from receipt")
			}
			require.Zero(t, counts, "callbacks before a later failure are tentative")
			require.Equal(t, tc.calls, calls)
		})
	}
}

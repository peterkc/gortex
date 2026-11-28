package indexer

import (
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/clones"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildCloneSignaturesUseFileGraphClaimProof(t *testing.T) {
	a := rebuildCloneInputFixture(rebuildCloneInput{
		ID: "a.go::F", FilePath: "a.go", Language: "go", Line: 3, Tokens: 80,
		Shingles: []uint64{1, 2, 3, 4, 5, 6, 7, 8},
	}, "go")
	shared := &graph.Node{ID: "string::error_msg::shared", Kind: graph.KindString, FilePath: "a.go"}
	other := *shared
	other.FilePath = "b.go"
	a.Nodes = append(a.Nodes, shared)
	a.File.NodeCount++
	b := store_pebble.ParseFacts{
		File:              graph.FileMetaRow{FilePath: "b.go", NodeCount: 1},
		EffectiveLanguage: "go", Nodes: []*graph.Node{&other},
	}
	build, _ := openRebuildTestFileStore(t)
	receipts := seedRebuildCloneInputReceipts(t, build, a, b)
	proof, err := build.ReduceRebuildSharedClaims(t.Context(), rebuildCloneClaimBytes, receipts, nil)
	require.NoError(t, err)
	require.EqualValues(t, 1, proof.AcceptedStrings)
	var ids []string
	yield := func(row rebuildCloneInput, _ clones.Signature, _ bool) error {
		ids = append(ids, row.ID)
		return nil
	}
	_, err = visitRebuildCloneSignatures(t.Context(), build, receipts, rebuildClonePredecessors{}, yield)
	var refusal *store_pebble.RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &refusal)
	require.Empty(t, ids)
	counts, err := visitRebuildCloneSignatures(t.Context(), build, receipts, rebuildClonePredecessors{fileClaims: &proof}, yield)
	require.NoError(t, err)
	require.Equal(t, proof.RebuildShadowedIDStats, counts.Claims)
	require.Equal(t, []string{"a.go::F"}, ids)
}

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
			counts, err := visitRebuildCloneSignatures(t.Context(), build, receipts, rebuildClonePredecessors{}, func(row rebuildCloneInput, sig clones.Signature, kept bool) error {
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
			require.Equal(t, rebuildCloneSignatureCounts{Corpus: len(inputs), Kept: len(tc.kept), Claims: store_pebble.RebuildShadowedIDStats{ScratchBytes: 57, PeakRowBytes: 19}}, counts)
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
	counts, err := visitRebuildCloneSignatures(t.Context(), build, receipts, rebuildClonePredecessors{}, func(rebuildCloneInput, clones.Signature, bool) error {
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
			counts, err := visitRebuildCloneSignatures(t.Context(), build, faulty, rebuildClonePredecessors{}, func(rebuildCloneInput, clones.Signature, bool) error {
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

func TestRebuildCloneIsolationCohortCoverage(t *testing.T) {
	const goPath, tsPath, emptyPath = "a.go", "b.ts", "empty.go"
	shingles := []uint64{1, 2, 3, 4, 5, 6, 7, 8}
	signed, ok := clones.SignatureFromShingles(shingles, 0)
	require.True(t, ok)
	previous := cmsMinCorpus
	cmsMinCorpus = 3
	t.Cleanup(func() { cmsMinCorpus = previous })
	for _, mixed := range []bool{false, true} {
		name := "supported_only"
		if mixed {
			name = "held_raw_and_signed"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			a := rebuildCloneInputFixture(rebuildCloneInput{ID: goPath + "::Raw", FilePath: goPath, Language: "go", Line: 3, Tokens: 80, Shingles: shingles}, "go")
			a.Nodes = append(a.Nodes, &graph.Node{ID: goPath + "::Signed", Name: "Signed", Kind: graph.KindMethod, FilePath: goPath, Language: "go", StartLine: 7, EndLine: 10,
				Meta: map[string]any{cloneSigMetaKey: clones.EncodeSignature(signed), cloneTokensMetaKey: 80}})
			a.File.NodeCount++
			b := rebuildCloneInputFixture(rebuildCloneInput{ID: tsPath + "::Raw", FilePath: tsPath, Language: "typescript", Line: 3, Tokens: 80, Shingles: shingles}, "javascript")
			empty := store_pebble.ParseFacts{EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: emptyPath}}
			build, _ := openRebuildTestFileStore(t)
			receipts := seedRebuildCloneInputReceipts(t, build, a, b, empty)
			manifestFiles := []store_pebble.ManifestFile{{Path: goPath, Kind: 'f', Language: "go"}, {Path: tsPath, Kind: 'f', Language: "typescript"}, {Path: emptyPath, Kind: 'f', Language: "go"}}
			var deferred []string
			var held map[string]uint64
			if mixed {
				raw := rebuildCloneInputFixture(rebuildCloneInput{ID: "held.py::Raw", FilePath: "held.py", Language: "python", Line: 3, Tokens: 80, Shingles: shingles}, "python")
				preSigned := rebuildSignedCloneFacts(clones.EncodeSignature(signed))
				noBody := store_pebble.ParseFacts{EffectiveLanguage: "ruby", File: graph.FileMetaRow{FilePath: "held.rb"}}
				for _, f := range []store_pebble.ParseFacts{raw, preSigned, noBody} {
					require.NoError(t, build.AddRebuildDeferredFile("", f, func(string) (bool, error) { return true, nil }))
					manifestFiles = append(manifestFiles, store_pebble.ManifestFile{Path: f.File.FilePath, Kind: 'f', Language: f.EffectiveLanguage})
				}
				deferred = []string{"python", "ruby", "rust"}
				// FileGraph owns this exact inventory; only counts, not bodies,
				// enter the link-facing clone pass. Ruby has no clone inputs.
				held = map[string]uint64{"python": 1, "rust": 2, "ruby": 0}
			}
			head, err := store_pebble.OpenHead(filepath.Join(t.TempDir(), "head"), rebuildTestIncarnation, store_pebble.FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, head.Close()) })
			manifest, err := store_pebble.PrepareManifest(ctx, t.TempDir(), manifestFiles, store_pebble.Witness{
				Input: store_pebble.HeadDigest{1}, Recipe: store_pebble.HeadDigest{2}, Stage: store_pebble.StageManifest, Profile: store_pebble.FullProfile,
			}, rebuildTestIncarnation)
			require.NoError(t, err)
			_, err = head.PublishStage(ctx, manifest)
			require.NoError(t, err)
			files, err := build.PrepareFileGraph(ctx, t.TempDir(), manifest.Witness, rebuildTestIncarnation)
			require.NoError(t, err)
			base, err := build.RetainFileGraph(ctx, files)
			require.NoError(t, err)
			_, err = head.PublishStage(ctx, files)
			require.NoError(t, err)
			require.NoError(t, build.IsolateRebuildLinks(ctx))
			predecessors := rebuildClonePredecessors{fileClaims: &store_pebble.RebuildSharedClaimStats{}, heldCloneByLanguage: held}
			stats, err := applyRebuildCloneRelations(ctx, build, receipts, predecessors, 0.82)
			require.NoError(t, err)
			require.Equal(t, 2, stats.Signatures.Corpus)
			require.Equal(t, 2, stats.Signatures.Kept, "held raw bodies must not cross the CMS floor")
			require.Equal(t, 1, stats.Signatures.Signed, "held signatures must not enter the selected pair corpus")
			require.NoError(t, build.RestoreRebuildDeclarations(ctx))
			require.NoError(t, build.CheckRebuildIsolationCoverage(deferred))
			relations, err := build.PrepareRelations(ctx, t.TempDir(), base, nil)
			require.NoError(t, err)
			_, err = head.PublishStage(ctx, relations)
			require.NoError(t, err)
			snap := head.OpenSnapshot()
			defer func() { require.NoError(t, snap.Close()) }()
			require.NoError(t, snap.RequireRelationCoverageContext(ctx, []string{"go"}, []string{emptyPath}))
			require.NoError(t, snap.RequireRelationCoverageContext(ctx, []string{"javascript"}, nil), "use effective extractor, not node language")
			if !mixed {
				require.NoError(t, snap.RequireRelationCoverageContext(ctx, nil, nil))
				return
			}
			var refusal *store_pebble.ErrLinksNotAvailable
			require.ErrorAs(t, snap.RequireRelationCoverageContext(ctx, []string{"go"}, []string{goPath}), &refusal)
			require.Equal(t, []string{"python", "rust"}, refusal.Languages)
			require.Equal(t, []store_pebble.RebuildIsolationAffectedScope{
				{FilePath: goPath, SupportedLanguage: "go", DeferredLanguage: "python", Cause: "clone-corpus", Count: 2},
				{FilePath: goPath, SupportedLanguage: "go", DeferredLanguage: "rust", Cause: "clone-corpus", Count: 2},
			}, refusal.Affected)
			require.ErrorAs(t, snap.RequireRelationCoverageContext(ctx, []string{"typescript"}, []string{tsPath}), &refusal)
			require.Equal(t, []store_pebble.RebuildIsolationAffectedScope{
				{FilePath: tsPath, SupportedLanguage: "typescript", DeferredLanguage: "python", Cause: "clone-corpus", Count: 1},
				{FilePath: tsPath, SupportedLanguage: "typescript", DeferredLanguage: "rust", Cause: "clone-corpus", Count: 1},
			}, refusal.Affected)
		})
	}
}

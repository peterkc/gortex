package indexer

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildCloneShadowedReceiptIDs(t *testing.T) {
	for _, tc := range []struct {
		name, id               string
		third, drift, sameFile bool
	}{
		{name: "plugin_and_go", id: "z.go::F"},
		{name: "three_claimants_with_predecessor_overlap", id: "grpc::Inventory", third: true},
		{name: "working_mismatch_before_last_claim", id: "z.go::F", third: true, drift: true},
		{name: "same_receipt_repeats_nonclone", id: "z.go::F", sameFile: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shingles := []uint64{1, 3, 5, 7, 11, 13, 17, 19}
			f := rebuildCloneInputFixture(rebuildCloneInput{ID: tc.id, FilePath: "z.go", Language: "go", Line: 3, Tokens: 80, Shingles: shingles}, "go")
			g := rebuildCloneInputFixture(rebuildCloneInput{ID: "z.go::G", FilePath: "z.go", Language: "go", Line: 7, Tokens: 80, Shingles: shingles}, "go")
			f.Nodes = append(f.Nodes, g.Nodes[0])
			f.File.NodeCount++
			f.Shingles[g.Nodes[0].ID] = g.Shingles[g.Nodes[0].ID]
			// A plugin may claim the exact Go ID with the same stored file
			// path but a different kind, with no clone metadata at all.
			plugin := &graph.Node{ID: tc.id, FilePath: "z.go", Kind: graph.KindVariable, Name: "plugin", Language: "zzt146"}
			if tc.sameFile {
				plugin.ID = "plugin-only"
			}
			a := rebuildCloneClaimFixture("a.zzt146", plugin)
			late := rebuildCloneClaimFixture("zz.zzt146", plugin)
			wantShadowed := uint64(1)
			if tc.drift {
				// A second shadow is found only after the live F check fails.
				other := &graph.Node{ID: "only-nonclone", Name: "non-clone", FilePath: "z.go", Language: "zzt146", Kind: graph.KindVariable}
				a.Nodes = append(a.Nodes, other)
				late.Nodes = append(late.Nodes, other)
				a.File.NodeCount++
				late.File.NodeCount++
				wantShadowed++
			}
			if tc.sameFile {
				a.Nodes = append(a.Nodes, plugin)
				a.File.NodeCount++
			}
			inputs := []store_pebble.ParseFacts{a, f}
			if tc.third {
				// A later identical plugin claim counts no more than the first
				// two claims, even when the clone view now differs from Go.
				inputs = append(inputs, late)
			}
			build, _ := openRebuildTestFileStore(t)
			receipts := seedRebuildCloneInputReceipts(t, build, inputs...)
			// The last plugin would otherwise replace F. For the no-drift
			// overlap case, keep F live so the predecessor also sees it.
			if tc.third && !tc.drift {
				build.AddNode(f.Nodes[0])
			}
			passes, read := 0, 0
			replay := func(yield func(store_pebble.ParseFacts, error) bool) {
				passes++
				for facts, err := range receipts {
					read++
					if !yield(facts, err) {
						return
					}
				}
			}
			if tc.sameFile {
				stats, err := applyRebuildCloneRelations(t.Context(), build, replay, rebuildClonePredecessors{}, 0.82)
				require.NoError(t, err)
				require.Equal(t, 2, passes)
				require.Equal(t, 2, stats.Signatures.Kept)
				require.EqualValues(t, 1, stats.Direct.Pairs)
				wantBytes := len(plugin.ID) + len(tc.id) + len("z.go::G") + 3*12
				require.EqualValues(t, wantBytes, stats.Signatures.Claims.ScratchBytes)
				require.EqualValues(t, len(plugin.ID)+12, stats.Signatures.Claims.PeakRowBytes)
				require.Zero(t, stats.Signatures.Claims.Shadowed)
				return
			}
			before := f.Nodes
			if tc.drift {
				before = []*graph.Node{plugin, a.Nodes[1], g.Nodes[0]}
			}
			stats, err := applyRebuildCloneRelations(t.Context(), build, replay, rebuildClonePredecessors{}, 0.82)
			require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
			reply := buildRefusal(err)
			require.Equal(t, uint8(36), reply.Refusal)
			require.Equal(t, wantShadowed, reply.UnknownProvenance)
			require.Contains(t, reply.Detail, "shadowed_id=")
			require.NotContains(t, reply.Detail, "contract=")
			require.Zero(t, stats)
			require.Equal(t, 1, passes)
			require.Equal(t, len(inputs), read)
			requireRebuildTestFileRows(t, build, before, nil)
			got := make(map[string][]uint64)
			require.NoError(t, build.RebuildView().VisitCloneShingles(t.Context(), "", func(id string, values []uint64) error {
				got[id] = values
				return nil
			}))
			require.Equal(t, f.Shingles, got)
		})
	}
}

func TestRebuildCloneShadowedReceiptCensusMustComplete(t *testing.T) {
	const id = "shared"
	n := &graph.Node{ID: id, FilePath: "z.go", Name: "same row", Language: "go", Kind: graph.KindVariable}
	extra := &graph.Node{ID: "extra", FilePath: "c.go", Name: "later claim", Language: "go", Kind: graph.KindVariable}
	build, _ := openRebuildTestFileStore(t)
	a := rebuildCloneClaimFixture("a.go", n)
	b := rebuildCloneClaimFixture("b.go", n)
	c := rebuildCloneClaimFixture("c.go", extra)
	receipts := seedRebuildCloneInputReceipts(t, build, a, b, c)
	broken := errors.New("later receipt failed to decode")
	for _, tc := range []struct {
		name string
		want error
	}{
		{name: "receipt", want: broken},
		{name: "order", want: errParseEntryCorrupt},
		{name: "canceled", want: context.Canceled},
		{name: "capacity", want: store_pebble.ErrRebuildLinkInputSize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			limit := uint64(rebuildCloneClaimBytes)
			if tc.name == "capacity" {
				// Allow the first claim and its shadow marker, not the third row.
				limit = 2 * (len(id) + 12)
			}
			replay := func(yield func(store_pebble.ParseFacts, error) bool) {
				for facts, err := range receipts {
					if facts.File.FilePath == c.File.FilePath {
						switch tc.name {
						case "receipt":
							err = broken
						case "order":
							facts.File.FilePath = b.File.FilePath
						case "canceled":
							cancel()
						}
					}
					if !yield(facts, err) {
						return
					}
				}
			}
			yield := func(rebuildCloneInput) error { return errors.New("unexpected clone input") }
			stats, err := censusRebuildCloneInputs(ctx, build, replay, limit, yield)
			require.ErrorIs(t, err, tc.want)
			require.NotErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
			require.Zero(t, stats)
			require.Zero(t, buildRefusal(err).UnknownProvenance)
			stats, err = censusRebuildCloneInputs(t.Context(), build, receipts, rebuildCloneClaimBytes, yield)
			require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
			require.EqualValues(t, 1, buildRefusal(err).UnknownProvenance)
			require.Zero(t, stats)
			requireRebuildTestFileRows(t, build, []*graph.Node{n, extra}, nil)
		})
	}
}

func rebuildCloneClaimFixture(path string, n *graph.Node) store_pebble.ParseFacts {
	return store_pebble.ParseFacts{
		EffectiveLanguage: n.Language, File: graph.FileMetaRow{FilePath: path, NodeCount: 1}, Nodes: []*graph.Node{n},
	}
}

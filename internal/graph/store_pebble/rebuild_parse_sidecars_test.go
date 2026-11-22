package store_pebble

import (
	"context"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func parsedSidecarFixture(t *testing.T) ParseFacts {
	t.Helper()
	const path = "nested/a.go"
	const id = path + "::F"
	const empty = path + "::Empty"
	facts := ParseFacts{
		Nodes: []*graph.Node{
			{ID: id, Name: "F", Kind: graph.KindFunction, FilePath: path, Language: "go"},
			{ID: empty, Name: "Empty", Kind: graph.KindConstant, FilePath: path, Language: "go"},
		},
		File: graph.FileMetaRow{FilePath: path, NodeCount: 2, Size: 64, ContentHash: "digest"},
		Constants: []graph.ConstantValueRow{
			{NodeID: id, FilePath: path, Value: "first"},
			{NodeID: empty, FilePath: path, Value: ""},
			{NodeID: id, FilePath: path, Value: "last\x00literal"},
		},
		Shingles: map[string][]uint64{id: {1<<63 + 1, 7, 1<<63 + 1}},
		Symbols:  []graph.SymbolFTSItem{{NodeID: id, Tokens: "pre resolution"}},
	}
	payload, err := EncodeParseFacts(facts)
	require.NoError(t, err)
	saved, err := DecodeParseFacts(payload)
	require.NoError(t, err)
	require.Equal(t, facts.Constants, saved.Constants, "the per-file receipt retains extraction order")
	require.Equal(t, facts.Shingles, saved.Shingles)
	require.Equal(t, facts.Symbols, saved.Symbols)
	return saved
}

func TestRebuildParseSidecarsSaved(t *testing.T) {
	b := newBuild(t, FullProfile)
	ctx := t.Context()
	f := parsedSidecarFixture(t)
	id := f.Nodes[0].ID
	require.NoError(t, b.AddParsedFile("repo", f))
	v := b.RebuildView()
	row, err := v.ConstantValue(ctx, id)
	require.NoError(t, err)
	require.Equal(t, &graph.ScopedConstantValueRow{ConstantValueRow: f.Constants[2], RepoPrefix: "repo"}, row)
	empty, err := v.ConstantValue(ctx, f.Nodes[1].ID)
	require.NoError(t, err)
	require.Equal(t, &graph.ScopedConstantValueRow{ConstantValueRow: f.Constants[1], RepoPrefix: "repo"}, empty)
	missing, err := v.ConstantValue(ctx, f.File.FilePath+"::Missing")
	require.NoError(t, err)
	require.Nil(t, missing)

	var visits int
	collect := func(gotID string, got []uint64) error {
		visits++
		require.Equal(t, id, gotID)
		require.Equal(t, f.Shingles[id], got)
		return nil
	}
	require.NoError(t, v.VisitCloneShingles(ctx, "repo", collect))
	require.Equal(t, 1, visits)
	require.NoError(t, v.VisitCloneShingles(ctx, "", collect))
	require.Equal(t, 1, visits, "an empty repo prefix is not all repos")
	require.NoError(t, b.DeleteCloneShingles([]string{id}))
	require.NoError(t, v.VisitCloneShingles(ctx, "repo", collect))
	require.Equal(t, 1, visits, "native deletion removes the saved row")
	raw, err := pendingGet(b.pending, pendingSymbolKey(id), copyValue)
	require.NoError(t, err)
	require.Nil(t, raw, "parse tokens are not a working search document")
}

func TestRebuildParseSidecarsRefused(t *testing.T) {
	for _, name := range []string{"malformed", "repeated"} {
		t.Run(name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			ctx := t.Context()
			f := parsedSidecarFixture(t)
			id := f.Nodes[0].ID
			require.NoError(t, b.AddParsedFile("repo", f))
			before, err := pendingGet(b.pending, parsedConstantKey(id), copyValue)
			require.NoError(t, err)
			require.NotEmpty(t, before)

			bad := f
			bad.Constants = []graph.ConstantValueRow{{NodeID: id, FilePath: f.File.FilePath, Value: "not saved"}}
			wantError := "repeated parse facts"
			if name == "malformed" {
				bad.File.FilePath = "other.go"
				bad.File.NodeCount, bad.Nodes, bad.Shingles = 0, nil, nil
				wantError = "parse constant outside file"
			}
			err = b.AddParsedFile("repo", bad)
			require.ErrorIs(t, err, errInvalidRecord)
			require.ErrorContains(t, err, wantError)
			after, err := pendingGet(b.pending, parsedConstantKey(id), copyValue)
			require.NoError(t, err)
			require.Equal(t, before, after)
			missing, err := pendingGet(b.pending, fileMetaKey("repo", "other.go"), copyValue)
			require.NoError(t, err)
			require.Nil(t, missing)

			v := b.RebuildView()
			value, err := v.ConstantValue(ctx, id)
			require.Nil(t, value)
			require.ErrorIs(t, err, ErrRebuildView)
			require.ErrorIs(t, err, errInvalidRecord)
			err = v.VisitCloneShingles(ctx, "repo", func(string, []uint64) error {
				t.Fatal("a poisoned build must not visit saved shingles")
				return nil
			})
			require.ErrorIs(t, err, ErrRebuildView)
			require.ErrorIs(t, err, errInvalidRecord)
		})
	}
}

func TestRebuildParseSidecarsCorruptRead(t *testing.T) {
	b := newBuild(t, FullProfile)
	ctx := t.Context()
	f := parsedSidecarFixture(t)
	id := f.Nodes[0].ID
	require.NoError(t, b.AddParsedFile("repo", f))
	require.NoError(t, b.pending.Set(parsedConstantKey(id), []byte{0xff}, pebble.NoSync))
	require.NoError(t, b.pending.Set(shingleKey("repo", id), []byte{0xff}, pebble.NoSync))
	v := b.RebuildView()
	value, err := v.ConstantValue(ctx, id)
	require.Nil(t, value)
	require.ErrorIs(t, err, ErrRebuildView)
	require.ErrorIs(t, err, errInvalidRecord)
	err = v.VisitCloneShingles(ctx, "repo", func(string, []uint64) error {
		t.Fatal("a malformed row must not be yielded")
		return nil
	})
	require.ErrorIs(t, err, ErrRebuildView)
	require.ErrorIs(t, err, errInvalidRecord)
}

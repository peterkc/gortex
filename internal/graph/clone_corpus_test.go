package graph

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBulkSetCloneCorpusRetention(t *testing.T) {
	for _, tc := range []struct {
		name      string
		prefix    string
		row       CloneCorpusRow
		signature string
		finalized bool
	}{
		{"equal pending", "repo", CloneCorpusRow{Shingles: []uint64{1, 2}, TokenCount: 20}, "old", true},
		{"pending ignores signature", "repo", CloneCorpusRow{Shingles: []uint64{1, 2}, TokenCount: 20, Signature: "ignored"}, "old", true},
		{"changed shingles", "repo", CloneCorpusRow{Shingles: []uint64{3}, TokenCount: 20}, "", false},
		{"changed tokens", "repo", CloneCorpusRow{Shingles: []uint64{1, 2}, TokenCount: 21}, "", false},
		{"changed prefix", "other", CloneCorpusRow{Shingles: []uint64{1, 2}, TokenCount: 20}, "", false},
		{"row prefix override", "other", CloneCorpusRow{RepoPrefix: "repo", Shingles: []uint64{1, 2}, TokenCount: 20}, "old", true},
		{"finalized new", "repo", CloneCorpusRow{Shingles: []uint64{1, 2}, TokenCount: 20, Signature: "new", Finalized: true}, "new", true},
		{"finalized empty", "repo", CloneCorpusRow{Shingles: []uint64{1, 2}, TokenCount: 20, Finalized: true}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Given an existing finalized corpus entry.
			g := New()
			require.NoError(t, g.BulkSetCloneCorpus("repo", []CloneCorpusRow{{NodeID: "f", Shingles: []uint64{1, 2}, TokenCount: 20, Signature: "old", Finalized: true}}))
			// When the full-row writer receives a replacement.
			row := tc.row
			row.NodeID = "f"
			require.NoError(t, g.BulkSetCloneCorpus(tc.prefix, []CloneCorpusRow{row}))
			// Then only an equal pending write retains the stored signature and state.
			prefix := tc.prefix
			if row.RepoPrefix != "" {
				prefix = row.RepoPrefix
			}
			rows, err := g.CloneCorpusPage(prefix, "", 10)
			require.NoError(t, err)
			row.RepoPrefix, row.Signature, row.Finalized = prefix, tc.signature, tc.finalized
			require.Equal(t, []CloneCorpusRow{row}, rows)
		})
	}
}

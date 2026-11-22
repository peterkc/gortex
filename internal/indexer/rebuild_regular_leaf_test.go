package indexer

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer/merkle"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

func TestFreezeRebuildRegularLeaf(t *testing.T) {
	const shebang = "#!/bin/sh\n"
	const parserLimit = 128
	for _, tc := range []struct {
		name, body, language string
		member               rebuildGitMember
		listed               fs.FileMode
		cap, charged         int64
		rows                 int
		wantParse, sizeSkip  bool
		wantErr              error
	}{
		{name: "main.go", body: "package demo\n", language: "go", member: rebuildGitTracked, wantParse: true},
		{name: "script", body: shebang + "echo hello\n", language: "bash", member: rebuildGitUntracked, cap: 512, charged: 21, rows: 1, wantParse: true},
		{name: "oversized", body: shebang + strings.Repeat("#", 600), language: "bash", member: rebuildGitTracked, cap: 512, charged: 512, rows: 1, sizeSkip: true},
		{name: "oversized.go", body: strings.Repeat(" ", 600), language: "go", member: rebuildGitTracked, rows: 1, sizeSkip: true},
		{name: "unknown", body: "no language", member: rebuildGitTracked, cap: 11, charged: 11, rows: 1},
		{name: "ignored", body: shebang, member: 0},
		{name: "link", body: shebang, member: rebuildGitTracked, listed: fs.ModeSymlink},
		{name: "too-little-prefix-budget", body: shebang, member: rebuildGitUntracked, cap: 1, wantErr: errRebuildCppCaptureLimit},
		{name: "replaced-by-link", body: shebang, member: rebuildGitTracked, cap: 512, wantErr: ErrBuildInputChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			root, src := rebuildInputFixture(t)
			path := filepath.Join(root, tc.name)
			if tc.name == "link" || tc.name == "replaced-by-link" {
				require.NoError(t, os.WriteFile(filepath.Join(root, "target"), []byte(tc.body), 0o600))
				require.NoError(t, os.Symlink("target", path))
			} else {
				require.NoError(t, os.WriteFile(path, []byte(tc.body), 0o600))
			}
			cfg := config.Default().Index
			cfg.MaxFileSize = parserLimit
			reg := parser.NewRegistry()
			languages.RegisterAll(reg)
			idx := New(graph.New(), reg, cfg, zap.NewNop())
			t.Cleanup(idx.Close)
			remaining := tc.cap
			var rows []rebuildSidecarRow
			input, err := idx.freezeRebuildRegularLeaf(ctx, src.FrozenRoot(), tc.name, tc.listed,
				tc.member, idx.newContentAdmissionGate(), parserLimit, &remaining, func(row rebuildSidecarRow) error {
					require.NoError(t, row.check())
					rows = append(rows, row)
					return nil
				})
			require.ErrorIs(t, err, tc.wantErr)
			require.Equal(t, tc.charged, tc.cap-remaining)
			require.Len(t, rows, tc.rows)
			if len(rows) > 0 {
				fact := rows[0].Classification
				require.Equal(t, tc.sizeSkip, fact.sizeSkipped())
				require.Equal(t, tc.language, fact.Language)
				if fact.PrefixBytes > 0 {
					require.Equal(t, rebuildDigest(blake3.Sum256([]byte(tc.body[:fact.PrefixBytes]))), fact.PrefixDigest)
				}
			}
			if tc.wantParse {
				require.Equal(t, &rebuildInput{Path: tc.name, Kind: merkle.RegularFile, Size: int64(len(tc.body)),
					Language: tc.language, Content: blake3.Sum256([]byte(tc.body))}, input)
			} else {
				require.Nil(t, input)
			}
		})
	}
}

func TestFreezeRebuildRegularLeafPreservesContentSkips(t *testing.T) {
	ctx := t.Context()
	root, src := rebuildInputFixture(t)
	cfg := config.Default().Index
	cfg.MaxFileSize = 1024
	cfg.Content.MaxImageBytes = 256
	cfg.SkipUntrackedAssets = true
	idx := New(graph.New(), newAssetTestRegistry(), cfg, zap.NewNop())
	t.Cleanup(idx.Close)
	const key = "large.gif"
	require.NoError(t, os.WriteFile(filepath.Join(root, key), make([]byte, 600), 0o600))
	for _, tc := range []struct {
		member rebuildGitMember
		reason string
	}{{rebuildGitTracked, skipReasonLargeImage}, {rebuildGitUntracked, skipReasonUntrackedAsset}} {
		var raw int64
		var rows []rebuildSidecarRow
		// A known-name asset skip is decided before the parse byte limit
		// and produces no input, even with no room for a content read.
		input, err := idx.freezeRebuildRegularLeaf(ctx, src.FrozenRoot(), key, 0, tc.member, idx.newContentAdmissionGate(), 1, &raw,
			func(row rebuildSidecarRow) error { rows = append(rows, row); return row.check() })
		require.NoError(t, err)
		require.Nil(t, input)
		require.Len(t, rows, 1)
		require.Equal(t, tc.reason, rows[0].Classification.ContentSkipReason)
		require.Zero(t, rows[0].Classification.PrefixBytes)
		require.Zero(t, raw)
	}
}

func TestReadRebuildClassifiedBodyRefusesDrift(t *testing.T) {
	ctx := t.Context()
	root, src := rebuildInputFixture(t)
	name := filepath.Join(root, "script")
	require.NoError(t, os.WriteFile(name, []byte("#!/bin/sh\n"+strings.Repeat("a", 600)), 0o600))
	obs, err := source.ObserveFrozenLiteralRootPath(ctx, src.FrozenRoot(), "script")
	require.NoError(t, err)
	prefix, err := obs.ReadPrefix(ctx, sniffPrefixBytes)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(name, []byte("#!/bin/sh\n"+strings.Repeat("b", 600)), 0o600))
	fresh, err := source.ObserveFrozenLiteralRootPath(ctx, src.FrozenRoot(), "script")
	require.NoError(t, err)
	// Even if existing metadata checks alone could accept the fresh
	// observation, no body may be bound to a different prefix.
	data, err := readRebuildClassifiedBody(ctx, fresh, 1024, prefix)
	require.ErrorIs(t, err, ErrBuildInputChanged)
	require.Nil(t, data)
}

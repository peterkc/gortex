package indexer

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/gitcmd"
)

func TestRebuildGitIgnoreNativeBatch(t *testing.T) {
	builderIsolateGit(t)
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
	dir := t.TempDir()
	out, err := gitcmd.Run(t.Context(), dir, "init", "--quiet", "--template=")
	require.NoError(t, err, "%s", out)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.tmp\n!keep.tmp\noutput/\n"), 0o600))

	const pathWithRawBytes = "sp ace\n\xff.tmp"
	const input = pathWithRawBytes + "\x00keep.tmp\x00output/\x00quiet\x00"
	const reply = ".gitignore\x001\x00*.tmp\x00" + pathWithRawBytes + "\x00" +
		".gitignore\x002\x00!keep.tmp\x00keep.tmp\x00" +
		".gitignore\x003\x00output/\x00output/\x00" + "\x00\x00\x00quiet\x00"
	wantBytes := int64(len(input) + len(reply))
	got, used, err := checkRebuildGitIgnore(t.Context(), dir, []string{pathWithRawBytes, "keep.tmp", "output/", "quiet"}, wantBytes)
	require.NoError(t, err)
	require.Equal(t, []rebuildGitIgnoreVerdict{rebuildGitIgnoreIgnored, rebuildGitIgnoreMatchedNegation,
		rebuildGitIgnoreIgnored, rebuildGitIgnoreNoMatch}, got)
	require.Equal(t, wantBytes, used)

	got, used, err = checkRebuildGitIgnore(t.Context(), dir, []string{"keep.tmp"}, wantBytes)
	require.NoError(t, err)
	require.Equal(t, []rebuildGitIgnoreVerdict{rebuildGitIgnoreMatchedNegation}, got)
	require.EqualValues(t, len("keep.tmp\x00")+len(".gitignore\x002\x00!keep.tmp\x00keep.tmp\x00"), used)

	// A complete non-match exits 1; it is not an empty result or a retry.
	got, used, err = checkRebuildGitIgnore(t.Context(), dir, []string{"quiet"}, 15)
	require.NoError(t, err)
	require.Equal(t, []rebuildGitIgnoreVerdict{rebuildGitIgnoreNoMatch}, got)
	require.EqualValues(t, 15, used)
}

func TestRebuildGitIgnoreMalformedAnswers(t *testing.T) {
	const a = "\x00\x00\x00a\x00"
	const b = "/operator/excludes\x0021\x00*\x00b\x00"
	for _, tc := range []struct {
		name    string
		wire    string
		exitOne bool
	}{
		{name: "missing", wire: a},
		{name: "reordered", wire: b + a},
		{name: "duplicate", wire: a + a},
		{name: "extra", wire: a + b + a},
		{name: "wrong echo", wire: a + strings.Replace(b, "b\x00", "other\x00", 1)},
		{name: "missing terminator", wire: a + strings.TrimSuffix(b, "\x00")},
		{name: "partial metadata", wire: a + "\x0021\x00*\x00b\x00"},
		{name: "invalid line", wire: a + strings.Replace(b, "21", "2a", 1)},
		{name: "exit one with positive rule", wire: a + b, exitOne: true},
		{name: "exit zero without a match", wire: a + "\x00\x00\x00b\x00"},
		{name: "exit one with missing answer", wire: a, exitOne: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRebuildGitIgnore(t.Context(), []string{"a", "b"}, []byte(tc.wire), tc.exitOne)
			require.ErrorIs(t, err, errRebuildGitIgnore)
			require.Nil(t, got)
		})
	}
	got, err := parseRebuildGitIgnore(t.Context(), []string{"a", "b"},
		[]byte(a+"src\x001\x00"+strings.Repeat("*", rebuildCppEntryLimit)+"\x00b\x00"), false)
	require.ErrorIs(t, err, errRebuildCppCaptureLimit)
	require.Nil(t, got)
}

func TestRebuildGitIgnoreInputRefusals(t *testing.T) {
	dir := t.TempDir()
	for _, paths := range [][]string{{"../other"}, {"x\x00y"}, {"x//"}, {"x", "x/"}, {"x", "x"}} {
		got, used, err := checkRebuildGitIgnore(t.Context(), dir, paths, rebuildCppRawLimit)
		require.ErrorIs(t, err, errRebuildGitIgnore)
		require.Nil(t, got)
		require.Zero(t, used)
	}
	for _, args := range []struct {
		paths []string
		limit int64
	}{
		{paths: []string{"a"}, limit: 6}, // Needs at least seven wire bytes.
		{paths: make([]string, rebuildGitIgnoreBatchLimit+1), limit: rebuildCppRawLimit},
	} {
		got, used, err := checkRebuildGitIgnore(t.Context(), dir, args.paths, args.limit)
		require.ErrorIs(t, err, errRebuildCppCaptureLimit)
		require.Nil(t, got)
		require.Zero(t, used)
	}
}

func TestRebuildGitIgnoreCommandRefusals(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake Git uses a POSIX shell")
	}
	bin := t.TempDir()
	dir := t.TempDir()
	spawn := filepath.Join(bin, "spawn")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("T61_CHECKOUT", dir)
	t.Setenv("T61_SPAWN", spawn)
	require.NoError(t, os.WriteFile(filepath.Join(bin, "git"), []byte(`#!/bin/sh
[ "$#" -eq 8 ] && [ "$1" = -C ] && [ "$2" = "$T61_CHECKOUT" ] &&
  [ "$3 $4 $5 $6 $7 $8" = 'check-ignore --no-index --stdin -z -v -n' ] || exit 97
case "$T61_FAULT" in
cap) printf 'src\0001\000*\000a\000' ;;
warn-zero) printf 'src\0001\000*\000a\000'; printf 'ignore read failed' >&2 ;;
warn-one) printf '\000\000\000a\000'; printf 'ignore read failed' >&2; exit 1 ;;
fatal) printf '\000\000\000a\000'; exit 128 ;;
timeout) printf '\000\000\000a\000'; printf 'started' > "$T61_SPAWN"; exec sleep 60 ;;
esac
`), 0o700))
	for _, tc := range []struct {
		fault string
		limit int64
		cause error
	}{
		{fault: "cap", limit: 7, cause: gitcmd.ErrOutputLimit},
		{fault: "warn-zero", limit: 64},
		{fault: "warn-one", limit: 64},
		{fault: "fatal", limit: 64},
		{fault: "timeout", limit: 64, cause: context.DeadlineExceeded},
	} {
		t.Run(tc.fault, func(t *testing.T) {
			t.Setenv("T61_FAULT", tc.fault)
			ctx := t.Context()
			if tc.fault == "timeout" {
				deadline, cancel := context.WithTimeout(ctx, 2*time.Second)
				defer cancel()
				ctx = deadline
			}
			got, used, err := checkRebuildGitIgnore(ctx, dir, []string{"a"}, tc.limit)
			require.ErrorIs(t, err, errRebuildGitIgnore)
			require.Nil(t, got)
			require.Zero(t, used)
			if tc.cause != nil {
				require.ErrorIs(t, err, tc.cause)
			}
			if tc.fault == "timeout" {
				started, err := os.ReadFile(spawn)
				require.NoError(t, err)
				require.Equal(t, "started", string(started))
			} else if tc.fault == "warn-zero" {
				require.ErrorContains(t, err, "stderr")
			} else if tc.fault == "warn-one" {
				require.ErrorContains(t, err, "ignore read failed")
			} else if tc.fault == "fatal" {
				require.ErrorContains(t, err, "exit status 128")
			}
		})
	}
}

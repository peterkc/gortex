package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/resolver"
)

func TestRebuildCppReplayOrder(t *testing.T) {
	lease := t.TempDir()
	digest := writeRebuildCppReplayFacts(t, lease,
		rebuildCppFact{Kind: rebuildCppTU, Path: "a.c", IncludeDirs: []string{"discard"}},
		rebuildCppFact{Kind: rebuildCppTU, Path: "empty.c", IncludeDirs: []string{"discard-too"}},
		rebuildCppFact{Kind: rebuildCppTU, Path: "b-\xff.c", IncludeDirs: []string{"raw-\xff", "a"}},
		rebuildCppFact{Kind: rebuildCppTU, Path: "a.c", IncludeDirs: []string{"z", "a"}},
		rebuildCppFact{Kind: rebuildCppTU, Path: "empty.c"},
		rebuildCppFact{Kind: rebuildCppHeuristicDir, Path: "include"},
	)
	r := openTestRebuildCppReplay(t, lease, digest)
	// Replay uses only the previously verified index, not the carrier or tree.
	require.NoError(t, os.Remove(filepath.Join(lease, rebuildInputSidecarName)))
	for _, tc := range []struct {
		file string
		dirs []string
	}{
		{"a.c", []string{"z", "a"}},
		{"b-\xff.c", []string{"raw-\xff", "a"}},
		{"empty.c", []string{"a", "raw-\xff", "z"}},
		{"missing.h", []string{"a", "raw-\xff", "z"}},
	} {
		t.Run(tc.file, func(t *testing.T) {
			checkRebuildCppReplay(t, r.Visit, tc.file, true, tc.dirs)
		})
	}
}

func TestRebuildCppReplayFallback(t *testing.T) {
	for _, emptyTU := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty-TU=%v", emptyTU), func(t *testing.T) {
			lease := t.TempDir()
			facts := []rebuildCppFact{
				{Kind: rebuildCppDatabase, Path: compileDBName, Present: true, Digest: rebuildDigest{1}},
				{Kind: rebuildCppHeuristicDir, Path: "include"},
				{Kind: rebuildCppHeuristicDir, Path: "src"},
				{Kind: rebuildCppHeuristicDir, Path: "api"},
				{Kind: rebuildCppHeuristicDir, Path: "aardvark"},
			}
			want := []string{"include", "src", "api", "aardvark"}
			if emptyTU {
				facts = append(facts, rebuildCppFact{Kind: rebuildCppTU, Path: "empty.c"})
				want = nil
			}
			r := openTestRebuildCppReplay(t, lease, writeRebuildCppReplayFacts(t, lease, facts...))
			checkRebuildCppReplay(t, r.Visit, "missing.h", emptyTU, want)
			checkRebuildCppReplay(t, r.Visit, "empty.c", emptyTU, want)
		})
	}
}

func TestRebuildCppReplayVisit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		db    bool
		file  string
		first string
	}{
		{"tu", true, "main.c", "z"},
		{"union", true, "missing.h", "a"},
		{"heuristic", false, "main.c", "src"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lease := t.TempDir()
			facts := []rebuildCppFact{
				{Kind: rebuildCppHeuristicDir, Path: "src"},
				{Kind: rebuildCppHeuristicDir, Path: "api"},
			}
			if tc.db {
				facts = append(facts, rebuildCppFact{Kind: rebuildCppTU, Path: "main.c", IncludeDirs: []string{"z", "a"}})
			}
			r := openTestRebuildCppReplay(t, lease, writeRebuildCppReplayFacts(t, lease, facts...))
			stopErr := errors.New("visitor failed")
			for _, mode := range []string{"stop", "error", "cancel", "already canceled"} {
				t.Run(mode, func(t *testing.T) {
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					var wantErr error
					wantCalls := 1
					switch mode {
					case "error":
						wantErr = stopErr
					case "cancel":
						wantErr = context.Canceled
					case "already canceled":
						cancel()
						wantErr, wantCalls = context.Canceled, 0
					}
					calls := 0
					db, err := r.Visit(ctx, tc.file, func(dir string) (bool, error) {
						calls++
						require.Equal(t, tc.first, dir)
						switch mode {
						case "error":
							return true, stopErr
						case "cancel":
							cancel()
							return false, nil
						default:
							return true, nil
						}
					})
					require.Equal(t, tc.db, db)
					require.True(t, wantErr == err, "got %v, want %v", err, wantErr)
					require.Equal(t, wantCalls, calls)
				})
			}
		})
	}
}

func TestRebuildCppReplayCorruptCleanup(t *testing.T) {
	lease := t.TempDir()
	digest := writeRebuildCppReplayFacts(t, lease, rebuildCppFact{Kind: rebuildCppTU, Path: "a.c", IncludeDirs: []string{"include"}})
	wrong := digest
	wrong[0] ^= 1
	assertRejected := func(expected rebuildDigest) {
		t.Helper()
		r, err := openRebuildCppReplay(t.Context(), lease, expected)
		require.ErrorIs(t, err, errRebuildInputSidecar)
		require.Nil(t, r, "a prefix of a refused input is not replayable")
		entries, err := os.ReadDir(lease)
		require.NoError(t, err)
		require.Len(t, entries, 1, "remove scratch after refusing the sidecar")
		require.Equal(t, rebuildInputSidecarName, entries[0].Name())
	}
	assertRejected(wrong)
	path := filepath.Join(lease, rebuildInputSidecarName)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	raw = append(raw, "{not json}\n"...)
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	assertRejected(blake3.Sum256(raw))
}

func TestRebuildCppReplaySpill(t *testing.T) {
	lease := t.TempDir()
	const n = 15000
	prefix := strings.Repeat("parts/", 100)
	dir := func(i int) string { return fmt.Sprintf("%s%05d", prefix, i) }
	digest, err := writeRebuildInputSidecar(t.Context(), lease, func(emit func(rebuildSidecarRow) error) error {
		tu := func(file, path string) error {
			return emit(rebuildSidecarCppRow(rebuildCppFact{Kind: rebuildCppTU, Path: file, IncludeDirs: []string{path}}))
		}
		if err := tu("first.c", "obsolete"); err != nil {
			return err
		}
		for i := 1; i < n; i++ {
			if err := tu(fmt.Sprintf("source/%05d.c", i), dir(i)); err != nil {
				return err
			}
		}
		return tu("first.c", dir(0)) // replacement occurs in a later SST
	})
	require.NoError(t, err)
	r := openTestRebuildCppReplay(t, lease, digest)
	tables, err := filepath.Glob(filepath.Join(lease, "cpp-replay-*", "db", "*.sst"))
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(tables), 4, "TU and union each spill multiple runs")
	for _, table := range tables {
		info, err := os.Stat(table)
		require.NoError(t, err)
		require.Positive(t, info.Size(), "physical SST bytes")
	}
	checkRebuildCppReplay(t, r.Visit, "first.c", true, []string{dir(0)})
	visited := 0
	db, err := r.Visit(t.Context(), "missing.h", func(got string) (bool, error) {
		require.Less(t, visited, n)
		require.Equal(t, dir(visited), got)
		visited++
		return false, nil
	})
	require.NoError(t, err)
	require.True(t, db)
	require.Equal(t, n, visited)
}

func writeRebuildCppReplayFacts(t *testing.T, lease string, facts ...rebuildCppFact) rebuildDigest {
	t.Helper()
	digest, err := writeRebuildInputSidecar(t.Context(), lease, func(emit func(rebuildSidecarRow) error) error {
		for _, fact := range facts {
			if err := emit(rebuildSidecarCppRow(fact)); err != nil {
				return err
			}
		}
		return emit(rebuildSidecarRow{Kind: rebuildFollowedManifest, Path: []byte("go.mod"), Present: true, Digest: rebuildDigest{2}})
	})
	require.NoError(t, err)
	return digest
}

func openTestRebuildCppReplay(t *testing.T, lease string, digest rebuildDigest) *store_pebble.RebuildCppReplay {
	t.Helper()
	r, err := openRebuildCppReplay(t.Context(), lease, digest)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, r.Close())
		dirs, err := filepath.Glob(filepath.Join(lease, "cpp-replay-*"))
		require.NoError(t, err)
		require.Empty(t, dirs)
		require.DirExists(t, lease)
	})
	return r
}

func checkRebuildCppReplay(t *testing.T, visit resolver.RebuildCppIncludePaths, file string, wantDB bool, wantDirs []string) {
	t.Helper()
	var dirs []string
	db, err := visit(t.Context(), file, func(dir string) (bool, error) {
		dirs = append(dirs, dir)
		return false, nil
	})
	require.NoError(t, err)
	require.Equal(t, wantDB, db)
	require.Equal(t, wantDirs, dirs)
}

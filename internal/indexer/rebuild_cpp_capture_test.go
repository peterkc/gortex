package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/indexer/source"
)

func TestRebuildCppCaptureOrder(t *testing.T) {
	encode := func(rows ...compileCommand) []byte {
		t.Helper()
		data, err := json.Marshal(rows)
		require.NoError(t, err)
		return data
	}
	rootDB := encode(
		compileCommand{File: "src/a.c", Arguments: []string{"cc", "-Iignored"}},
		compileCommand{File: "src/b.c", Command: `cc -I"quoted dir" -isystem z -iquote /unrelated -idirafter z private_token`},
		compileCommand{File: "src/empty.c", Arguments: []string{"cc", "-Ioverridden"}},
	)
	fastDB := encode(compileCommand{File: "src/a.c", Arguments: []string{"cc", "-Ifast"}})
	buildDB := encode(
		compileCommand{File: "src/a.c", Command: "cc -Iunused", Arguments: []string{"cc", "-I", "final", "-I./final"}},
		compileCommand{File: "src/empty.c", Arguments: []string{"cc", "-I/unrelated"}},
	)
	wantDBs := []rebuildCppFact{
		{Kind: rebuildCppDatabase, Path: compileDBName, Present: true, Digest: blake3.Sum256(rootDB)},
		{Kind: rebuildCppDatabase, Path: "build-absent/" + compileDBName},
		{Kind: rebuildCppDatabase, Path: "build-fast/" + compileDBName, Present: true, Digest: blake3.Sum256(fastDB)},
		{Kind: rebuildCppDatabase, Path: "build/" + compileDBName, Present: true, Digest: blake3.Sum256(buildDB)},
	}
	capture := func() []rebuildCppFact {
		t.Helper()
		dir := t.TempDir()
		for rel, data := range map[string][]byte{
			compileDBName: rootDB, "build/" + compileDBName: buildDB, "build-fast/" + compileDBName: fastDB,
			"build-absent/keep": nil, "build-not-dir": nil,
		} {
			putRebuildCppFixture(t, dir, rel, data)
		}
		// A valid compile DB prevents consultation of heuristic roots.
		require.NoError(t, os.Symlink("../../never_consulted", filepath.Join(dir, "include")))
		facts, left, err := gatherRebuildCppFacts(t, dir, int64(len(rootDB)+len(buildDB)+len(fastDB)), source.FrozenRootPath.Read)
		require.NoError(t, err)
		require.Zero(t, left)
		var dbs []rebuildCppFact
		tus := map[string]cppTU{}
		for _, fact := range facts {
			switch fact.Kind {
			case rebuildCppDatabase:
				dbs = append(dbs, fact)
			case rebuildCppTU:
				tus[fact.Path] = cppTU{file: fact.Path, includeDirs: fact.IncludeDirs}
			default:
				t.Fatalf("unexpected fact %#v", fact)
			}
		}
		require.Equal(t, wantDBs, dbs)
		require.Len(t, facts, 10)
		require.Equal(t, map[string]cppTU{
			"src/a.c":     {file: "src/a.c", includeDirs: []string{"final"}},
			"src/b.c":     {file: "src/b.c", includeDirs: []string{"quoted dir", "z"}},
			"src/empty.c": {file: "src/empty.c"},
		}, tus)
		clearCppIncludeDirCache(dir)
		t.Cleanup(func() { clearCppIncludeDirCache(dir) })
		require.Equal(t, loadCompileCommands(newDiskManifestTree(dir)), tus)
		encoded, err := json.Marshal(facts)
		require.NoError(t, err)
		for _, secret := range []string{dir, "private_token", "/unrelated", "cc -Iunused"} {
			require.NotContains(t, string(encoded), secret)
		}
		return facts
	}
	require.Equal(t, capture(), capture(), "equal bytes and topology at a different root")
}

func TestRebuildCppCaptureLinks(t *testing.T) {
	dir := t.TempDir()
	data := []byte(`[{"file":"linked.c","arguments":["cc","-Iinc"]}]`)
	putRebuildCppFixture(t, dir, "db/actual.json", data)
	require.NoError(t, os.Symlink("actual.json", filepath.Join(dir, "db", compileDBName)))
	require.NoError(t, os.Symlink("db", filepath.Join(dir, "build-linked")))
	facts, left, err := gatherRebuildCppFacts(t, dir, int64(len(data)), source.FrozenRootPath.Read)
	require.NoError(t, err)
	require.Zero(t, left)
	require.Equal(t, []rebuildCppFact{
		{Kind: rebuildCppDatabase, Path: compileDBName},
		{Kind: rebuildCppDatabase, Path: "build-linked/" + compileDBName, Present: true, Digest: blake3.Sum256(data)},
		{Kind: rebuildCppTU, Path: "linked.c", IncludeDirs: []string{"inc"}},
	}, facts)
}

func TestRebuildCppCaptureLiteralRoot(t *testing.T) {
	data := []byte(`[{"file":"src/a.c","arguments":["cc","-Iinc"]}]`)
	var want []rebuildCppFact
	for _, name := range []string{"plain", "repo[1]"} {
		// Give the bracketed root no matching sibling, so the native oracle
		// records the missed DB without probing another checkout's file.
		dir := filepath.Join(t.TempDir(), name)
		putRebuildCppFixture(t, dir, "real/"+compileDBName, data)
		require.NoError(t, os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "build")))
		got, left, err := gatherRebuildCppFacts(t, dir, int64(len(data)), source.FrozenRootPath.Read)
		require.NoError(t, err)
		require.Zero(t, left)
		require.Equal(t, []rebuildCppFact{
			{Kind: rebuildCppDatabase, Path: compileDBName},
			{Kind: rebuildCppDatabase, Path: "build/" + compileDBName, Present: true, Digest: blake3.Sum256(data)},
			{Kind: rebuildCppTU, Path: "src/a.c", IncludeDirs: []string{"inc"}},
		}, got)
		if want == nil {
			want = got
		} else {
			require.Equal(t, want, got)
		}
		native := loadCompileCommands(newDiskManifestTree(dir))
		clearCppIncludeDirCache(dir)
		if name == "plain" {
			require.Equal(t, []string{"inc"}, native["src/a.c"].includeDirs)
		} else {
			require.Empty(t, native, "native glob treats the checkout name as a pattern")
		}
	}
}

func TestRebuildCppCaptureHeuristic(t *testing.T) {
	dir := t.TempDir()
	wrongType := []byte(`[{"file":"not-kept.c","arguments":["-Iwrong"]},{"file":17}]`)
	trailing := []byte(`[{"file":"also-not-kept.c"}] null`)
	for rel, data := range map[string][]byte{
		compileDBName: wrongType, "build-bad/" + compileDBName: trailing,
		"headers/direct.hpp": nil, "src/nested/keep": nil, "not-a-root/sub/child.h": nil,
		".hidden/hidden.h": nil, "not-a-root/not-a-header.h/.keep": nil,
	} {
		putRebuildCppFixture(t, dir, rel, data)
	}
	for name, target := range map[string]string{
		"include": "headers", "other-alias": "headers", "z-last/dangling.h": "../../outside/never-read.h",
	} {
		abs := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
		require.NoError(t, os.Symlink(target, abs))
	}
	facts, left, err := gatherRebuildCppFacts(t, dir, int64(len(wrongType)+len(trailing)), source.FrozenRootPath.Read)
	require.NoError(t, err)
	require.Zero(t, left)
	require.Equal(t, []rebuildCppFact{
		{Kind: rebuildCppDatabase, Path: compileDBName, Present: true, Digest: blake3.Sum256(wrongType)},
		{Kind: rebuildCppDatabase, Path: "build-bad/" + compileDBName, Present: true, Digest: blake3.Sum256(trailing)},
		{Kind: rebuildCppHeuristicDir, Path: "include"},
		{Kind: rebuildCppHeuristicDir, Path: "src"},
		{Kind: rebuildCppHeuristicDir, Path: "headers"},
		{Kind: rebuildCppHeuristicDir, Path: "z-last"},
	}, facts, "a bad row or trailing JSON must not leave any TU from its database")
	require.Empty(t, loadCompileCommands(newDiskManifestTree(dir)))
	t.Cleanup(func() { clearCppIncludeDirCache(dir) })
	require.Equal(t, []string{"include", "src", "headers", "z-last"}, heuristicIncludeDirs(newDiskManifestTree(dir)))
}

func TestRebuildCppCaptureLimits(t *testing.T) {
	t.Run("raw across databases", func(t *testing.T) {
		dir := t.TempDir()
		first := []byte(`[{"file":"source.c"}]`)
		second := []byte(`[broken`)
		putRebuildCppFixture(t, dir, compileDBName, first)
		putRebuildCppFixture(t, dir, "build/"+compileDBName, second)
		facts, left, err := gatherRebuildCppFacts(t, dir, int64(len(first)+len(second)-1), source.FrozenRootPath.Read)
		require.ErrorIs(t, err, errRebuildCppCaptureLimit)
		require.EqualValues(t, len(second)-1, left)
		require.Len(t, facts, 2, "only the first DB receipt and its TU precede refusal")
	})
	t.Run("entry", func(t *testing.T) {
		row := func(n int) []byte {
			t.Helper()
			const envelope = `{"file":"source.c","unknown":""}`
			data, err := json.Marshal(struct {
				File    string `json:"file"`
				Unknown string `json:"unknown"`
			}{"source.c", strings.Repeat("x", n-len(envelope))})
			require.NoError(t, err)
			require.Len(t, data, n)
			return data
		}
		dir := t.TempDir()
		putRebuildCppFixture(t, dir, compileDBName, append(append([]byte("["), row(rebuildCppEntryLimit)...), ']'))
		facts, _, err := gatherRebuildCppFacts(t, dir, rebuildCppRawLimit, source.FrozenRootPath.Read)
		require.NoError(t, err)
		require.Len(t, facts, 2)
		require.Equal(t, rebuildCppFact{Kind: rebuildCppTU, Path: "source.c"}, facts[1])
		// A wrong-typed entry must not hide the oversized entry after it.
		bad := append(append([]byte(`[{"file":19},`), row(rebuildCppEntryLimit+1)...), ']')
		putRebuildCppFixture(t, dir, compileDBName, bad)
		facts, _, err = gatherRebuildCppFacts(t, dir, rebuildCppRawLimit, source.FrozenRootPath.Read)
		require.ErrorIs(t, err, errRebuildCppCaptureLimit)
		require.Equal(t, []rebuildCppFact{{Kind: rebuildCppDatabase, Path: compileDBName, Present: true, Digest: blake3.Sum256(bad)}}, facts)
		// The decoder reaches the limit in an unclosed string, not a full row.
		broken := []byte(`[{"unknown":"` + strings.Repeat("x", rebuildCppEntryLimit))
		putRebuildCppFixture(t, dir, compileDBName, broken)
		facts, _, err = gatherRebuildCppFacts(t, dir, rebuildCppRawLimit, source.FrozenRootPath.Read)
		require.ErrorIs(t, err, errRebuildCppCaptureLimit)
		require.Len(t, facts, 1)
	})
	t.Run("normalized entry", func(t *testing.T) {
		dir := t.TempDir()
		cc := compileCommand{Directory: dir + "/" + strings.Repeat("d/", 1800), File: "source.c"}
		for i := range 350 {
			cc.Arguments = append(cc.Arguments, fmt.Sprintf("-Iinc%d", i))
		}
		data, err := json.Marshal([]compileCommand{cc})
		require.NoError(t, err)
		require.Less(t, len(data), rebuildCppEntryLimit)
		putRebuildCppFixture(t, dir, compileDBName, data)
		facts, _, err := gatherRebuildCppFacts(t, dir, rebuildCppRawLimit, source.FrozenRootPath.Read)
		require.ErrorIs(t, err, errRebuildCppCaptureLimit)
		require.Len(t, facts, 1, "no partial TU escapes if normalization exceeds the limit")
	})
}

func TestRebuildCppCaptureRefusal(t *testing.T) {
	t.Run("escaped links", func(t *testing.T) {
		base := t.TempDir()
		outside := []byte(`[{"file":"outside.c"}]`)
		putRebuildCppFixture(t, base, "outside/"+compileDBName, outside)
		for _, tc := range []struct {
			path   string
			target string
		}{
			{compileDBName, "../outside/" + compileDBName},
			{"build-linked", filepath.Join(base, "outside")},
			{"include", "../outside"},
		} {
			dir, err := os.MkdirTemp(base, "root-")
			require.NoError(t, err)
			require.NoError(t, os.Symlink(tc.target, filepath.Join(dir, tc.path)))
			facts, left, err := gatherRebuildCppFacts(t, dir, rebuildCppRawLimit,
				func(source.FrozenRootPath, context.Context, int64) ([]byte, error) {
					t.Error("the confined path probe must refuse before a content read")
					return nil, errors.New("unexpected read")
				})
			require.ErrorIs(t, err, errRebuildCppCapturePath)
			require.EqualValues(t, rebuildCppRawLimit, left)
			if tc.path == compileDBName {
				require.Empty(t, facts, "an escaped candidate is not known absent")
			} else {
				require.Equal(t, []rebuildCppFact{{Kind: rebuildCppDatabase, Path: compileDBName}}, facts)
			}
		}
	})
	t.Run("observed candidate replaced", func(t *testing.T) {
		dir := t.TempDir()
		data := []byte(`[{"file":"source.c"}]`)
		putRebuildCppFixture(t, dir, compileDBName, data)
		putRebuildCppFixture(t, dir, "replacement", data)
		facts, left, err := gatherRebuildCppFacts(t, dir, rebuildCppRawLimit,
			func(observed source.FrozenRootPath, ctx context.Context, limit int64) ([]byte, error) {
				require.NoError(t, os.Rename(filepath.Join(dir, "replacement"), filepath.Join(dir, compileDBName)))
				return observed.Read(ctx, limit)
			})
		require.ErrorIs(t, err, errRebuildCppCapturePath)
		require.ErrorIs(t, err, source.ErrFrozenRootChanged)
		require.Empty(t, facts)
		require.EqualValues(t, rebuildCppRawLimit, left)
	})
}

func TestRebuildCppCaptureSpecial(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("Unix-domain socket fixture")
	}
	dir := t.TempDir()
	ln, err := net.Listen("unix", filepath.Join(dir, "s"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ln.Close()) })
	require.NoError(t, os.Symlink("s", filepath.Join(dir, compileDBName)))
	facts, left, err := gatherRebuildCppFacts(t, dir, rebuildCppRawLimit,
		func(source.FrozenRootPath, context.Context, int64) ([]byte, error) {
			t.Error("a socket must not reach the content reader")
			return nil, errors.New("unexpected read")
		})
	require.ErrorIs(t, err, errRebuildCppCapturePath)
	require.Empty(t, facts)
	require.EqualValues(t, rebuildCppRawLimit, left)
}

func putRebuildCppFixture(t *testing.T, dir, rel string, data []byte) {
	t.Helper()
	name := filepath.Join(dir, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o755))
	require.NoError(t, os.WriteFile(name, data, 0o600))
}

func gatherRebuildCppFacts(t *testing.T, dir string, left int64,
	read func(source.FrozenRootPath, context.Context, int64) ([]byte, error),
) ([]rebuildCppFact, int64, error) {
	t.Helper()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })
	var facts []rebuildCppFact
	err = captureRebuildCppInputs(t.Context(), root, dir, &left, read, func(fact rebuildCppFact) error {
		facts = append(facts, fact)
		return nil
	})
	return facts, left, err
}

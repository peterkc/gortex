package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graph/storetest"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// k12PrivateTemp points the process temp directory and the per-user
// directories at fresh paths and returns the temp directory, so a case can
// see every directory a constructor creates there.
func k12PrivateTemp(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	tmp := filepath.Join(base, "tmp")
	home := filepath.Join(base, "home")
	for _, dir := range []string{tmp, home} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	if got := os.TempDir(); got != tmp {
		t.Fatalf("os.TempDir() = %q, want the private directory %q", got, tmp)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	return tmp
}

// tempEntries lists the names in dir.
func tempEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// testEvalStoreIsPrivateAndRemovedOnCleanup: each eval run opens its own
// store in its own temp directory, the store serves fixture F's R as the eval
// harnesses index it and answers symbol search, and cleanup removes the
// directory; a later run starts empty.
func testEvalStoreIsPrivateAndRemovedOnCleanup(t *testing.T, backend string) {
	if backend != "sqlite" {
		t.Fatalf("the constructor has no backend selection and opens SQLite, not listed backend %q", backend)
	}
	f := storetest.NewFixtureF(t)
	tmp := k12PrivateTemp(t)

	first, closeFirst, err := newEvalStore("k12")
	if err != nil {
		t.Fatalf("open the first eval store: %v", err)
	}
	t.Cleanup(closeFirst)
	requireK12SQLiteStore(t, first)
	second, closeSecond, err := newEvalStore("k12")
	if err != nil {
		closeFirst()
		t.Fatalf("open the second eval store: %v", err)
	}
	t.Cleanup(closeSecond)
	requireK12SQLiteStore(t, second)
	if got := tempEntries(t, tmp); len(got) != 2 {
		t.Fatalf("two eval stores made temp entries %v, want one directory each", got)
	}

	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := indexer.New(first, reg, config.Default().Index, zap.NewNop())
	_, err = idx.Index(f.R.Root)
	idx.Close()
	if err != nil {
		t.Fatalf("index R into the eval store: %v", err)
	}
	assertServesFixture(t, first, f, f.R.Prefix, storetest.E1, true)
	searcher, ok := first.(graph.SymbolSearcher)
	if !ok {
		t.Fatalf("the eval store %T does not serve symbol search", first)
	}
	hits, err := searcher.SearchSymbols("Twice", 10)
	if err != nil || len(hits) != 1 || hits[0].NodeID != "app/twice.go::Twice" {
		t.Errorf("symbol search for Twice = %v, %v; want the one hit app/twice.go::Twice", hits, err)
	}
	if n := second.NodeCount(); n != 0 {
		t.Errorf("the second eval store serves %d nodes the first run indexed, want 0", n)
	}

	closeFirst()
	closeSecond()
	if got := tempEntries(t, tmp); len(got) != 0 {
		t.Fatalf("eval cleanup left %v in the temp directory", got)
	}
	again, closeAgain, err := newEvalStore("k12")
	if err != nil {
		t.Fatalf("reopen an eval store: %v", err)
	}
	defer closeAgain()
	requireK12SQLiteStore(t, again)
	if n := again.NodeCount(); n != 0 {
		t.Errorf("a new eval store after cleanup serves %d nodes, want 0", n)
	}
}

// testInitIndexesIntoAPrivateStoreAndRemovesIt: init indexes fixture F's R
// into a store in its own temp directory and its cleanup removes it; an index
// that fails also leaves nothing behind.
func testInitIndexesIntoAPrivateStoreAndRemovesIt(t *testing.T, backend string) {
	if backend != "sqlite" {
		t.Fatalf("the constructor has no backend selection and opens SQLite, not listed backend %q", backend)
	}
	f := storetest.NewFixtureF(t)
	tmp := k12PrivateTemp(t)
	ctx := k12Context(t)

	store, cleanup, err := indexRepoForInit(ctx, f.R.Root, zap.NewNop())
	if err != nil {
		t.Fatalf("init index of R: %v", err)
	}
	t.Cleanup(cleanup)
	requireK12SQLiteStore(t, store)
	if got := tempEntries(t, tmp); len(got) != 1 {
		cleanup()
		t.Fatalf("init made temp entries %v, want one store directory", got)
	}
	assertServesFixture(t, store, f, f.R.Prefix, storetest.E1, true)
	cleanup()
	if got := tempEntries(t, tmp); len(got) != 0 {
		t.Fatalf("init cleanup left %v in the temp directory", got)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if store, cleanup, err := indexRepoForInit(canceled, f.R.Root, zap.NewNop()); err == nil {
		cleanup()
		t.Fatalf("init index under a canceled context returned store %T and no error", store)
	}
	if got := tempEntries(t, tmp); len(got) != 0 {
		t.Fatalf("a failed init index left %v in the temp directory", got)
	}
}

func requireK12SQLiteStore(t *testing.T, store graph.Store) {
	t.Helper()
	if opened, ok := store.(*store_sqlite.Store); !ok || opened == nil {
		t.Fatalf("sqlite case opened %T, want a SQLite store", store)
	}
}

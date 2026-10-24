package main

import (
	"context"
	"github.com/dgraph-io/badger/v4"
	"gxstudy/conformance"
	"path/filepath"
	"sync"
	"testing"
)

func checkBadgerBudget(t *testing.T, want int) {
	t.Helper()
	badgerCaches.Lock()
	defer badgerCaches.Unlock()
	if len(badgerCaches.stores) != want {
		t.Fatalf("open handles=%d want=%d", len(badgerCaches.stores), want)
	}
	var total int64
	for s := range badgerCaches.stores {
		for _, kind := range []badger.CacheType{badger.BlockCache, badger.IndexCache} {
			cost, e := s.db.CacheMaxCost(kind, -1)
			if e != nil {
				t.Fatal(e)
			}
			if cost != servingCacheBytes/int64(want)/2 {
				t.Fatalf("cache cost %d", cost)
			}
			total += cost
		}
	}
	if total > servingCacheBytes {
		t.Fatalf("cache budget exceeded: %d", total)
	}
}
func TestSharedCacheLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph")
	if e := buildKV(context.Background(), path, conformance.Fixture()); e != nil {
		t.Fatal(e)
	}
	checkBadgerBudget(t, 0)
	var stores []kvStore
	for range 3 {
		s, e := openKV(path)
		if e != nil {
			t.Fatal(e)
		}
		defer s.Close()
		stores = append(stores, s)
		checkBadgerBudget(t, len(stores))
		opt := s.(*badgerKV).db.Opts()
		if !opt.ReadOnly || opt.NumMemtables != 0 || opt.MemTableSize != 32<<10 || opt.ValueThreshold != 0 || opt.NumCompactors != 0 {
			t.Fatal("unexpected read-only options", opt)
		}
	}
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			s, e := openKV(path)
			if e != nil {
				t.Error(e)
				return
			}
			if _, e = s.Get(key("N", "a")); e != nil {
				t.Error(e)
			}
			if e = s.Close(); e != nil {
				t.Error(e)
			}
		})
	}
	wg.Wait()
	checkBadgerBudget(t, 3)
	if _, e := openKV(filepath.Join(path, "missing")); e == nil {
		t.Fatal("missing DB opened")
	}
	checkBadgerBudget(t, 3)
	for i, s := range stores {
		if e := s.Close(); e != nil {
			t.Fatal(e)
		}
		checkBadgerBudget(t, len(stores)-i-1)
	}
}

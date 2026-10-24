package main

import (
	"context"
	"errors"
	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/vfs"
	"gxstudy/adapter"
	"gxstudy/conformance"
	"path/filepath"
	"sync"
	"testing"
)

func TestSharedCacheLifecycle(t *testing.T) {
	ctx := context.Background()
	b := backend{}
	dir := t.TempDir()
	var refs []adapter.SegmentRef
	for _, name := range []string{"one", "two"} {
		ref, _, e := b.Build(ctx, conformance.Fixture(), filepath.Join(dir, name))
		if e != nil {
			t.Fatal(e)
		}
		refs = append(refs, ref)
	}
	if pebbleCaches.users != 0 || pebbleCaches.cache != nil {
		t.Fatal("build retained shared cache")
	}
	var stores []adapter.Segment
	defer func() {
		for _, s := range stores {
			if e := s.Close(); e != nil {
				t.Error(e)
			}
		}
	}()
	for _, r := range refs {
		opened, e := b.Open(ctx, r.Path, r)
		if e != nil {
			t.Fatal(e)
		}
		stores = append(stores, opened)
		graph := opened.(*segment).kv.(*pebbleKV)
		if e := graph.db.Set([]byte("must-not-write"), []byte("x"), pebble.Sync); !errors.Is(e, pebble.ErrReadOnly) {
			t.Fatal("native write guard missing", e)
		}
	}
	if pebbleCaches.users != 2 || pebbleCaches.cache == nil {
		t.Fatal("cache was not shared")
	}
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			s, e := b.Open(ctx, refs[0].Path, refs[0])
			if e != nil {
				t.Error(e)
				return
			}
			if _, e = s.NodeByID(ctx, "a"); e != nil {
				t.Error(e)
			}
			if e = s.Close(); e != nil {
				t.Error(e)
			}
		})
	}
	wg.Wait()
	if pebbleCaches.users != 2 {
		t.Fatal("concurrent opener leaked shared cache")
	}
	for _, s := range stores {
		if e := s.Close(); e != nil {
			t.Fatal(e)
		}
	}
	stores = nil
	if pebbleCaches.users != 0 || pebbleCaches.cache != nil {
		t.Fatal("last close did not release shared cache")
	}
}
func TestWritableBuildKeepsNativeLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph")
	store, e := openPebble(path, pebble.Options{})
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	if e = buildKV(context.Background(), path, conformance.Fixture()); e == nil {
		t.Fatal("Build opened an already locked writable path")
	}
	if pebbleCaches.users != 1 {
		t.Fatal("failed writable open retained cache reference")
	}
	if _, e = openPebble(filepath.Join(path, "missing"), pebble.Options{ReadOnly: true}); e == nil {
		t.Fatal("missing DB opened")
	}
	if pebbleCaches.users != 1 {
		t.Fatal("failed read-only open retained cache reference")
	}
}

func TestSealedLockRejectsWritableOptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph")
	store, err := openPebble(path, pebble.Options{FS: sealedReadFS{FS: vfs.Default, lockPath: filepath.Join(path, "LOCK")}})
	if err == nil {
		store.Close()
		t.Fatal("writable option accepted with sealed lock override")
	}
	if pebbleCaches.users != 0 || pebbleCaches.cache != nil {
		t.Fatal("invalid options retained a cache")
	}
}

package main

import (
	"context"
	"errors"
	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/vfs"
	"gxstudy/adapter"
	"gxstudy/conformance"
	"gxstudy/dataset"
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
		graph := opened.(*segment).kv.(*scopedKV).stores["graph"]
		if e := graph.db.Set([]byte("must-not-write"), []byte("x"), pebble.Sync); !errors.Is(e, pebble.ErrReadOnly) {
			t.Fatal("native write guard missing", e)
		}
	}
	if pebbleCaches.users != 6 || pebbleCaches.cache == nil {
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
	if pebbleCaches.users != 6 {
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
	root := t.TempDir()
	path := filepath.Join(root, "graph")
	store, e := openPebble(path, pebble.Options{})
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	if e = buildKV(context.Background(), root, conformance.Fixture(), nil, nil); e == nil {
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

func TestPhysicalStoresKeepScopesAndOneSharedCache(t *testing.T) {
	b := backend{}
	ref, _, err := b.Build(context.Background(), conformance.Fixture(), filepath.Join(t.TempDir(), "split"))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := b.Open(context.Background(), ref.Path, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	stores := opened.(*segment).kv.(*scopedKV)
	for _, name := range []string{"graph", "text-records", "vectors"} {
		if stores.stores[name] == nil {
			t.Fatal("missing physical scope", name)
		}
		if err := stores.stores[name].Scan([]byte{}, func(key, value []byte) error {
			if recordScope(key) != name {
				t.Fatalf("prefix in wrong store: %q in %s", key, name)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := stores.stores[name].db.Set([]byte("write"), []byte("x"), pebble.Sync); !errors.Is(err, pebble.ErrReadOnly) {
			t.Fatal("store is writable", name, err)
		}
	}
	if pebbleCaches.users != 3 {
		t.Fatal("split stores did not share one cache", pebbleCaches.users)
	}
	var manifest sealManifest
	if err := dataset.ReadJSON(filepath.Join(ref.Path, "seal.json"), &manifest); err != nil {
		t.Fatal(err)
	}
	for _, file := range manifest.Files {
		if file.Category != artifactCategory(file.Name) {
			t.Fatal("wrong physical byte category", file)
		}
	}
}
func TestPartialSplitOpenReleasesEveryCacheReference(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"graph", "text-records"} {
		store, err := openPebble(filepath.Join(root, name), pebble.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := openStores(root); err == nil {
		t.Fatal("missing vector store accepted")
	}
	if pebbleCaches.users != 0 || pebbleCaches.cache != nil {
		t.Fatal("partial open leaked cache ownership")
	}
}

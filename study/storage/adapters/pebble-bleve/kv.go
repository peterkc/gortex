package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/vfs"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"gxstudy/runner"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const engineName = "pebble-bleve"
const engineVersion = "github.com/cockroachdb/pebble/v2 v2.1.7"

const engineCacheNotes = "KV cache policy: one shared 256 MiB Pebble cache across every open serving/build DB; OS page cache and Bleve memory are outside this cache budget. Sealed ReadOnly opens use a VFS lock override; writable Build keeps native locking."

var pebbleCaches struct {
	sync.Mutex
	cache *pebble.Cache
	users int
}

func acquirePebbleCache() *pebble.Cache {
	pebbleCaches.Lock()
	defer pebbleCaches.Unlock()
	if pebbleCaches.cache == nil {
		pebbleCaches.cache = pebble.NewCache(servingCacheBytes)
	}
	pebbleCaches.users++
	return pebbleCaches.cache
}
func releasePebbleCache() {
	pebbleCaches.Lock()
	defer pebbleCaches.Unlock()
	pebbleCaches.users--
	if pebbleCaches.users == 0 {
		pebbleCaches.cache.Unref()
		pebbleCaches.cache = nil
	}
}

type pebbleKV struct {
	db       *pebble.DB
	once     sync.Once
	closeErr error
}

func openPebble(path string, opts pebble.Options) (*pebbleKV, error) {
	if _, sealed := opts.FS.(sealedReadFS); sealed && !opts.ReadOnly {
		return nil, fmt.Errorf("sealed lock override requires ReadOnly")
	}
	opts.Cache = acquirePebbleCache()
	db, e := pebble.Open(path, &opts)
	if e != nil {
		releasePebbleCache()
		return nil, e
	}
	return &pebbleKV{db: db}, nil
}

// openKV is called only after backend.Open validates the immutable seal.
// Pebble otherwise takes an exclusive directory lock even for ReadOnly opens.
func openKV(path string) (kvStore, error) {
	return openPebble(path, pebble.Options{ReadOnly: true, FS: sealedReadFS{FS: vfs.Default, lockPath: filepath.Join(path, "LOCK")}})
}

type sealedReadFS struct {
	vfs.FS
	lockPath string
}
type sealedNoLock struct{}

func (sealedNoLock) Close() error { return nil }
func (f sealedReadFS) Lock(path string) (io.Closer, error) {
	if path != f.lockPath {
		return nil, fmt.Errorf("unexpected sealed lock path %q", path)
	}
	return sealedNoLock{}, nil
}
func (p *pebbleKV) Close() error {
	p.once.Do(func() { p.closeErr = p.db.Close(); releasePebbleCache() })
	return p.closeErr
}
func (p *pebbleKV) Get(k []byte) ([]byte, error) {
	v, c, e := p.db.Get(k)
	if errors.Is(e, pebble.ErrNotFound) {
		return nil, adapter.ErrNotFound
	}
	if e != nil {
		return nil, e
	}
	copy := bytes.Clone(v)
	return copy, c.Close()
}
func (p *pebbleKV) Scan(prefix []byte, visit func([]byte, []byte) error) (err error) {
	it, err := p.db.NewIter(&pebble.IterOptions{LowerBound: prefix})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, it.Close()) }()
	for ok := it.First(); ok && bytes.HasPrefix(it.Key(), prefix); ok = it.Next() {
		if err = visit(it.Key(), it.Value()); err != nil {
			return err
		}
	}
	return it.Error()
}

// Stores are physically separate so compressed SSTs, filters and metadata
// can be counted without estimating a key prefix's share of a mixed file.
type scopedKV struct{ stores map[string]*pebbleKV }

func recordScope(key []byte) string {
	if len(key) > 0 {
		switch key[0] {
		case 'S', 'C':
			return "text-records"
		case 'V':
			return "vectors"
		}
	}
	return "graph"
}
func (s *scopedKV) Get(key []byte) ([]byte, error) { return s.stores[recordScope(key)].Get(key) }
func (s *scopedKV) Scan(prefix []byte, visit func([]byte, []byte) error) error {
	return s.stores[recordScope(prefix)].Scan(prefix, visit)
}
func (s *scopedKV) Close() error {
	var err error
	for _, name := range []string{"vectors", "text-records", "graph"} {
		if db := s.stores[name]; db != nil {
			err = errors.Join(err, db.Close())
		}
	}
	return err
}
func openStores(path string) (kvStore, error) {
	stores := &scopedKV{stores: map[string]*pebbleKV{}}
	for _, name := range []string{"graph", "text-records", "vectors"} {
		opened, err := openKV(filepath.Join(path, name))
		if err != nil {
			return nil, errors.Join(err, stores.Close())
		}
		stores.stores[name] = opened.(*pebbleKV)
	}
	return stores, nil
}
func buildKV(ctx context.Context, path string, d dataset.Dataset, addSymbol, addContent func(string, string) error) (err error) {
	stores := &scopedKV{stores: map[string]*pebbleKV{}}
	defer func() { err = errors.Join(err, stores.Close()) }()
	batches := map[string]*pebble.Batch{}
	owned := map[string]os.FileInfo{}
	closeBatches := func() error {
		var err error
		for name, batch := range batches {
			err = errors.Join(err, batch.Close())
			delete(batches, name)
		}
		return err
	}
	defer func() { err = errors.Join(err, closeBatches()) }()
	for _, name := range []string{"graph", "text-records", "vectors"} {
		store, e := openPebble(filepath.Join(path, name), pebble.Options{DisableWAL: true})
		if e != nil {
			return e
		}
		stores.stores[name] = store
		identity, e := os.Lstat(filepath.Join(path, name))
		if e != nil {
			return e
		}
		owned[name] = identity
		batches[name] = store.db.NewBatch()
	}
	err = writeRecords(ctx, d, func(key, value []byte) error {
		batch := batches[recordScope(key)]
		if err := batch.Set(key, value, nil); err != nil {
			return err
		}
		if batch.Len() >= 4<<20 {
			if err := batch.Commit(pebble.NoSync); err != nil {
				return err
			}
			batch.Reset()
		}
		return nil
	}, addSymbol, addContent)
	if err != nil {
		return err
	}
	for _, name := range []string{"graph", "text-records", "vectors"} {
		if err = batches[name].Commit(pebble.NoSync); err != nil {
			return err
		}
		if err = stores.stores[name].db.Flush(); err != nil {
			return err
		}
	}
	if err = closeBatches(); err != nil {
		return err
	}
	if err = stores.Close(); err != nil {
		return err
	}
	for _, name := range []string{"graph", "text-records", "vectors"} {
		if err = runner.RemoveEmptyBuildWALs(filepath.Join(path, name), owned[name]); err != nil {
			return err
		}
	}
	return nil
}

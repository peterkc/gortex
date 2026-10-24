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
	"io"
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
func buildKV(ctx context.Context, path string, d dataset.Dataset) (err error) {
	store, err := openPebble(path, pebble.Options{})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, store.Close()) }()
	db := store.db
	batch := db.NewBatch()
	defer func() { err = errors.Join(err, batch.Close()) }()
	err = writeRecords(ctx, d, func(k, v []byte) error {
		if e := batch.Set(k, v, nil); e != nil {
			return e
		}
		if batch.Len() >= 4<<20 {
			if e := batch.Commit(pebble.Sync); e != nil {
				return e
			}
			batch.Reset()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err = batch.Commit(pebble.Sync); err != nil {
		return err
	}
	return db.Flush()
}

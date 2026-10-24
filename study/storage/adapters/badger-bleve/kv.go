package main

import (
	"context"
	"errors"
	"github.com/dgraph-io/badger/v4"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"sync"
)

const engineName = "badger-bleve"
const engineVersion = "github.com/dgraph-io/badger/v4 v4.9.6"

const engineCacheNotes = "KV cache policy: 256 MiB combined Badger block+index max-cost capacity across every open serving/build DB, split 128 MiB/N per cache per handle and rebalanced on open/close. Read-only: no writable memtables, NumMemtables=0, MemTableSize=32 KiB, ValueThreshold=0, NumCompactors=0. Cache metadata, OS page cache and Bleve memory are outside the max-cost budget."

var badgerCaches = struct {
	sync.Mutex
	stores map[*badgerKV]struct{}
}{stores: make(map[*badgerKV]struct{})}

type badgerKV struct {
	db       *badger.DB
	once     sync.Once
	closeErr error
}

func resizeBadgerCaches(handles int) error {
	if handles == 0 {
		return nil
	}
	share := servingCacheBytes / int64(handles) / 2
	for store := range badgerCaches.stores {
		if _, e := store.db.CacheMaxCost(badger.BlockCache, share); e != nil {
			return e
		}
		if _, e := store.db.CacheMaxCost(badger.IndexCache, share); e != nil {
			return e
		}
	}
	return nil
}
func openBadger(opts badger.Options) (*badgerKV, error) {
	badgerCaches.Lock()
	defer badgerCaches.Unlock()
	n := len(badgerCaches.stores) + 1
	// Shrink existing capacities before adding the new handle.
	if e := resizeBadgerCaches(n); e != nil {
		return nil, e
	}
	share := servingCacheBytes / int64(n) / 2
	db, e := badger.Open(opts.WithBlockCacheSize(share).WithIndexCacheSize(share))
	if e != nil {
		return nil, errors.Join(e, resizeBadgerCaches(n-1))
	}
	store := &badgerKV{db: db}
	badgerCaches.stores[store] = struct{}{}
	return store, nil
}

func openKV(path string) (kvStore, error) {
	opts := badger.DefaultOptions(path).WithReadOnly(true).WithLogger(nil).
		WithNumMemtables(0).WithMemTableSize(32 << 10).WithValueThreshold(0).WithNumCompactors(0)
	return openBadger(opts)
}
func (p *badgerKV) Close() error {
	p.once.Do(func() {
		badgerCaches.Lock()
		defer badgerCaches.Unlock()
		p.closeErr = p.db.Close()
		delete(badgerCaches.stores, p)
		p.closeErr = errors.Join(p.closeErr, resizeBadgerCaches(len(badgerCaches.stores)))
	})
	return p.closeErr
}
func (p *badgerKV) Get(k []byte) ([]byte, error) {
	var v []byte
	e := p.db.View(func(tx *badger.Txn) error {
		item, e := tx.Get(k)
		if e != nil {
			return e
		}
		v, e = item.ValueCopy(nil)
		return e
	})
	if errors.Is(e, badger.ErrKeyNotFound) {
		return nil, adapter.ErrNotFound
	}
	return v, e
}
func (p *badgerKV) Scan(prefix []byte, visit func([]byte, []byte) error) error {
	return p.db.View(func(tx *badger.Txn) error {
		opt := badger.DefaultIteratorOptions
		opt.Prefix = prefix
		it := tx.NewIterator(opt)
		defer it.Close()
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			if e := item.Value(func(v []byte) error { return visit(item.Key(), v) }); e != nil {
				return e
			}
		}
		return nil
	})
}
func buildKV(ctx context.Context, path string, d dataset.Dataset) (err error) {
	store, err := openBadger(badger.DefaultOptions(path).WithSyncWrites(true).WithLogger(nil))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, store.Close()) }()
	db := store.db
	batch := db.NewWriteBatch()
	defer batch.Cancel()
	if err = writeRecords(ctx, d, batch.Set); err != nil {
		return err
	}
	if err = batch.Flush(); err != nil {
		return err
	}
	return db.Sync()
}

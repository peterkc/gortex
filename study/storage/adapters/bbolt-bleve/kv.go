package main

import (
	"bytes"
	"context"
	"errors"
	bolt "go.etcd.io/bbolt"
	"gxstudy/adapter"
	"gxstudy/dataset"
)

const engineName = "bbolt-bleve"
const engineVersion = "go.etcd.io/bbolt v1.5.0"

const engineCacheNotes = "KV cache policy: bbolt uses mmap and the OS page cache, with no application block cache. The common 256 MiB cache budget is not allocated by this engine. Bleve memory and OS page cache are not capped by it."

var bucket = []byte("records")

type boltKV struct{ db *bolt.DB }

func openKV(path string) (kvStore, error) {
	db, e := bolt.Open(path, 0600, &bolt.Options{ReadOnly: true})
	if e != nil {
		return nil, e
	}
	return &boltKV{db: db}, nil
}
func (p *boltKV) Close() error { return p.db.Close() }
func (p *boltKV) Get(k []byte) ([]byte, error) {
	var v []byte
	e := p.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return adapter.ErrNotFound
		}
		raw := b.Get(k)
		if raw == nil {
			return adapter.ErrNotFound
		}
		v = bytes.Clone(raw)
		return nil
	})
	return v, e
}
func (p *boltKV) Scan(prefix []byte, visit func([]byte, []byte) error) error {
	return p.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return adapter.ErrNotFound
		}
		c := b.Cursor()
		for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			if e := visit(k, v); e != nil {
				return e
			}
		}
		return nil
	})
}
func buildKV(ctx context.Context, path string, d dataset.Dataset) (err error) {
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	tx, err := db.Begin(true)
	if err != nil {
		return err
	}
	defer func() {
		if tx != nil {
			err = errors.Join(err, tx.Rollback())
		}
	}()
	b, err := tx.CreateBucket(bucket)
	if err != nil {
		return err
	}
	n := 0
	err = writeRecords(ctx, d, func(k, v []byte) error {
		if e := b.Put(k, v); e != nil {
			return e
		}
		n++
		if n >= 1000 {
			e := tx.Commit()
			tx = nil
			if e != nil {
				return e
			}
			tx, e = db.Begin(true)
			if e != nil {
				return e
			}
			b = tx.Bucket(bucket)
			n = 0
		}
		return nil
	})
	if err != nil {
		return err
	}
	err = tx.Commit()
	tx = nil
	return err
}

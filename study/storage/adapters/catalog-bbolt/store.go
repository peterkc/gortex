package main

import (
	"context"
	"encoding/json"
	bolt "go.etcd.io/bbolt"
	"gxstudy/adapter"
	"os"
	"path/filepath"
	"time"
)

type boltStore struct{ db *bolt.DB }

func openStore(dir string) (storage, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	db, err := bolt.Open(filepath.Join(dir, "catalog.bbolt"), 0600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{"current", "previous"} {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &boltStore{db: db}, nil
}
func (s *boltStore) close() error { return s.db.Close() }
func (s *boltStore) point() string {
	return "bbolt Tx.Commit: atomic meta-page publication after data sync (NoSync=false)"
}
func readBolt(ctx context.Context, tx *bolt.Tx, repos []string) (state, error) {
	st := emptyState()
	for name, dest := range map[string]adapter.View{"current": st.Current, "previous": st.Previous} {
		bucket := tx.Bucket([]byte(name))
		decode := func(key, value []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if value == nil {
				return nil
			}
			var ref adapter.SegmentRef
			if err := json.Unmarshal(value, &ref); err != nil {
				return err
			}
			dest[string(key)] = ref
			return nil
		}
		if repos == nil {
			if err := bucket.ForEach(decode); err != nil {
				return st, err
			}
			continue
		}
		for _, repo := range repos {
			if err := decode([]byte(repo), bucket.Get([]byte(repo))); err != nil {
				return st, err
			}
		}
	}
	return st, nil
}
func (s *boltStore) read(ctx context.Context, repos []string) (state, error) {
	var st state
	err := s.db.View(func(tx *bolt.Tx) error { var err error; st, err = readBolt(ctx, tx, repos); return err })
	return st, err
}
func (s *boltStore) commit(ctx context.Context, set, old adapter.View) error {
	tx, err := s.db.Begin(true)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := readBolt(ctx, tx, keys(set))
	if err != nil {
		return err
	}
	st, err = applySet(st, set, old)
	if err != nil {
		return err
	}
	adapter.Checkpoint("before-write")
	for name, view := range map[string]adapter.View{"current": st.Current, "previous": st.Previous} {
		for repo, ref := range view {
			data, err := json.Marshal(ref)
			if err != nil {
				return err
			}
			if err = tx.Bucket([]byte(name)).Put([]byte(repo), data); err != nil {
				return err
			}
		}
	}
	adapter.Checkpoint("after-write")
	// Dirty pages, data sync and meta sync are indivisible through the public API.
	adapter.Checkpoint("after-sync")
	adapter.Checkpoint("before-rename")
	if err = tx.Commit(); err != nil {
		return err
	}
	adapter.Checkpoint("after-rename")
	adapter.Checkpoint("after-dirsync")
	return nil
}

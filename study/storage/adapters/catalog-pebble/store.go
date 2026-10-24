package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/cockroachdb/pebble/v2"
	"gxstudy/adapter"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type pebbleStore struct {
	db     *pebble.DB
	writer sync.Mutex
}

func openStore(dir string) (storage, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	db, err := pebble.Open(filepath.Join(dir, "catalog.pebble"), &pebble.Options{DisableWAL: false})
	if err != nil {
		return nil, err
	}
	return &pebbleStore{db: db}, nil
}
func (s *pebbleStore) close() error { return s.db.Close() }
func (s *pebbleStore) point() string {
	return "Pebble atomic WriteBatch.Commit(pebble.Sync), WAL enabled"
}
func (s *pebbleStore) read(ctx context.Context, repos []string) (state, error) {
	st := emptyState()
	snapshot := s.db.NewSnapshot()
	defer snapshot.Close()
	for prefix, dest := range map[string]adapter.View{"c/": st.Current, "p/": st.Previous} {
		if repos == nil {
			it, err := snapshot.NewIter(&pebble.IterOptions{LowerBound: []byte(prefix), UpperBound: []byte(prefix[:1] + "0")})
			if err != nil {
				return st, err
			}
			for it.First(); it.Valid(); it.Next() {
				if err = ctx.Err(); err != nil {
					it.Close()
					return st, err
				}
				var ref adapter.SegmentRef
				if err = json.Unmarshal(it.Value(), &ref); err != nil {
					it.Close()
					return st, err
				}
				dest[strings.TrimPrefix(string(it.Key()), prefix)] = ref
			}
			err = errors.Join(it.Error(), it.Close())
			if err != nil {
				return st, err
			}
			continue
		}
		for _, repo := range repos {
			if err := ctx.Err(); err != nil {
				return st, err
			}
			value, closer, err := snapshot.Get([]byte(prefix + repo))
			if errors.Is(err, pebble.ErrNotFound) {
				continue
			}
			if err != nil {
				return st, err
			}
			var ref adapter.SegmentRef
			err = json.Unmarshal(value, &ref)
			closeErr := closer.Close()
			if err != nil {
				return st, err
			}
			if closeErr != nil {
				return st, closeErr
			}
			dest[repo] = ref
		}
	}
	return st, nil
}
func (s *pebbleStore) commit(ctx context.Context, set, old adapter.View) error {
	// Pebble excludes other DB processes; this mutex serializes CAS within its owner.
	s.writer.Lock()
	defer s.writer.Unlock()
	st, err := s.read(ctx, keys(set))
	if err != nil {
		return err
	}
	st, err = applySet(st, set, old)
	if err != nil {
		return err
	}
	batch := s.db.NewBatch()
	defer batch.Close()
	adapter.Checkpoint("before-write")
	for prefix, view := range map[string]adapter.View{"c/": st.Current, "p/": st.Previous} {
		for repo, ref := range view {
			data, err := json.Marshal(ref)
			if err != nil {
				return err
			}
			if err = batch.Set([]byte(prefix+repo), data, nil); err != nil {
				return err
			}
		}
	}
	adapter.Checkpoint("after-write")
	// WAL write, sync and atomic visibility are owned by Batch.Commit.
	adapter.Checkpoint("after-sync")
	adapter.Checkpoint("before-rename")
	if err = batch.Commit(pebble.Sync); err != nil {
		return err
	}
	adapter.Checkpoint("after-rename")
	adapter.Checkpoint("after-dirsync")
	return nil
}

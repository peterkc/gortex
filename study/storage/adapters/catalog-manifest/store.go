package main

import (
	"context"
	"encoding/json"
	"errors"
	"gxstudy/adapter"
	"os"
	"path/filepath"
	"sync"
)

type manifestStore struct {
	dir    string
	writer sync.Mutex
}

func openStore(dir string) (storage, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	return &manifestStore{dir: dir}, nil
}
func (s *manifestStore) close() error { return nil }
func (s *manifestStore) point() string {
	return "atomic rename of manifest.json; file fsync before and directory fsync after"
}
func (s *manifestStore) read(ctx context.Context, _ []string) (state, error) {
	st := emptyState()
	if err := ctx.Err(); err != nil {
		return st, err
	}
	data, err := os.ReadFile(filepath.Join(s.dir, "manifest.json"))
	if os.IsNotExist(err) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(data, &st)
	return st, err
}
func (s *manifestStore) commit(ctx context.Context, set, old adapter.View) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	lock, err := lockWriter(ctx, s.dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	st, err := s.read(ctx, nil)
	if err != nil {
		return err
	}
	st, err = applySet(st, set, old)
	if err != nil {
		return err
	}
	adapter.Checkpoint("before-write")
	f, err := os.CreateTemp(s.dir, "manifest-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = json.NewEncoder(f).Encode(st); err != nil {
		f.Close()
		return err
	}
	adapter.Checkpoint("after-write")
	err = errors.Join(f.Sync(), f.Close())
	if err != nil {
		return err
	}
	adapter.Checkpoint("after-sync")
	adapter.Checkpoint("before-rename")
	if err = os.Rename(f.Name(), filepath.Join(s.dir, "manifest.json")); err != nil {
		return err
	}
	adapter.Checkpoint("after-rename")
	if err = syncDir(s.dir); err != nil {
		return err
	}
	adapter.Checkpoint("after-dirsync")
	return nil
}

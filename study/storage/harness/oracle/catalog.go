package oracle

import (
	"context"
	"encoding/json"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

type catalogState struct {
	Current  adapter.View `json:"current"`
	Previous adapter.View `json:"previous"`
}
type Catalog struct {
	dir    string
	mu     sync.Mutex
	verify func(adapter.SegmentRef) error
}

func NewCatalog(dir string) (adapter.Catalog, error) {
	return NewCatalogWithVerifier(dir, VerifyRef)
}

// NewCatalogWithVerifier keeps reference publication independent of segment format.
func NewCatalogWithVerifier(dir string, verify func(adapter.SegmentRef) error) (adapter.Catalog, error) {
	if verify == nil {
		return nil, fmt.Errorf("catalog requires a segment verifier")
	}
	if e := os.MkdirAll(dir, 0755); e != nil {
		return nil, e
	}
	return &Catalog{dir: dir, verify: verify}, nil
}
func (c *Catalog) read() (catalogState, error) {
	s := catalogState{Current: adapter.View{}, Previous: adapter.View{}}
	e := dataset.ReadJSON(filepath.Join(c.dir, "state.json"), &s)
	if os.IsNotExist(e) {
		return s, nil
	}
	return s, e
}
func (c *Catalog) Publish(ctx context.Context, repo string, n adapter.SegmentRef, old *adapter.SegmentRef) error {
	expect := adapter.View{}
	if old != nil {
		expect[repo] = *old
	}
	return c.PublishSet(ctx, adapter.View{repo: n}, expect)
}
func (c *Catalog) PublishSet(ctx context.Context, set, old adapter.View) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	lock, e := os.OpenFile(filepath.Join(c.dir, "lock"), os.O_CREATE|os.O_RDWR, 0644)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); e != nil {
		return e
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	s, e := c.read()
	if e != nil {
		return e
	}
	for r, n := range set {
		if r != n.Repo {
			return fmt.Errorf("catalog repository mismatch")
		}
		if s.Current[r] != old[r] {
			return adapter.ErrConflict
		}
		if e = c.verify(n); e != nil {
			return e
		}
	}
	for r, n := range set {
		s.Previous[r] = s.Current[r]
		s.Current[r] = n
	}
	adapter.Checkpoint("before-write")
	f, e := os.CreateTemp(c.dir, "state-*.tmp")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if e = json.NewEncoder(f).Encode(s); e != nil {
		f.Close()
		return e
	}
	adapter.Checkpoint("after-write")
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	adapter.Checkpoint("after-sync")
	adapter.Checkpoint("before-rename")
	if e = os.Rename(tmp, filepath.Join(c.dir, "state.json")); e != nil {
		return e
	}
	adapter.Checkpoint("after-rename")
	d, e := os.Open(c.dir)
	if e != nil {
		return e
	}
	e = d.Sync()
	d.Close()
	if e != nil {
		return e
	}
	adapter.Checkpoint("after-dirsync")
	return nil
}
func (c *Catalog) Pin(ctx context.Context, repos []string) (adapter.View, func(), error) {
	if e := ctx.Err(); e != nil {
		return nil, nil, e
	}
	s, e := c.read()
	if e != nil {
		return nil, nil, e
	}
	v := adapter.View{}
	for _, r := range repos {
		n, ok := s.Current[r]
		if !ok {
			return nil, nil, adapter.ErrNotFound
		}
		v[r] = n
	}
	return v, func() {}, nil
}
func (c *Catalog) Previous(ctx context.Context, repo string) (adapter.SegmentRef, error) {
	if e := ctx.Err(); e != nil {
		return adapter.SegmentRef{}, e
	}
	s, e := c.read()
	if e != nil {
		return adapter.SegmentRef{}, e
	}
	r, ok := s.Previous[repo]
	if !ok || r.Version == "" {
		return r, adapter.ErrNotFound
	}
	return r, nil
}
func (c *Catalog) Recover(ctx context.Context) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	s, e := c.read()
	if e != nil {
		return e
	}
	for _, r := range s.Current {
		if e = c.verify(r); e != nil {
			return e
		}
	}
	return nil
}
func (c *Catalog) Close() error { return nil }

func (c *Catalog) CommitPoint() string { return "atomic rename of state.json" }

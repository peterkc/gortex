package main

import (
	"context"
	"errors"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/oracle"
	"os"
	"path/filepath"
	"sync"
)

// lifecycle is a single-process study owner. It adds real pins and retirement to
// the harness's durable guarded reference catalog. It is not a daemon catalog.
type lifecycle struct {
	mu   sync.Mutex
	root string
	b    backend
	cat  adapter.Catalog
	pins map[string]int
}

func newLifecycle(root string, b backend) (*lifecycle, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(abs, 0700); err != nil {
		return nil, err
	}
	c, err := oracle.NewCatalogWithVerifier(filepath.Join(abs, "catalog"), func(ref adapter.SegmentRef) error {
		s, e := b.Open(context.Background(), ref.Path, ref)
		if e != nil {
			return e
		}
		return s.Close()
	})
	if err != nil {
		return nil, err
	}
	return &lifecycle{root: abs, b: b, cat: c, pins: map[string]int{}}, nil
}
func (l *lifecycle) close() error { return l.cat.Close() }
func (l *lifecycle) publish(ctx context.Context, ref adapter.SegmentRef, old *adapter.SegmentRef) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	refs, err := ancestors(l.b, ref)
	if err != nil {
		return err
	}
	for _, ancestor := range refs {
		if _, err := os.Stat(filepath.Join(l.root, "retired-"+ancestor.Digest)); err == nil {
			return fmt.Errorf("retirement fence")
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return l.cat.Publish(ctx, ref.Repo, ref, old)
}
func ancestors(b backend, ref adapter.SegmentRef) ([]adapter.SegmentRef, error) {
	var out []adapter.SegmentRef
	seen := map[string]bool{}
	for {
		if seen[ref.Path] {
			return nil, fmt.Errorf("ancestor cycle")
		}
		seen[ref.Path] = true
		out = append(out, ref)
		s, err := b.readSeal(ref)
		if err != nil {
			return nil, err
		}
		if s.Parent == nil {
			return out, nil
		}
		ref = *s.Parent
	}
}
func (l *lifecycle) pin(ctx context.Context, repo string) (adapter.Segment, func() error, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	v, release, err := l.cat.Pin(ctx, []string{repo})
	if err != nil {
		return nil, nil, err
	}
	defer release()
	refs, err := ancestors(l.b, v[repo])
	if err != nil {
		return nil, nil, err
	}
	s, err := l.b.Open(ctx, v[repo].Path, v[repo])
	if err != nil {
		return nil, nil, err
	}
	for _, ref := range refs {
		l.pins[ref.Path]++
	}
	var once sync.Once
	var closeErr error
	done := func() error {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			closeErr = s.Close()
			for _, ref := range refs {
				l.pins[ref.Path]--
			}
		})
		return closeErr
	}
	return s, done, nil
}
func (l *lifecycle) retire(ctx context.Context, ref adapter.SegmentRef) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	rel, err := filepath.Rel(l.root, ref.Path)
	if err != nil || !filepath.IsLocal(rel) || rel == "." {
		return fmt.Errorf("unowned retirement path")
	}
	for _, repoRef := range []func() (adapter.SegmentRef, error){func() (adapter.SegmentRef, error) {
		v, release, e := l.cat.Pin(ctx, []string{ref.Repo})
		if e != nil {
			return adapter.SegmentRef{}, e
		}
		defer release()
		return v[ref.Repo], nil
	}, func() (adapter.SegmentRef, error) { return l.cat.Previous(ctx, ref.Repo) }} {
		root, e := repoRef()
		if errors.Is(e, adapter.ErrNotFound) {
			continue
		}
		if e != nil {
			return e
		}
		refs, e := ancestors(l.b, root)
		if e != nil {
			return e
		}
		for _, r := range refs {
			if r.Path == ref.Path {
				return fmt.Errorf("referenced version")
			}
		}
	}
	if l.pins[ref.Path] > 0 {
		return fmt.Errorf("pinned version")
	}
	if _, err = l.b.readSeal(ref); err != nil {
		return err
	}
	fence := filepath.Join(l.root, "retired-"+ref.Digest)
	if err = durableJSON(fence, ref); err != nil {
		return err
	}
	if err = syncDir(l.root); err != nil {
		return err
	}
	// Only this owner's exact, validated version directory is removed.
	return os.RemoveAll(ref.Path)
}

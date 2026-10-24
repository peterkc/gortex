package main

import (
	"context"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/oracle"
	"gxstudy/runner"
)

const candidate = "catalog-bbolt"

type backend struct{ oracle.Backend }

func (backend) Name() string { return candidate }
func (backend) Capabilities() adapter.Capabilities {
	c := (oracle.Backend{}).Capabilities()
	c.Operations["catalog"] = candidate
	c.Libraries = []string{"go.etcd.io/bbolt v1.5.0"}
	c.Notes = append(c.Notes, "Catalog-only candidate. Serving uses the oracle, including exact vector scan; no serving-engine performance claim.", "Pins retain versions indefinitely because this study has no segment or catalog-generation garbage collection.")
	return c
}

type state struct {
	Current  adapter.View `json:"current"`
	Previous adapter.View `json:"previous"`
}

func emptyState() state { return state{Current: adapter.View{}, Previous: adapter.View{}} }

// The catalog owns policy; each engine owns its transaction and snapshot boundary.
type storage interface {
	read(context.Context, []string) (state, error)
	commit(context.Context, adapter.View, adapter.View) error
	close() error
	point() string
}
type catalog struct{ store storage }

func factory(dir string) (adapter.Catalog, error) {
	s, err := openStore(dir)
	if err != nil {
		return nil, err
	}
	return &catalog{store: s}, nil
}
func (c *catalog) CommitPoint() string { return c.store.point() }
func (c *catalog) Close() error        { return c.store.close() }
func (c *catalog) Publish(ctx context.Context, repo string, next adapter.SegmentRef, old *adapter.SegmentRef) error {
	expected := adapter.View{}
	if old != nil {
		expected[repo] = *old
	}
	return c.PublishSet(ctx, adapter.View{repo: next}, expected)
}
func (c *catalog) PublishSet(ctx context.Context, set, old adapter.View) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for repo, ref := range set {
		if repo == "" || ref.Repo != repo || ref.Version == "" || ref.Digest == "" || ref.Path == "" {
			return fmt.Errorf("invalid catalog ref for %q", repo)
		}
		if err := oracle.VerifyRef(ref); err != nil {
			return err
		}
	}
	return c.store.commit(ctx, set, old)
}
func (c *catalog) Pin(ctx context.Context, repos []string) (adapter.View, func(), error) {
	s, err := c.store.read(ctx, repos)
	if err != nil {
		return nil, nil, err
	}
	view := adapter.View{}
	for _, repo := range repos {
		ref, ok := s.Current[repo]
		if !ok {
			return nil, nil, adapter.ErrNotFound
		}
		view[repo] = ref
	}
	return view, func() {}, nil
}
func (c *catalog) Previous(ctx context.Context, repo string) (adapter.SegmentRef, error) {
	s, err := c.store.read(ctx, []string{repo})
	if err != nil {
		return adapter.SegmentRef{}, err
	}
	ref, ok := s.Previous[repo]
	if !ok {
		return ref, adapter.ErrNotFound
	}
	return ref, nil
}
func (c *catalog) Recover(ctx context.Context) error {
	s, err := c.store.read(ctx, nil)
	if err != nil {
		return err
	}
	for repo, ref := range s.Current {
		if ref.Repo != repo {
			return fmt.Errorf("recovery repository mismatch")
		}
		if err := oracle.VerifyRef(ref); err != nil {
			return err
		}
	}
	return nil
}
func keys(set adapter.View) []string {
	repos := make([]string, 0, len(set))
	for repo := range set {
		repos = append(repos, repo)
	}
	return repos
}

// applySet changes only owned, freshly decoded state, never the caller's maps.
func applySet(s state, set, expected adapter.View) (state, error) {
	for repo := range set {
		current, exists := s.Current[repo]
		old, wanted := expected[repo]
		if exists != wanted || (exists && current != old) {
			return state{}, adapter.ErrConflict
		}
	}
	for repo, ref := range set {
		if prior, exists := s.Current[repo]; exists {
			s.Previous[repo] = prior
		}
		s.Current[repo] = ref
	}
	return s, nil
}
func main() { b := backend{}; runner.Main(b, factory) }

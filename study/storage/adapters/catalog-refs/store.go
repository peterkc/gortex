package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"gxstudy/adapter"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type refEntry struct {
	Current  adapter.SegmentRef  `json:"current"`
	Previous *adapter.SegmentRef `json:"previous,omitempty"`
}
type route struct {
	Entry refEntry `json:"entry"`
	Group string   `json:"group,omitempty"`
	Token string   `json:"token,omitempty"`
}
type refsStore struct {
	dir    string
	writer sync.Mutex
}

func openStore(dir string) (storage, error) {
	for _, sub := range []string{"refs", "sets"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0755); err != nil {
			return nil, err
		}
	}
	return &refsStore{dir: dir}, nil
}
func (s *refsStore) close() error { return nil }
func (s *refsStore) point() string {
	return "standalone: repository ref rename; linked set: set CURRENT rename; both followed by directory fsync"
}
func refName(repo string) string { return hex.EncodeToString([]byte(repo)) + ".json" }

type observation struct {
	path   string
	data   []byte
	exists bool
}

func observe(path string) (observation, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return observation{path: path}, nil
	}
	return observation{path: path, data: data, exists: err == nil}, err
}
func (s *refsStore) read(ctx context.Context, repos []string) (state, error) {
	// Double collection, with non-reused group IDs, gives one coherent snapshot
	// while descriptors are prepared or a set pointer changes. No reader flock.
	for attempt := 0; attempt < 128; attempt++ {
		if err := ctx.Err(); err != nil {
			return state{}, err
		}
		st, observed, err := s.collect(ctx, repos)
		if err != nil {
			return st, err
		}
		unchanged := true
		for _, prior := range observed {
			next, err := observe(prior.path)
			if err != nil {
				return st, err
			}
			if prior.exists != next.exists || !bytes.Equal(prior.data, next.data) {
				unchanged = false
				break
			}
		}
		if unchanged {
			return st, nil
		}
	}
	return state{}, fmt.Errorf("catalog snapshot contention after 128 collections")
}
func (s *refsStore) collect(ctx context.Context, repos []string) (state, []observation, error) {
	st := emptyState()
	var observations []observation
	if repos == nil {
		entries, err := os.ReadDir(filepath.Join(s.dir, "refs"))
		if err != nil {
			return st, nil, err
		}
		repos = make([]string, 0, len(entries))
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			raw, err := hex.DecodeString(strings.TrimSuffix(entry.Name(), ".json"))
			if err != nil {
				return st, nil, err
			}
			repos = append(repos, string(raw))
		}
	}
	groups := map[string]observation{}
	for _, repo := range repos {
		if err := ctx.Err(); err != nil {
			return st, nil, err
		}
		o, err := observe(filepath.Join(s.dir, "refs", refName(repo)))
		if err != nil {
			return st, nil, err
		}
		observations = append(observations, o)
		if !o.exists {
			continue
		}
		var r route
		if err = json.Unmarshal(o.data, &r); err != nil {
			return st, nil, err
		}
		entry := r.Entry
		if r.Group != "" {
			// Group is an internal basename, never a caller-controlled path.
			if filepath.Base(r.Group) != r.Group || !strings.HasPrefix(r.Group, "set-") {
				return st, nil, fmt.Errorf("invalid set descriptor")
			}
			pointer, ok := groups[r.Group]
			if !ok {
				pointer, err = observe(filepath.Join(s.dir, "sets", r.Group, "CURRENT"))
				if err != nil {
					return st, nil, err
				}
				groups[r.Group] = pointer
				observations = append(observations, pointer)
			}
			if pointer.exists {
				var generation string
				if err = json.Unmarshal(pointer.data, &generation); err != nil {
					return st, nil, err
				}
				if generation != "generation" {
					return st, nil, fmt.Errorf("invalid generation")
				}
				data, err := os.ReadFile(filepath.Join(s.dir, "sets", r.Group, generation, refName(repo)))
				if err != nil {
					return st, nil, err
				}
				if err = json.Unmarshal(data, &entry); err != nil {
					return st, nil, err
				}
			}
		}
		if entry.Current.Version == "" {
			continue
		}
		st.Current[repo] = entry.Current
		if entry.Previous != nil {
			st.Previous[repo] = *entry.Previous
		}
	}
	return st, observations, nil
}
func entryFor(st state, repo string) refEntry {
	entry := refEntry{Current: st.Current[repo]}
	if prev, ok := st.Previous[repo]; ok {
		entry.Previous = &prev
	}
	return entry
}
func (s *refsStore) commit(ctx context.Context, set, old adapter.View) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	lock, err := lockWriter(ctx, s.dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	before, err := s.read(ctx, keys(set))
	if err != nil {
		return err
	}
	// applySet owns a separate map so the preparation fallback remains the old view.
	after := emptyState()
	for repo, ref := range before.Current {
		after.Current[repo] = ref
	}
	for repo, ref := range before.Previous {
		after.Previous[repo] = ref
	}
	after, err = applySet(after, set, old)
	if err != nil {
		return err
	}
	adapter.Checkpoint("before-write")
	log, err := os.OpenFile(filepath.Join(s.dir, "refs.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	// This is a durable prepare/audit log, not recovery authority. A crash may
	// leave an uncommitted or partial tail. Only canonical ref/CURRENT decides.
	if err = json.NewEncoder(log).Encode(struct {
		Set adapter.View `json:"set"`
	}{set}); err != nil {
		return err
	}
	if len(set) == 1 {
		for repo := range set {
			return s.single(repo, entryFor(after, repo), log)
		}
	}
	if len(set) == 0 {
		return fmt.Errorf("empty publication set")
	}
	groupDir, err := os.MkdirTemp(filepath.Join(s.dir, "sets"), "set-*")
	if err != nil {
		return err
	}
	generation := "generation"
	if err = os.Mkdir(filepath.Join(groupDir, generation), 0755); err != nil {
		return err
	}
	for repo := range set {
		if err = writeJSONFile(filepath.Join(groupDir, generation, refName(repo)), entryFor(after, repo)); err != nil {
			return err
		}
	}
	if err = syncDir(filepath.Join(groupDir, generation)); err != nil {
		return err
	}
	if err = syncDir(groupDir); err != nil {
		return err
	}
	if err = syncDir(filepath.Join(s.dir, "sets")); err != nil {
		return err
	}
	for repo := range set {
		r := route{Entry: entryFor(before, repo), Group: filepath.Base(groupDir)}
		// Absence is represented by a zero fallback until the set commit.
		if err = replaceJSON(filepath.Join(s.dir, "refs"), refName(repo), r); err != nil {
			return err
		}
	}
	f, err := os.CreateTemp(groupDir, ".CURRENT-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = json.NewEncoder(f).Encode(generation); err != nil {
		f.Close()
		return err
	}
	adapter.Checkpoint("after-write")
	err = errors.Join(log.Sync(), f.Sync(), f.Close())
	if err != nil {
		return err
	}
	adapter.Checkpoint("after-sync")
	adapter.Checkpoint("before-rename")
	if err = os.Rename(f.Name(), filepath.Join(groupDir, "CURRENT")); err != nil {
		return err
	}
	adapter.Checkpoint("after-rename")
	if err = syncDir(groupDir); err != nil {
		return err
	}
	adapter.Checkpoint("after-dirsync")
	return nil
}
func (s *refsStore) single(repo string, entry refEntry, log *os.File) error {
	dir := filepath.Join(s.dir, "refs")
	f, err := os.CreateTemp(dir, ".ref-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = json.NewEncoder(f).Encode(route{Entry: entry, Token: rand.Text()}); err != nil {
		f.Close()
		return err
	}
	adapter.Checkpoint("after-write")
	err = errors.Join(log.Sync(), f.Sync(), f.Close())
	if err != nil {
		return err
	}
	adapter.Checkpoint("after-sync")
	adapter.Checkpoint("before-rename")
	if err = os.Rename(f.Name(), filepath.Join(dir, refName(repo))); err != nil {
		return err
	}
	adapter.Checkpoint("after-rename")
	if err = syncDir(dir); err != nil {
		return err
	}
	adapter.Checkpoint("after-dirsync")
	return nil
}

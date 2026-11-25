package mcp

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/persistence"
	"github.com/zzet/gortex/internal/platform"
)

// SavedScope is a named, persisted set of repository prefixes — a reusable
// slice of a multi-repo workspace that query tools accept by name instead
// of re-specifying repo filters on every call.
type SavedScope struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Repos       []string `json:"repos"`
	// Paths narrows a scope below the repository grain to a set of
	// sub-paths -- the monorepo-service slice (services/billing,
	// libs/auth). Anchored, slash-segment-normalised prefixes
	// relative to each repo root. Empty (the back-compatible default)
	// means the scope is repo-level only -- an existing scopes.json
	// without this field decodes cleanly.
	Paths []string `json:"paths,omitempty"`
}

// scopeStore is a small registry of SavedScopes backed by the SQLite
// sidecar DB. It survives daemon restarts. Scopes are global (not
// repo-scoped). The in-memory byName map mirrors the scopes table so
// reads stay lock-cheap; mutations write through to the sidecar. All
// exported methods are safe for concurrent use.
type scopeStore struct {
	mu      sync.Mutex
	sidecar scopeRecords
	byName  map[string]SavedScope
}

// scopesFilePath returns the legacy on-disk location of the saved-scope
// store, honouring GORTEX_SCOPES_PATH (used by tests) over the cache
// default. The sidecar DB lives next to it (<dir>/sidecar.sqlite); a
// pre-existing scopes.json at this path is imported once, then renamed
// to scopes.json.bak.
func scopesFilePath() string {
	if p := strings.TrimSpace(os.Getenv("GORTEX_SCOPES_PATH")); p != "" {
		return p
	}
	return filepath.Join(platform.OSCacheDir(), "scopes.json")
}

// newScopeStore builds a store whose sidecar DB lives next to the given
// legacy scopes.json path. Any scopes.json present is imported once,
// then the in-memory map is hydrated from the sidecar. A nil sidecar
// (open failure) yields an in-memory-only store.
func newScopeStore(legacyPath string) *scopeStore {
	sidecarPath := persistence.DefaultSidecarPath(filepath.Dir(legacyPath))
	sidecar, _ := persistence.OpenSidecar(sidecarPath)
	return newScopeStoreFromSidecar(sidecar, legacyPath)
}

// newScopeStoreFromSidecar builds a scope store bound to an already-open
// sidecar, importing legacyPath/scopes.json once. Used by the daemon
// path where the sidecar is opened once and shared.
func newScopeStoreFromSidecar(sidecar *persistence.SidecarStore, legacyPath string) *scopeStore {
	st := &scopeStore{byName: map[string]SavedScope{}}
	if sidecar != nil {
		st.sidecar = sidecar
		_ = sidecar.MigrateLegacyScopes(legacyPath)
		if rows, err := sidecar.LoadScopes(); err == nil {
			for _, r := range rows {
				if r.Name != "" {
					st.byName[r.Name] = SavedScope{Name: r.Name, Description: r.Description, Repos: r.Repos, Paths: r.Paths}
				}
			}
		}
	}
	return st
}

func (st *scopeStore) get(name string) (SavedScope, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	sc, ok := st.byName[name]
	return sc, ok
}

func (st *scopeStore) list() []SavedScope {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]SavedScope, 0, len(st.byName))
	for _, sc := range st.byName {
		out = append(out, sc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (st *scopeStore) put(sc SavedScope) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.byName[sc.Name] = sc
	if st.sidecar == nil {
		return nil
	}
	return st.sidecar.UpsertScope(persistence.ScopeRow{
		Name: sc.Name, Description: sc.Description, Repos: sc.Repos, Paths: sc.Paths,
	})
}

func (st *scopeStore) remove(name string) (bool, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, ok := st.byName[name]; !ok {
		return false, nil
	}
	delete(st.byName, name)
	if st.sidecar == nil {
		return true, nil
	}
	return true, st.sidecar.DeleteScope(name)
}

// scopeStoreOrInit lazily constructs the per-server saved-scope store.
func (s *Server) scopeStoreOrInit() *scopeStore {
	s.scopesOnce.Do(func() {
		s.scopes = newScopeStore(scopesFilePath())
	})
	return s.scopes
}

// lookupScope returns the named saved scope.
func (s *Server) lookupScope(name string) (SavedScope, bool) {
	return s.scopeStoreOrInit().get(name)
}

// scopeRepoSet expands a saved scope into a repo-prefix allow-set.
func (s *Server) scopeRepoSet(sc SavedScope) map[string]bool {
	out := make(map[string]bool, len(sc.Repos))
	for _, r := range sc.Repos {
		if r = strings.TrimSpace(r); r != "" {
			out[r] = true
		}
	}
	return out
}

package mcp

import (
	"sort"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/persistence"
)

// demoteAfterSessions is the hysteresis window: a learned tool promotion
// unused for this many session epochs (server startups) is demoted back to
// the deferred catalogue. Small enough that a stale promotion clears out
// within a few sessions, large enough that an occasional-use tool sticks.
const demoteAfterSessions = 3

// promotedToolsManager is the per-workspace learned tool surface: tools
// that started deferred behind tools_search but were promoted into the
// eager surface after use, persisted so the learning survives daemon
// restarts. It advances a session epoch on load, demotes promotions unused
// for demoteAfterSessions epochs, and records fresh promotions/uses. A nil
// sidecar (no cache dir) degrades to an in-memory-only, non-persistent
// surface.
type promotedToolsManager struct {
	mu      sync.Mutex
	sidecar promotionRecords
	repoKey string
	epoch   int64
	live    map[string]persistence.PromotedToolEntry
}

func newPromotedToolsManager(cacheDir, repoPath string) *promotedToolsManager {
	m := &promotedToolsManager{live: map[string]persistence.PromotedToolEntry{}}
	if cacheDir == "" || repoPath == "" {
		return m
	}
	sidecar, err := persistence.OpenSidecar(persistence.DefaultSidecarPath(cacheDir))
	if err != nil || sidecar == nil {
		return m
	}
	m.sidecar = sidecar
	m.repoKey = persistence.RepoCacheKey(repoPath)
	return m
}

// Load advances the session epoch, demotes promotions unused past the
// hysteresis window, and returns the surviving tool names to re-promote
// into the eager surface. Call once at server startup.
func (m *promotedToolsManager) Load() []string {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sidecar == nil {
		return nil
	}
	epoch, err := m.sidecar.BumpSessionEpoch(m.repoKey)
	if err != nil {
		return nil
	}
	m.epoch = epoch
	rows, err := m.sidecar.LoadPromotedTools(m.repoKey)
	if err != nil {
		return nil
	}
	var survivors []string
	for _, e := range rows {
		if epoch-e.LastUsedEpoch >= demoteAfterSessions {
			_ = m.sidecar.DeletePromotedTool(m.repoKey, e.Tool) // demote
			continue
		}
		m.live[e.Tool] = e
		survivors = append(survivors, e.Tool)
	}
	sort.Strings(survivors)
	return survivors
}

// Record persists a tool promotion (or refreshes an existing one's
// last-used epoch, resetting its demotion clock). workspaceID is stored for
// diagnostics; the surface is keyed per repo.
func (m *promotedToolsManager) Record(tool, workspaceID string) {
	if m == nil || tool == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sidecar == nil {
		return
	}
	e, ok := m.live[tool]
	if !ok {
		e = persistence.PromotedToolEntry{Tool: tool, PromotedEpoch: m.epoch}
	}
	if workspaceID != "" {
		e.WorkspaceID = workspaceID
	}
	e.LastUsedEpoch = m.epoch
	e.UseCount++
	e.UpdatedAt = time.Now()
	m.live[tool] = e
	_ = m.sidecar.UpsertPromotedTool(m.repoKey, e)
}

// Has reports whether a tool is in the learned surface.
func (m *promotedToolsManager) Has(tool string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.live[tool]
	return ok
}

// Names returns the learned surface, sorted.
func (m *promotedToolsManager) Names() []string {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.live))
	for t := range m.live {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Count returns the size of the learned surface.
func (m *promotedToolsManager) Count() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.live)
}

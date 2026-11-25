package mcp

import (
	"sort"
	"sync"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/storage/backend"
)

// registeredToolNames is the process-cached result of RegisteredToolNames. The
// tool surface is fixed at build time, so a single throwaway server enumerates
// it once and every later caller reads the cached slice.
var (
	registeredToolNamesOnce sync.Once
	registeredToolNames     []string
)

// RegisteredToolNames returns the sorted names of every MCP tool the server
// registers — live and deferred — WITHOUT a running daemon. It builds one
// ephemeral in-memory server purely to walk the registration, then caches the
// result. Callers that only need to know "is this a real tool name" (the CLI
// did-you-mean recovery, guidance-shape regression tests) get the authoritative
// registry without a socket round-trip. Returns a fresh copy so callers can't
// mutate the cache.
func RegisteredToolNames() []string {
	registeredToolNamesOnce.Do(func() {
		st, _, err := backend.OpenRaw("sqlite", ":memory:")
		if err != nil {
			return
		}
		// The store is intentionally left open for the process lifetime — the
		// server may retain a reference and this runs once per process in the
		// CLI / test contexts that call it.
		srv := NewServer(query.NewEngine(st), st, nil, nil, zap.NewNop(), nil)
		descs := srv.ToolDescriptors()
		names := make([]string, 0, len(descs))
		for _, d := range descs {
			if d.Name != "" {
				names = append(names, d.Name)
			}
		}
		sort.Strings(names)
		registeredToolNames = names
	})
	out := make([]string, len(registeredToolNames))
	copy(out, registeredToolNames)
	return out
}

// IsRegisteredToolName reports whether name is one of the MCP tools the server
// registers. Daemon-free (see RegisteredToolNames).
func IsRegisteredToolName(name string) bool {
	if name == "" {
		return false
	}
	for _, n := range RegisteredToolNames() {
		if n == name {
			return true
		}
	}
	return false
}

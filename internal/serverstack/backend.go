// Package serverstack is the single construction path for Gortex's server
// stack: it builds graph.Store -> parser.Registry -> indexer -> query.Engine
// -> mcp.Server plus every side-effect init, so the daemon, the HTTP
// surface, and the one-shot embedded path all share one wiring instead of
// three near-identical copies.
//
// It lives in its own leaf package (not internal/daemon) because the
// constructor builds an *mcp.Server, and internal/mcp already imports
// internal/daemon one-way; hosting the constructor in internal/daemon
// would form the cycle daemon -> mcp -> daemon.
package serverstack

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/storage"
	"github.com/zzet/gortex/internal/storage/backend"
)

// OpenBackend constructs the graph.Store the server will run against. The
// pure-Go modernc.org/sqlite store is the only implementation: an empty
// name selects it, and it lives at the resolved path (an empty path means
// ~/.gortex/store/store.sqlite).
//
// Returns the store, a cleanup func the caller must defer (it closes the
// handle on disk), and any open error.
// allowRebuild permits rebuilding supported older schemas when necessary and
// backing an existing store up before an in-place schema upgrade; a schema
// newer than this binary is always refused and retained. The caller must
// hold the store lock. NewSharedServer passes true only after acquiring the
// exclusive flock.
func OpenBackend(name, path string, logger *zap.Logger, allowRebuild bool, observers ...storage.MigrationObserver) (graph.Store, func() error, error) {
	return openBackendWithUpgradeHook(name, path, logger, allowRebuild, backend.UpgradeHook{}, observers...)
}

func openBackendWithUpgradeHook(name, path string, logger *zap.Logger, allowRebuild bool, upgradeHook backend.UpgradeHook, observers ...storage.MigrationObserver) (graph.Store, func() error, error) {
	if err := checkBackend(name); err != nil {
		return nil, nil, err
	}
	resolved, err := resolveBackendPath(path, "store.sqlite")
	if err != nil {
		return nil, nil, err
	}
	if logger != nil {
		logger.Info("opening sqlite backend", zap.String("path", resolved))
	}
	options := backend.OpenOptions{AllowRebuild: allowRebuild, UpgradeHook: upgradeHook}
	if len(observers) > 0 {
		options.MigrationObserver = observers[0]
	}
	s, closeStore, err := backend.Open(name, resolved, options)
	if err != nil {
		return nil, nil, backendOpenError(resolved, err)
	}
	return s, closeStore, nil
}

// checkBackend validates a requested backend name. It is split out of
// OpenBackend so a caller that takes the cross-process store lock first can
// reject an unusable name before touching the lock or the filesystem.
func checkBackend(name string) error { return backend.CheckName(name) }

// resolveBackendPath turns an empty backend path into a default under the
// unified store directory (~/.gortex/store/<filename>, or the XDG_DATA_HOME
// equivalent). Otherwise expands ~ and returns the absolute path, creating
// the parent directory so the on-disk store can open the leaf.
func resolveBackendPath(in, filename string) (string, error) {
	in = strings.TrimSpace(in)
	if in == "" {
		in = filepath.Join(platform.StoreDir(), filename)
	} else if strings.HasPrefix(in, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		in = filepath.Join(home, in[2:])
	}
	abs, err := filepath.Abs(in)
	if err != nil {
		return "", fmt.Errorf("abs path %q: %w", in, err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", fmt.Errorf("mkdir parent %q: %w", filepath.Dir(abs), err)
	}
	return abs, nil
}

// ResolveStorePath applies the same path/default/parent rules as the daemon
// stack without opening SQLite. Detached startup needs this before exec so
// SQLITE_TMPDIR is present in SQLite's process-start environment.
func ResolveStorePath(path string) (string, error) {
	return resolveBackendPath(path, "store.sqlite")
}

func backendOpenError(path string, err error) error {
	if errors.Is(err, storage.ErrSchemaTooNew) || errors.Is(err, storage.ErrInsufficientUpgradeSpace) {
		// Restarting another process cannot make this binary compatible,
		// nor free the disk space an upgrade needs.
		return fmt.Errorf("open sqlite store at %q: %w", path, err)
	}
	hint := "if another gortex daemon is using this store, stop it first (`gortex daemon status` / `gortex daemon stop`)"
	if pid, ok := daemon.RunningPID(); ok {
		hint = fmt.Sprintf("a gortex daemon is already running (pid %d) — stop it with `gortex daemon stop`, or use `gortex daemon restart`", pid)
	}
	return fmt.Errorf("open sqlite store at %q: %w (%s)", path, err, hint)
}

type upgradeBackupPruner interface {
	PruneUpgradeBackups(path string, now time.Time) ([]string, error)
}

// pruneStoreUpgradeBackups removes the pre-upgrade backups of storePath that
// have outlived the backend's retention window at now, along with any
// partial copy an interrupted upgrade left behind. The caller must hold the
// store lock. Failures are logged, never fatal: a backup that cannot be
// removed costs disk space, not correctness.
func pruneStoreUpgradeBackups(name, storePath string, logger *zap.Logger, now time.Time) {
	pruner, ok := backend.Offline(name).(upgradeBackupPruner)
	if !ok {
		return
	}
	removed, err := pruner.PruneUpgradeBackups(storePath, now)
	for _, path := range removed {
		logger.Info("serverstack: removed expired store upgrade backup", zap.String("path", path))
	}
	if err != nil {
		logger.Warn("serverstack: could not prune store upgrade backups", zap.String("store", storePath), zap.Error(err))
	}
}

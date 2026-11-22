package store_pebble

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/vfs"
	"github.com/stretchr/testify/require"
)

const rebuildScratchFaultEnv = "GORTEX_TEST_REBUILD_SCRATCH_FAULT"

func TestRebuildScratchFailStop(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	for _, mode := range []string{"working", "pending"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, exe, "-test.run=^TestRebuildScratchFailStopChild$", "--", t.TempDir())
			cmd.Env = append(os.Environ(), rebuildScratchFaultEnv+"="+mode)
			out, err := cmd.CombinedOutput()
			require.NoError(t, ctx.Err(), "%s", out)
			var exit *exec.ExitError
			require.ErrorAs(t, err, &exit, "%s", out)
			require.Equal(t, 1, exit.ExitCode(), "%s", out)
			want := `rebuild build worker: Pebble scratch background error: "planted\nworking fault"`
			if mode == "pending" {
				want = "planted pending SST create failure"
				require.Contains(t, string(out), "rebuild build worker: Pebble scratch background error:")
			}
			require.Contains(t, string(out), want)
		})
	}
}

func TestRebuildScratchFailStopChild(t *testing.T) {
	mode := os.Getenv(rebuildScratchFaultEnv)
	if mode == "" {
		t.Skip("isolated Pebble failure fixture")
	}
	b, err := OpenWorkerBuild(filepath.Join(os.Args[len(os.Args)-1], "working"), DefaultProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	onError := b.options.EventListener.BackgroundError
	switch mode {
	case "working":
		onError(errors.New("planted\nworking fault"))
	case "pending":
		// Use the production pending options and a fixture-local FS. Only
		// SST creation fails, after Open succeeds and a row enters the memtable.
		opts := pendingOptions(onError)
		opts.FS = scratchFaultFS{FS: vfs.NewMem()}
		db, err := pebble.Open("pending", opts)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		require.NoError(t, db.Set([]byte("key"), []byte("value"), pebble.NoSync))
		require.NoError(t, db.Flush())
	}
	t.Fatal("scratch background error did not terminate the worker")
}

type scratchFaultFS struct{ vfs.FS }

func (fs scratchFaultFS) Create(name string, category vfs.DiskWriteCategory) (vfs.File, error) {
	if strings.HasSuffix(name, ".sst") {
		return nil, errors.New("planted pending SST create failure")
	}
	return fs.FS.Create(name, category)
}

package main

import (
	"os"

	"github.com/spf13/cobra"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/indexer"
)

const rebuildBuildWorkerCommand = "__rebuild-build-worker"

var rebuildBuildWorkerCmd = &cobra.Command{
	Use:               rebuildBuildWorkerCommand + " lease-dir",
	Args:              cobra.ExactArgs(1),
	Hidden:            true,
	SilenceUsage:      true,
	SilenceErrors:     true,
	PersistentPreRun:  skipWorkerHooks,
	PersistentPostRun: skipWorkerHooks,
	RunE: func(cmd *cobra.Command, args []string) error {
		link := store_pebble.RebuildLinkOptions{SpillBytes: 1 << 20}
		return indexer.RunRebuildBuildWorker(cmd.Context(), args[0], os.Stdin, os.Stdout, newLogger(), link)
	},
}

// Re-exec workers have private pipes and leases. They do not migrate or
// refresh user state, or record a user-facing command invocation.
func skipWorkerHooks(*cobra.Command, []string) {}

func init() {
	rootCmd.AddCommand(rebuildBuildWorkerCmd)
}

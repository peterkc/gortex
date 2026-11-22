package main

import (
	"os"
	"strconv"

	"github.com/spf13/cobra"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/indexer"
)

const rebuildBuildWorkerCommand = "__rebuild-build-worker"
const rebuildLinkWorkerCommand = "__rebuild-link-worker"

var rebuildBuildWorkerCmd = &cobra.Command{
	Use:               rebuildBuildWorkerCommand + " lease-dir",
	Args:              cobra.ExactArgs(1),
	Hidden:            true,
	SilenceUsage:      true,
	SilenceErrors:     true,
	PersistentPreRun:  skipWorkerHooks,
	PersistentPostRun: skipWorkerHooks,
	RunE: func(cmd *cobra.Command, args []string) error {
		return indexer.RunRebuildBuildWorker(cmd.Context(), args[0], os.Stdin, os.Stdout, newLogger())
	},
}

var rebuildLinkWorkerCmd = &cobra.Command{
	Use:               rebuildLinkWorkerCommand + " partition-path spill-bytes",
	Args:              cobra.ExactArgs(2),
	Hidden:            true,
	SilenceUsage:      true,
	SilenceErrors:     true,
	PersistentPreRun:  skipWorkerHooks,
	PersistentPostRun: skipWorkerHooks,
	RunE: func(cmd *cobra.Command, args []string) error {
		spill, err := strconv.Atoi(args[1])
		if err != nil {
			return err
		}
		return store_pebble.RunRebuildLinkWorker(cmd.Context(), args[0], spill, os.Stdin, os.Stdout)
	},
}

// Re-exec workers have private pipes and leases. They do not migrate or
// refresh user state, or record a user-facing command invocation.
func skipWorkerHooks(*cobra.Command, []string) {}

func init() {
	rootCmd.AddCommand(rebuildBuildWorkerCmd, rebuildLinkWorkerCmd)
}

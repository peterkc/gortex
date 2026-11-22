package main

import (
	"os"

	"github.com/spf13/cobra"
	"github.com/zzet/gortex/internal/indexer"
)

// parseWorkerCmd is the subprocess side of the crash-resilient parser
// pool. The indexer spawns `gortex __parse-worker` instances and feeds
// them files over stdin; a grammar fault here kills only this process,
// not the daemon. Hidden — never invoked by users directly.
var parseWorkerCmd = &cobra.Command{
	Use:               "__parse-worker",
	Short:             "Internal: isolated tree-sitter extraction worker",
	Hidden:            true,
	SilenceUsage:      true,
	SilenceErrors:     true,
	PersistentPreRun:  skipWorkerHooks,
	PersistentPostRun: skipWorkerHooks,
	RunE: func(_ *cobra.Command, _ []string) error {
		return indexer.RunRebuildParseWorker(os.Stdin, os.Stdout)
	},
}

func init() {
	rootCmd.AddCommand(parseWorkerCmd)
}

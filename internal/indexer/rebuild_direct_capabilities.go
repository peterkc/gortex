package indexer

import (
	"context"
	"iter"
	"strings"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// runRebuildDirectCapabilities is the direct-only control used by tests.
func runRebuildDirectCapabilities(ctx context.Context, build *store_pebble.BuildStore,
	receipts iter.Seq2[store_pebble.ParseFacts, error],
) error {
	return build.RebuildDirectCapabilities(ctx, receipts, processExecMechanism, rebuildProcessImport)
}

func rebuildReceiverRules() store_pebble.ReceiverMutationRules {
	return store_pebble.ReceiverMutationRules{
		OwnerKey: receiverOwnerKey, BareCallName: bareCallName, IsStdlibMutator: isStdlibMutator,
	}
}

// Only the structured extractor form can prove a call's import. The
// name matcher alone also accepts bare calls and third-party lookalikes.
func rebuildProcessImport(language, target string) (string, string) {
	name, ok := strings.CutPrefix(target, "unresolved::extern::")
	if !ok {
		return "", ""
	}
	path, symbol, ok := strings.Cut(name, "::")
	if !ok {
		return "", ""
	}
	switch language {
	case "go":
		switch path {
		case "os/exec":
			if symbol == "Command" || symbol == "CommandContext" {
				return path, symbol
			}
		case "os":
			if symbol == "StartProcess" {
				return path, symbol
			}
		case "syscall":
			if symbol == "Exec" || symbol == "ForkExec" {
				return path, symbol
			}
		}
	case "javascript", "typescript":
		if path == "child_process" && processExecAPIs[path+"."+symbol] != "" {
			return path, symbol
		}
	}
	return "", ""
}

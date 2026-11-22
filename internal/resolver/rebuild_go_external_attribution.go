package resolver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// ErrRebuildGoExternalRecordingPath marks a potential external-attribution
// site whose stored path contains a NUL. Do not assume SQLite's suffix result.
var ErrRebuildGoExternalRecordingPath = errors.New("Go external attribution recording path contains NUL")

func runRebuildGoExternalCalls(ctx context.Context, build *store_pebble.BuildStore) error {
	return build.RebuildGoExternalCalls(ctx, rebuildGoExternalNodes, goExternalModuleRoleRank)
}

func rebuildGoExternalNodes(e *graph.Edge) (*graph.Node, *graph.Node, error) {
	if _, ok := goExternalAttribKindSet[e.Kind]; !ok {
		return nil, nil, nil
	}
	beforeNUL, _, nul := strings.Cut(e.FilePath, "\x00")
	if !strings.HasSuffix(e.FilePath, ".go") && !(nul && strings.HasSuffix(beforeNUL, ".go")) {
		return nil, nil, nil
	}
	prefix, importPath, symbol := splitGoExternalTarget(e.To)
	if prefix == "" {
		return nil, nil, nil
	}
	if nul {
		return nil, nil, fmt.Errorf("%w: %q", ErrRebuildGoExternalRecordingPath, e.FilePath)
	}
	repo := graph.StubRepoPrefix(e.To)
	role := goExternalModuleRole(prefix)
	module := &graph.Node{
		ID:   graph.StubID(repo, graph.StubKindModule, "go:"+importPath),
		Kind: graph.KindModule, Name: lastImportSegment(importPath), Language: "go", RepoPrefix: repo,
		Meta: map[string]any{"ecosystem": "go", "role": role, "import_path": importPath},
	}
	if symbol == "" {
		return module, nil, nil
	}
	id, symbolRepo := prefix+importPath+"::"+symbol, ""
	if prefix == "stdlib::" {
		id, symbolRepo = graph.StubID(repo, graph.StubKindStdlib, importPath, symbol), repo
	}
	return module, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: symbol, Language: "go", RepoPrefix: symbolRepo,
		Meta: map[string]any{"external": true, "module_path": importPath, "module_role": role},
	}, nil
}

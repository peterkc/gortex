package indexer

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/modules"
)

var errRebuildWorkspaceOutput = errors.New("workspace graph output outside rebuild support")

// runRebuildPackageWorkspace is the module stage's workspace tail. It uses
// the same checked declarations and member-name evidence as the resolver.
func runRebuildPackageWorkspace(ctx context.Context, lease string, digest rebuildDigest, build *store_pebble.BuildStore) error {
	ns, npm, followed, err := openRebuildNpmManifestFacts(ctx, lease, digest)
	if err != nil {
		return err
	}
	manifest, err := openRebuildWorkspaceManifest(ctx, lease, digest, ns, npm["package.json"], followed)
	if err != nil || manifest == nil {
		return err
	}
	glob, err := newRebuildWorkspaceGlob(ctx, ns)
	if err != nil {
		return err
	}
	var members []modules.WorkspaceMember
	_, err = collectRebuildWorkspaceMembers(ctx, glob, manifest, func(member modules.WorkspaceMember) {
		members = append(members, member)
	})
	if err != nil {
		return err
	}
	if len(members) == 0 {
		return nil
	}
	if manifest.Ecosystem == modules.WorkspaceCargo {
		return fmt.Errorf("%w: %s has %d members (e.g. %q)", errRebuildWorkspaceOutput, manifest.ManifestPath, len(members), members[0].ManifestPath)
	}
	// Native sorts on manifest path only, starting from the same ordered
	// directory expansion. Equal paths can carry distinct literal dirs;
	// keep that order and let the Graph-style upsert settle the edge key.
	sort.Slice(members, func(i, j int) bool { return members[i].ManifestPath < members[j].ManifestPath })
	root, edges := modules.BuildWorkspaceArtifacts(manifest, members)
	nodes := make([]*graph.Node, 1, len(members)+1)
	nodes[0] = root
	for _, mem := range members {
		key := mem.ManifestPath
		nodes = append(nodes, &graph.Node{ID: key, Name: path.Base(key), FilePath: key, Kind: graph.KindFile, Language: manifestLanguage(key)})
	}
	return build.AddRebuildManifestArtifacts(ctx, nodes, edges)
}

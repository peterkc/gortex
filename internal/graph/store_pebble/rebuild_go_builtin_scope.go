package store_pebble

import (
	"go/token"
	"maps"
	"path"
	"strings"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

// RebuildGoBuiltinScopeReceiptMeta transports private syntax facts through the
// existing receipt codec. It is not part of the persisted graph node.
const RebuildGoBuiltinScopeReceiptMeta = "platform_receiver_go_package_scope"

// Use the same projection when writing the graph and proving the identity of
// a working row against a receipt. Keep the original receipt available to the
// package-scope census.
func projectRebuildReceiptNode(n *graph.Node) *graph.Node {
	if n == nil || n.Kind != graph.KindFile || n.Language != "go" {
		return n
	}
	if _, present := n.Meta[RebuildGoBuiltinScopeReceiptMeta]; !present {
		return n
	}
	projected := *n
	projected.Meta = maps.Clone(n.Meta)
	delete(projected.Meta, RebuildGoBuiltinScopeReceiptMeta)
	if len(projected.Meta) == 0 {
		projected.Meta = nil
	}
	return &projected
}

func rebuildGoBuiltinFileScopeKey(file string) []byte {
	return keyStrings([]byte{'g', 'G', 'f'}, file)
}

func rebuildGoBuiltinBlockerKey(repo, dir, pkg, name string) []byte {
	return keyStrings([]byte{'g', 'G', 'b'}, repo, dir, pkg, name)
}

func (b *BuildStore) recordRebuildGoBuiltinScope(repo string, facts ParseFacts) error {
	if facts.EffectiveLanguage != "go" {
		return nil
	}
	file := facts.File.FilePath
	var scope string
	for _, n := range facts.Nodes {
		if n != nil && n.Kind == graph.KindFile && n.Language == "go" && n.ID == file && n.FilePath == file {
			repo = n.RepoPrefix
			if raw, present := n.Meta[RebuildGoBuiltinScopeReceiptMeta]; present {
				var ok bool
				scope, ok = raw.(string)
				if !ok || scope == "" {
					return invalid("Go builtin package scope receipt")
				}
			}
			break
		}
	}
	dir := path.Dir(file)
	if scope == "" {
		// An unchecked file could declare any name in any package in its dir.
		return b.pending.Set(rebuildGoBuiltinBlockerKey(repo, dir, "", ""), []byte{1}, pebble.NoSync)
	}
	fields := strings.Fields(scope)
	if len(fields) == 0 || !token.IsIdentifier(fields[0]) {
		return invalid("Go builtin package identifier")
	}
	pkg := fields[0]
	for _, name := range fields[1:] {
		if !graph.IsGoBuiltinFunction(name) {
			return invalid("Go builtin package declaration name")
		}
		if err := b.pending.Set(rebuildGoBuiltinBlockerKey(repo, dir, pkg, name), []byte{1}, pebble.NoSync); err != nil {
			return err
		}
	}
	return b.pending.Set(rebuildGoBuiltinFileScopeKey(file), []byte(pkg), pebble.NoSync)
}

func rebuildGoBuiltinPackageClear(reader rebuildPlatformReader, file, repo, name string) (bool, error) {
	pkg, err := pendingGet(reader, rebuildGoBuiltinFileScopeKey(file), copyValue)
	if err != nil || len(pkg) == 0 {
		return false, err
	}
	dir := path.Dir(file)
	for _, key := range [][]byte{rebuildGoBuiltinBlockerKey(repo, dir, "", ""), rebuildGoBuiltinBlockerKey(repo, dir, string(pkg), name)} {
		blocked, err := pendingGet(reader, key, copyValue)
		if err != nil || len(blocked) != 0 {
			return false, err
		}
	}
	return true, nil
}

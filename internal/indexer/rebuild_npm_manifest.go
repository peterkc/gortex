package indexer

import (
	"fmt"
	"path"

	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/indexer/merkle"
	"github.com/zzet/gortex/internal/modules"
)

// A row is present regardless of whether the manifest had any useful fields.
type rebuildNpmManifestFact struct {
	Path     string
	Digest   rebuildDigest
	Manifest npmResolverManifest
}

// The owner supplies the admitted identity and bounded checked bytes. For a
// followed root symlink it supplies the verified regular target identity under
// the logical package.json key, not the hash of the link text.
func normalizeRebuildNpmManifest(rel string, claimed rebuildInput, src []byte, rawRemaining *int64) (rebuildNpmManifestFact, error) {
	if !validRebuildNamespaceKey(rel) || path.Base(rel) != "package.json" || claimed.Path != rel ||
		claimed.Kind != merkle.RegularFile || claimed.Language == "" || claimed.Size < 0 || claimed.Content == (rebuildDigest{}) {
		return rebuildNpmManifestFact{}, fmt.Errorf("%w: invalid npm manifest identity %q", ErrBuildInputChanged, rel)
	}
	entryRemaining := int64(rebuildNpmFactBytes - len(rel))
	size := int64(len(src))
	if rawRemaining == nil || *rawRemaining > rebuildCppRawLimit || max(claimed.Size, size) > min(entryRemaining, *rawRemaining) {
		return rebuildNpmManifestFact{}, fmt.Errorf("%w: npm manifest %q", errRebuildCppCaptureLimit, rel)
	}
	if size != claimed.Size || rebuildDigest(blake3.Sum256(src)) != claimed.Content {
		return rebuildNpmManifestFact{}, fmt.Errorf("%w: npm manifest %q", ErrBuildInputChanged, rel)
	}
	// These parsers have independent JSON type errors. Do not merge their
	// targets into one struct whose first error would drop unrelated fields.
	specs := modules.ParsePackageJSON(src)
	manifest := npmResolverManifest{
		aliases:      npmResolverAliases(specs),
		dependencies: npmResolverDependencies(specs),
		exports:      parsePackageExports(src),
		name:         parseNpmPackageName(src),
	}
	if rel == "package.json" {
		manifest.workspaces = parseNpmWorkspacePatterns(src)
	}
	*rawRemaining -= size
	return rebuildNpmManifestFact{Path: rel, Digest: claimed.Content, Manifest: manifest}, nil
}

package indexer

import (
	"context"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

// These are the native resolver's parsed projections, not package JSON.
// Exports contains selected targets only; its declaring name is Name.
// The JSON writer sorts map keys but does not change workspace order.
type rebuildNpmManifestPayload struct {
	Name         string            `json:"name,omitempty"`
	Aliases      map[string]string `json:"aliases,omitempty"`
	Dependencies map[string]string `json:"dependencies,omitempty"`
	Exports      map[string]string `json:"exports,omitempty"`
	Workspaces   []string          `json:"workspaces,omitzero"`
}

// The owner emits these positive facts in literal key order and separately
// supplies their checked namespace. Normalization has already charged the
// raw manifest bytes against the lease allowance.
func rebuildNpmManifestSidecarRow(f rebuildNpmManifestFact) (rebuildSidecarRow, error) {
	m := f.Manifest
	row := rebuildSidecarRow{Kind: rebuildNpmManifestRow, Path: []byte(f.Path), Present: true, Digest: f.Digest}
	p := rebuildNpmManifestPayload{Name: m.name, Aliases: m.aliases, Dependencies: m.dependencies, Workspaces: m.workspaces}
	if m.exports != nil {
		if m.exports.name != m.name || len(m.exports.subpaths) == 0 {
			return rebuildSidecarRow{}, fmt.Errorf("%w: inconsistent npm export projection %q", errRebuildInputSidecar, f.Path)
		}
		p.Exports = m.exports.subpaths
	}
	if !p.empty() {
		row.NPM = &p
	}
	return row, row.checkNpmManifest()
}

func (p *rebuildNpmManifestPayload) empty() bool {
	return p.Name == "" && len(p.Aliases) == 0 && len(p.Dependencies) == 0 && len(p.Exports) == 0 && p.Workspaces == nil
}

func (r rebuildSidecarRow) checkNpmManifest() error {
	key := string(r.Path)
	if !validRebuildNamespaceKey(key) || path.Base(key) != "package.json" || !r.Present ||
		r.Digest == (rebuildDigest{}) || len(r.Dirs) != 0 || r.Config != nil {
		return fmt.Errorf("%w: invalid npm manifest row %q", errRebuildInputSidecar, key)
	}
	p := r.NPM
	if p == nil {
		return nil // read, but no useful fields
	}
	bad := func() error {
		return fmt.Errorf("%w: invalid normalized npm manifest %q", errRebuildInputSidecar, key)
	}
	if p.empty() || key != "package.json" && p.Workspaces != nil {
		return bad()
	}
	// Bound work before JSON encoding. Each string needs at least its UTF-8
	// bytes plus quotes. The stream writer checks the full escaped row size.
	left := rebuildCppEntryLimit - len(r.Path)
	checkText := func(s string) error {
		if len(s) > left-2 {
			return fmt.Errorf("%w: npm manifest row %q: %w", errRebuildInputSidecar, key, errRebuildCppCaptureLimit)
		}
		left -= len(s) + 2
		if !utf8.ValidString(s) {
			return bad()
		}
		return nil
	}
	if err := checkText(p.Name); err != nil {
		return err
	}
	for _, m := range [...]map[string]string{p.Aliases, p.Dependencies, p.Exports} {
		for k, v := range m {
			if err := checkText(k); err != nil {
				return err
			}
			if err := checkText(v); err != nil {
				return err
			}
		}
	}
	for _, glob := range p.Workspaces {
		if err := checkText(glob); err != nil {
			return err
		}
	}
	// An alias can outlive an override of its dependency version, and native
	// even retains an empty alias key. Do not infer one projection from the other.
	for _, alias := range p.Aliases {
		if alias == "" {
			return bad()
		}
	}
	if _, emptyKey := p.Dependencies[""]; emptyKey {
		return bad()
	}
	for _, target := range p.Exports {
		if target == "" || strings.TrimSpace(target) != target {
			return bad()
		}
	}
	return nil
}

// Only the leased sidecar is opened. The encoded cap bounds the total retained
// text and number of fields and rows: each is decoded from its own bytes once.
// A missing indexed key is not a miss; the lookup must consult the namespace.
func openRebuildNpmManifestFacts(ctx context.Context, lease string, expected rebuildDigest) (*rebuildResolverNamespace, map[string]rebuildNpmManifestFact, rebuildDigest, error) {
	manifests := make(map[string]rebuildNpmManifestFact)
	var followed rebuildDigest
	ns, err := visitRebuildResolverNamespace(ctx, lease, expected, func(row rebuildSidecarRow) error {
		switch row.Kind {
		case rebuildNpmManifestRow:
			key := string(row.Path)
			if _, seen := manifests[key]; seen {
				return fmt.Errorf("%w: duplicate npm manifest %q", errRebuildInputSidecar, key)
			}
			f := rebuildNpmManifestFact{Path: key, Digest: row.Digest}
			if p := row.NPM; p != nil {
				f.Manifest = npmResolverManifest{name: p.Name, aliases: p.Aliases, dependencies: p.Dependencies, workspaces: p.Workspaces}
				if len(p.Exports) != 0 {
					f.Manifest.exports = &packageExports{name: p.Name, subpaths: p.Exports}
				}
			}
			manifests[key] = f
		case rebuildFollowedManifest:
			if string(row.Path) == "package.json" {
				if followed != (rebuildDigest{}) {
					return fmt.Errorf("%w: duplicate followed package.json", errRebuildInputSidecar)
				}
				followed = row.Digest
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, rebuildDigest{}, err
	}
	if followed != (rebuildDigest{}) && (manifests["package.json"].Digest != followed || ns.entries["package.json"].kind != rebuildNamespaceUnsupported) {
		return nil, nil, rebuildDigest{}, fmt.Errorf("%w: conflicting followed package.json proof: %w", errRebuildInputSidecar, errRebuildResolverNamespace)
	}
	for key := range manifests {
		if err := ctx.Err(); err != nil {
			return nil, nil, rebuildDigest{}, err
		}
		if key == "package.json" && followed != (rebuildDigest{}) {
			continue
		}
		kind, err := ns.lookup(key)
		if err != nil {
			return nil, nil, rebuildDigest{}, fmt.Errorf("%w: npm manifest %q: %w", errRebuildInputSidecar, key, err)
		}
		if kind != rebuildNamespaceRegular {
			return nil, nil, rebuildDigest{}, fmt.Errorf("%w: npm manifest %q disagrees with namespace: %w", errRebuildInputSidecar, key, errRebuildResolverNamespace)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, rebuildDigest{}, err
	}
	return ns, manifests, followed, nil
}

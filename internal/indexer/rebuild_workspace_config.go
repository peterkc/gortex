package indexer

import (
	"context"
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/modules"
)

const (
	rebuildPnpmWorkspaceFile  = "pnpm-workspace.yaml"
	rebuildCargoWorkspaceFile = "Cargo.toml"
)

// npm workspace patterns already ride the npm manifest row. Only the root
// pnpm/Cargo declarations need a separate payload. Keep exact pattern bytes.
type rebuildWorkspacePayload struct {
	Patterns [][]byte `json:"patterns,omitempty"`
	Exclude  [][]byte `json:"exclude,omitempty"`
}

func (r rebuildSidecarRow) checkWorkspaceConfig() error {
	key := string(r.Path)
	if (key != rebuildPnpmWorkspaceFile && key != rebuildCargoWorkspaceFile) || !r.Present ||
		r.Digest == (rebuildDigest{}) || len(r.Dirs) != 0 || r.Config != nil || r.NPM != nil ||
		r.Classification != nil || r.GitVersion != "" || r.GitVerdict != 0 || r.Workspace == nil ||
		(len(r.Workspace.Exclude) != 0 && key != rebuildCargoWorkspaceFile) {
		return fmt.Errorf("%w: invalid workspace declaration %q", errRebuildInputSidecar, key)
	}
	left := rebuildCppEntryLimit - len(key)
	for _, patterns := range [][][]byte{r.Workspace.Patterns, r.Workspace.Exclude} {
		for _, pattern := range patterns {
			if len(pattern) > left-1 {
				return fmt.Errorf("%w: workspace declaration %q: %w", errRebuildInputSidecar, key, errRebuildCppCaptureLimit)
			}
			left -= len(pattern) + 1
			if len(pattern) == 0 || strings.TrimSpace(string(pattern)) != string(pattern) {
				return fmt.Errorf("%w: unnormalized workspace declaration %q", errRebuildInputSidecar, key)
			}
		}
	}
	return nil
}

// Capture in native priority order. An opaque higher-priority config question
// belongs to replay; it must not authorize reading a lower-priority body.
func captureRebuildWorkspaceConfigs(ctx context.Context, src *source.FilesystemSource, epoch rebuildCheckoutInputs,
	raw *int64, emit func(rebuildSidecarRow) error,
) error {
	for _, key := range []string{rebuildPnpmWorkspaceFile, rebuildCargoWorkspaceFile} {
		kind, err := epoch.namespace.lookup(key)
		if err != nil {
			return nil // the denied or unproved name is already carried by the namespace
		}
		if kind != rebuildNamespaceRegular {
			continue
		}
		if epoch.admitted[key] == nil {
			if _, ok := epoch.configObservations[key]; !ok {
				return nil // replay must refuse when only the regular type is known
			}
		}
		m, err := readCheckedRebuildRootManifest(ctx, src, epoch, key, min(*raw, rebuildCppEntryLimit))
		if err != nil {
			return err
		}
		*raw -= int64(len(m.data))
		var patterns, excludes []string
		if key == rebuildPnpmWorkspaceFile {
			patterns = modules.ParsePnpmWorkspaceYAML(m.data)
		} else {
			patterns, excludes = modules.ParseCargoWorkspace(m.data)
		}
		payload := new(rebuildWorkspacePayload)
		for _, p := range patterns {
			payload.Patterns = append(payload.Patterns, []byte(p))
		}
		for _, p := range excludes {
			payload.Exclude = append(payload.Exclude, []byte(p))
		}
		err = emit(rebuildSidecarRow{Kind: rebuildWorkspaceConfigRow, Path: []byte(key), Present: true,
			Digest: m.target.Content, Workspace: payload})
		if err != nil || len(patterns) != 0 {
			return err
		}
	}
	return ctx.Err()
}

// Resolve declarations, not member package names: a member manifest needs
// only a verified file type. npm rewrite has different pattern semantics.
func openRebuildWorkspaceManifest(ctx context.Context, lease string, digest rebuildDigest,
	ns *rebuildResolverNamespace, npm rebuildNpmManifestFact, followed rebuildDigest,
) (*modules.WorkspaceManifest, error) {
	const npmKey = "package.json"
	if followed == (rebuildDigest{}) {
		kind, err := ns.lookup(npmKey)
		if err != nil {
			return nil, fmt.Errorf("workspace root %q: %w", npmKey, err)
		}
		if kind == rebuildNamespaceRegular && npm.Digest == (rebuildDigest{}) {
			return nil, fmt.Errorf("%w: no workspace declaration evidence for %q", errRebuildResolverNamespace, npmKey)
		}
	}
	var patterns []string
	for _, p := range npm.Manifest.workspaces {
		if p = strings.TrimSpace(p); p != "" {
			patterns = append(patterns, p)
		}
	}
	if len(patterns) != 0 {
		return &modules.WorkspaceManifest{Ecosystem: modules.WorkspaceNPM, ManifestPath: npmKey, Patterns: patterns}, nil
	}
	var rows map[string]*rebuildWorkspacePayload
	for _, key := range []string{rebuildPnpmWorkspaceFile, rebuildCargoWorkspaceFile} {
		kind, err := ns.lookup(key)
		if err != nil {
			return nil, fmt.Errorf("workspace root %q: %w", key, err)
		}
		if kind != rebuildNamespaceRegular {
			continue
		}
		if rows == nil {
			rows = make(map[string]*rebuildWorkspacePayload, 2)
			if err := visitRebuildInputSidecar(ctx, lease, digest, func(row rebuildSidecarRow) error {
				if row.Kind != rebuildWorkspaceConfigRow {
					return nil
				}
				name := string(row.Path)
				if rows[name] != nil || ns.entries[name].kind != rebuildNamespaceRegular {
					return fmt.Errorf("%w: conflicting workspace declaration %q", errRebuildInputSidecar, name)
				}
				rows[name] = row.Workspace
				return nil
			}); err != nil {
				return nil, err
			}
		}
		p := rows[key]
		if p == nil {
			return nil, fmt.Errorf("%w: no workspace declaration evidence for %q", errRebuildResolverNamespace, key)
		}
		if len(p.Patterns) != 0 {
			eco := modules.WorkspacePnpm
			if key == rebuildCargoWorkspaceFile {
				eco = modules.WorkspaceCargo
			}
			return &modules.WorkspaceManifest{Ecosystem: eco, ManifestPath: key,
				Patterns: workspacePatternStrings(p.Patterns), Exclude: workspacePatternStrings(p.Exclude)}, nil
		}
	}
	return nil, ctx.Err()
}

func workspacePatternStrings(patterns [][]byte) []string {
	var out []string
	for _, p := range patterns {
		out = append(out, string(p))
	}
	return out
}

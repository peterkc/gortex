package indexer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/modules"
)

var errRebuildModuleInput = errors.New("unproved frozen rebuild module input")

func isRebuildModuleLock(key string) bool {
	switch key {
	case "package-lock.json", "yarn.lock", "pnpm-lock.yaml":
		return true
	default:
		return false
	}
}

func isRebuildModuleRoot(key string) bool {
	return key == "go.mod" || key == "package.json" || isRebuildModuleLock(key)
}

// All five roots have a row, including explicit absence. The payload keeps
// only the native artifact and import matcher inputs, in parser order.
type rebuildModulePayload struct {
	Own   string         `json:"own,omitempty"`
	Specs []modules.Spec `json:"specs,omitempty"`
}

func emitRebuildModuleFact(key string, m *rebuildRootManifest, emit func(rebuildSidecarRow) error) error {
	r := rebuildSidecarRow{Kind: rebuildModuleFact, Path: []byte(key)}
	if m != nil {
		r.Present, r.Digest = true, m.target.Content
		p := &rebuildModulePayload{}
		for _, spec := range rootManifests() {
			if spec.path != key {
				continue
			}
			p.Specs = spec.parse(m.data)
			if spec.ownPathFromSrc != nil {
				p.Own = spec.ownPathFromSrc(m.data)
			}
			break
		}
		for i := range p.Specs {
			p.Specs[i].Alias = "" // neither native consumer reads Alias
		}
		r.Module = p
	}
	return emit(r)
}

func (r rebuildSidecarRow) checkModuleFact() error {
	key := string(r.Path)
	if !isRebuildModuleRoot(key) || r.Present != (r.Digest != (rebuildDigest{})) || r.Present != (r.Module != nil) ||
		len(r.Dirs) != 0 || r.Config != nil || r.NPM != nil || r.Workspace != nil || r.Classification != nil || r.GitVersion != "" || r.GitVerdict != 0 {
		return fmt.Errorf("%w: invalid fact fields for %q", errRebuildModuleInput, key)
	}
	if !r.Present {
		return nil
	}
	validText := func(s string) bool { return utf8.ValidString(s) && !strings.ContainsRune(s, 0) }
	p := r.Module
	if !validText(p.Own) || isRebuildModuleLock(key) && p.Own != "" {
		return fmt.Errorf("%w: invalid own path in %q", errRebuildModuleInput, key)
	}
	for i, s := range p.Specs {
		goSpec := key == "go.mod"
		if (goSpec && (s.Ecosystem != "go" || s.Line <= 0 || i > 0 && p.Specs[i-1].Line >= s.Line)) ||
			(!goSpec && (s.Ecosystem != "npm" || s.Line != 0)) ||
			s.Alias != "" || !validText(s.Path) || !validText(s.Version) || !validText(s.Replace) {
			return fmt.Errorf("%w: invalid spec in %q", errRebuildModuleInput, key)
		}
		// The npm lock parser sorts a map's output by Path only. Equal
		// paths with different specs have no stable native first winner.
		if key == "package-lock.json" && i > 0 {
			prev := p.Specs[i-1]
			if prev.Path > s.Path || prev.Path == s.Path && prev != s {
				return fmt.Errorf("%w: unordered or ambiguous %q dependency %q", errRebuildModuleInput, key, s.Path)
			}
		}
	}
	return nil
}

func openRebuildModuleFacts(ctx context.Context, lease string, digest rebuildDigest, inputs []rebuildInput) (map[string]rebuildSidecarRow, error) {
	facts := make(map[string]rebuildSidecarRow)
	identities := make(map[string]rebuildDigest)
	for _, in := range inputs {
		if isRebuildModuleRoot(in.Path) {
			identities[in.Path] = in.Content
		}
	}
	followed := make(map[string]rebuildDigest)
	// Bind the new rows to the old capture's independent identities where
	// present, including the effective digest of an admitted root link.
	record := func(ids map[string]rebuildDigest, name string, digest rebuildDigest) error {
		if prev, ok := ids[name]; ok && prev != digest {
			return fmt.Errorf("%w: conflicting identity for %q", errRebuildModuleInput, name)
		}
		ids[name] = digest
		return nil
	}
	ns, err := visitRebuildResolverNamespace(ctx, lease, digest, func(r rebuildSidecarRow) error {
		key := string(r.Path)
		if !isRebuildModuleRoot(key) {
			return nil
		}
		switch r.Kind {
		case rebuildModuleFact:
			if _, dup := facts[key]; dup {
				return fmt.Errorf("%w: repeated root %q", errRebuildModuleInput, key)
			}
			facts[key] = r
		case rebuildGoManifestFact, rebuildNpmManifestRow:
			return record(identities, key, r.Digest)
		case rebuildFollowedManifest:
			return record(followed, key, r.Digest)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errRebuildModuleInput, err)
	}
	if ns.entries["."].close != rebuildNamespaceCloseExact {
		return nil, fmt.Errorf("%w: no exact root listing", errRebuildModuleInput)
	}
	for _, spec := range rootManifests() {
		key := spec.path
		if !isRebuildModuleRoot(key) {
			continue
		}
		r, ok := facts[key]
		if !ok {
			return nil, fmt.Errorf("%w: missing root %q", errRebuildModuleInput, key)
		}
		link, linked := followed[key]
		kind := ns.entries[key].kind
		if !linked {
			var err error
			kind, err = ns.lookup(key)
			if err != nil {
				return nil, fmt.Errorf("%w: %q: %w", errRebuildModuleInput, key, err)
			}
		}
		known, identified := identities[key]
		present := kind == rebuildNamespaceRegular || linked
		if (linked && (kind != rebuildNamespaceUnsupported || link != r.Digest)) ||
			(!present && kind != rebuildNamespaceAbsent && kind != rebuildNamespaceDirectory) ||
			r.Present != present || identified && known != r.Digest {
			return nil, fmt.Errorf("%w: mismatched root %q", errRebuildModuleInput, key)
		}
	}
	return facts, nil
}

// runRebuildModules owns only the Go/npm manifests of the empty-prefix
// single checkout. Workspace emission and other ecosystems are separate.
func runRebuildModules(ctx context.Context, lease string, digest rebuildDigest, build *store_pebble.BuildStore, inputs []rebuildInput) error {
	facts, err := openRebuildModuleFacts(ctx, lease, digest, inputs)
	if err != nil {
		return err
	}
	view := build.RebuildView()
	for _, spec := range rootManifests() {
		key := spec.path
		p := facts[key].Module
		if p == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		manifest := &graph.Node{ID: key, Name: key, FilePath: key, Kind: graph.KindFile, Language: manifestLanguage(key)}
		nodes, edges := modules.BuildGraphArtifacts(key, p.Specs)
		if err := build.AddRebuildManifestArtifacts(ctx, append([]*graph.Node{manifest}, nodes...), edges); err != nil {
			return err
		}
		if len(p.Specs) == 0 {
			// No import scan is needed. A view read still surfaces a sticky
			// error from writing the synthetic manifest node.
			if _, err := view.Node(ctx, key); err != nil {
				return err
			}
			continue
		}
		importEdge := modules.ModuleImportMatcher(p.Specs, p.Own)
		if err := view.VisitNodes(ctx, func(n *graph.Node) error {
			if n.RepoPrefix == "" {
				if e := importEdge(n); e != nil {
					build.AddEdge(e)
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return ctx.Err()
}

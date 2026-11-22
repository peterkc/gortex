package resolver

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

type rebuildMainImportInputs struct {
	jsts        rebuildJSTSImportInputs
	goOwnership func(GoImportCandidate) (GoPackageOwnershipResult, error)
}

// resolveRebuildImport is the ordinary main import reducer. It changes only
// the tentative edge; the page owner clears terminal stamps and saves moves.
// Missing input authority is not a native miss and cannot emit an external ID.
func resolveRebuildImport(ctx context.Context, f *rebuildLinkFrontier, edge *graph.Edge, spec string) error {
	if isGodotResPlaceholder(edge.To) || isLuaRequireEdge(edge) {
		return nil
	}
	source, err := f.view.Node(ctx, edge.From)
	if err != nil {
		return err
	}
	repo, workspace := "", candidateWorkspaceID(source)
	if source != nil {
		repo = source.RepoPrefix
	}
	bind := func(n *graph.Node) {
		edge.To = n.ID
		if repo != "" && n.RepoPrefix != "" && n.RepoPrefix != repo {
			edge.CrossRepo = true
		}
	}

	if source != nil && (source.Language == "typescript" || source.Language == "javascript") {
		target, effective, err := resolveRebuildJSTSImport(edge, source, spec, func(id string) (*graph.Node, error) {
			return f.view.Node(ctx, id)
		}, f.imports.jsts)
		if err != nil {
			return err
		}
		if target != nil {
			bind(target)
			return nil
		}
		spec = effective
	} else {
		callerFile := jsTSImportCallerFile(edge)
		if module, _ := splitImportSpecSymbol(spec); module != "" && isJSTSPath(callerFile) {
			if !strings.HasPrefix(module, "./") && !strings.HasPrefix(module, "../") {
				return rebuildImportNeeds(edge, spec, "TS path-alias/baseUrl result (before npm rewrite)")
			}
			// Native relative expansion never calls the alias callback. Reuse
			// its exact file and binding probes against the typed working rows.
			id := resolveJSTSImportTarget(func(id string) *graph.Node {
				if err != nil {
					return nil
				}
				var n *graph.Node
				n, err = f.view.Node(ctx, id)
				return n
			}, nil, callerFile, spec)
			if err != nil {
				return err
			}
			if id != "" {
				n, err := f.view.Node(ctx, id)
				if err != nil {
					return err
				}
				if n == nil {
					return rebuildImportNeeds(edge, spec, "stable relative target")
				}
				if rebuildImportNeedsGoOwnership(source, n) {
					return rebuildImportNeeds(edge, spec, "Go package ownership")
				}
				bind(n)
				return nil
			}
		}
		// Unlike the JS/TS point probe, the cold indexer's npm callback uses
		// the literal Edge.FilePath and case-sensitive source extensions, with
		// no SFC hosts. A relative miss never needs an npm rewrite receipt.
		if spec != "" && !strings.HasPrefix(spec, ".") && !strings.HasPrefix(spec, "/") {
			switch path.Ext(edge.FilePath) {
			case ".ts", ".tsx", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs":
				return rebuildImportNeeds(edge, spec, "npm alias/workspace/exports rewrite result")
			}
		}
	}
	if fqn := phpEdgeMetaString(edge, "fqn"); fqn != "" && !strings.HasSuffix(fqn, `\`) {
		// A point/live name lookup cannot replace the cold page's merged
		// repository + language groups, including their checked negatives.
		return rebuildImportNeeds(edge, spec, "native PHP FQN warm candidate group")
	}

	// Keep only the native qualified-name winners. Repo/workspace here
	// are saved node identities, not package-manager directory ownership.
	var sameWorkspace, sameRepo, foreign *graph.Node
	var ambiguousForeign bool
	if spec == "" {
		return rebuildImportNeeds(edge, spec, "native file-directory buckets and terminal result")
	}
	req := store_pebble.LinkRequest{Kind: store_pebble.LinkQualName, Key: spec, AllRepos: true}
	if _, err := f.candidates(ctx, []store_pebble.LinkRequest{req}, func(_ int, n *graph.Node) error {
		if strings.HasPrefix(n.ID, externalCallPrefix) {
			return nil
		}
		if rebuildImportNeedsGoOwnership(source, n) {
			if f.imports.goOwnership == nil {
				return rebuildImportNeeds(edge, spec, "Go package ownership")
			}
			owner, err := f.imports.goOwnership(GoImportCandidate{
				ImportPath: spec, ImporterRepoPrefix: source.RepoPrefix, ImporterFilePath: source.FilePath,
				CandidateID: n.ID, CandidateRepoPrefix: n.RepoPrefix, CandidateFilePath: n.FilePath, CandidateNode: n,
			})
			if err != nil {
				return err
			}
			switch owner {
			case GoPackageOwnershipDifferent:
				return nil
			case GoPackageOwnershipExact, GoPackageOwnershipUnknown:
			default:
				return rebuildImportNeeds(edge, spec, "valid Go package ownership")
			}
		}
		if n.RepoPrefix == repo {
			if sameRepo == nil || n.ID < sameRepo.ID {
				sameRepo = n
			}
			if candidateWorkspaceID(n) == workspace && (sameWorkspace == nil || n.ID < sameWorkspace.ID) {
				sameWorkspace = n
			}
		} else if foreign == nil {
			foreign = n
		} else {
			ambiguousForeign = true
		}
		return nil
	}); err != nil {
		return err
	}
	switch {
	case sameWorkspace != nil:
		bind(sameWorkspace)
	case sameRepo != nil:
		bind(sameRepo)
	case foreign != nil && !ambiguousForeign:
		bind(foreign)
	default:
		// A qualified miss or ambiguity still reaches the directory buckets
		// before the dependency-contract bridge or terminal decision. The
		// tail basename index cannot attest those buckets or their ordering.
		if source != nil && source.Language == "go" {
			return rebuildImportNeeds(edge, spec, "native file-directory buckets with Go package ownership and workspace membership")
		}
		return rebuildImportNeeds(edge, spec, "native file-directory buckets and workspace membership")
	}
	return nil
}

// Non-Go callers never use the native Go gate. In both pinned cold ownership
// providers, foreign, non-Go and contract rows cannot be certified Different.
func rebuildImportNeedsGoOwnership(source, target *graph.Node) bool {
	return source != nil && source.Language == "go" && target.FilePath != "" &&
		target.RepoPrefix == source.RepoPrefix && target.Language == "go" && target.Kind != graph.KindContract
}

func rebuildImportNeeds(edge *graph.Edge, spec, fact string) error {
	return fmt.Errorf("%w: import %q in %q needs %s", errRebuildMainRule, spec, jsTSImportCallerFile(edge), fact)
}

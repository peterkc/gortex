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
	jsts                  rebuildJSTSImportInputs
	goOwnership           func(GoImportCandidate) (GoPackageOwnershipResult, error)
	workspaceMembership   func(string) (string, error)
	externalNpmDependency func(string, string) (bool, error)
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
	bind := func(id, targetRepo string) {
		edge.To = id
		if repo != "" && targetRepo != "" && targetRepo != repo {
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
			bind(target.ID, target.RepoPrefix)
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
				bind(n.ID, n.RepoPrefix)
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
		bind(sameWorkspace.ID, sameWorkspace.RepoPrefix)
	case sameRepo != nil:
		bind(sameRepo.ID, sameRepo.RepoPrefix)
	case foreign != nil && !ambiguousForeign:
		bind(foreign.ID, foreign.RepoPrefix)
	default:
		// Source authority is required for this tier. Do not expand the
		// projected pass to deferred languages or to a missing source.
		if source != nil && (source.Language == "go" && f.imports.goOwnership != nil ||
			source.Language == "typescript" || source.Language == "javascript") {
			file, err := resolveRebuildImportDirectory(ctx, f, edge, source, spec)
			if err != nil {
				return err
			}
			if file.ID != "" {
				bind(file.ID, file.RepoPrefix)
				return nil
			}
		}
		// A directory miss or bypass doesn't prove the downstream
		// dependency bridge, npm retry or terminal result.
		if source != nil && source.Language == "go" {
			return rebuildImportNeeds(edge, spec, "native file-directory buckets with Go package ownership and workspace membership")
		}
		return rebuildImportNeeds(edge, spec, "native file-directory buckets and workspace membership")
	}
	return nil
}

// resolveRebuildImportDirectory walks the native cold Indexer's two buckets.
// That Indexer always installs workspace collection, so neither a folder nor
// exact Go ownership can stop the walk. Keep only the winning rank and first
// ordered identities; for a relevant tie, replay the frozen buckets rather
// than retaining a candidate list.
func resolveRebuildImportDirectory(ctx context.Context, f *rebuildLinkFrontier, edge *graph.Edge, source *graph.Node, spec string) (graph.FileNodeIdentity, error) {
	var same, foreign graph.FileNodeIdentity
	caller := jsTSImportCallerFile(edge)
	bareJSTS := isJSTSBareSpecifier(caller, spec)
	if bareJSTS {
		if f.imports.externalNpmDependency == nil {
			return same, rebuildImportNeeds(edge, spec, "checked external npm dependency result")
		}
		module, _ := splitImportSpecSymbol(spec)
		external, err := f.imports.externalNpmDependency(caller, module)
		if err != nil || external {
			return same, err
		}
	}
	dirs, err := f.importDirectories(ctx)
	if err != nil {
		return same, err
	}
	walk := func(visit func(graph.FileNodeIdentity) (bool, error)) error {
		more := true
		visitBucket := func(file graph.FileNodeIdentity) (bool, error) {
			var err error
			more, err = visit(file)
			return more, err
		}
		if err := dirs.VisitExactDirectory(ctx, spec, visitBucket); err != nil || !more {
			return err
		}
		last := spec[strings.LastIndexByte(spec, '/')+1:]
		return dirs.VisitLastDirectoryComponent(ctx, last, visitBucket)
	}

	const (
		rejected = iota
		fallback
		folderMatch
		exactOwnership
	)
	rank := func(file graph.FileNodeIdentity) (int, error) {
		owner := GoPackageOwnershipUnknown
		// Native compact candidates have no CandidateNode. The provider's
		// Go file inventory, not the projected row's language, owns this gate.
		if source.Language == "go" && file.RepoPrefix == source.RepoPrefix && file.FilePath != "" {
			var err error
			owner, err = f.imports.goOwnership(GoImportCandidate{
				ImportPath: spec, ImporterRepoPrefix: source.RepoPrefix, ImporterFilePath: source.FilePath,
				CandidateID: file.ID, CandidateRepoPrefix: file.RepoPrefix, CandidateFilePath: file.FilePath,
			})
			if err != nil {
				return rejected, err
			}
			switch owner {
			case GoPackageOwnershipDifferent:
				return rejected, nil
			case GoPackageOwnershipExact, GoPackageOwnershipUnknown:
			default:
				return rejected, rebuildImportNeeds(edge, spec, "valid Go package ownership")
			}
		}
		if bareJSTS && !isJSTSDirEntryPoint(file.FilePath) {
			return rejected, nil
		}
		if owner == GoPackageOwnershipExact {
			return exactOwnership, nil
		}
		dir := filePathDir(file.FilePath)
		if dir == file.RepoPrefix {
			dir = ""
		} else if file.RepoPrefix != "" {
			dir = strings.TrimPrefix(dir, file.RepoPrefix+"/")
		}
		if file.Language == source.Language && dirMatchesImport(dir, spec) {
			return folderMatch, nil
		}
		return fallback, nil
	}

	bestRank := rejected
	tied := false
	isSameRepo := func(file graph.FileNodeIdentity) bool {
		return source.RepoPrefix == "" || file.RepoPrefix == source.RepoPrefix
	}
	if err := walk(func(file graph.FileNodeIdentity) (bool, error) {
		quality, err := rank(file)
		if err != nil || quality == rejected {
			return err == nil, err
		}
		if !isSameRepo(file) {
			if foreign.ID == "" && dirMatchesImport(filePathDir(file.FilePath), spec) {
				foreign = file
			}
			return true, nil
		}
		if quality > bestRank {
			same, bestRank, tied = file, quality, false
		} else if quality == bestRank && file.FilePath != same.FilePath {
			tied = true
		}
		return true, nil
	}); err != nil {
		return graph.FileNodeIdentity{}, err
	}
	if same.ID == "" {
		return foreign, nil
	}
	// Several IDs at one path cannot give native a unique workspace match.
	if !tied || edge.FilePath == "" {
		return same, nil
	}
	workspace := f.imports.workspaceMembership
	if workspace == nil {
		return graph.FileNodeIdentity{}, rebuildImportNeeds(edge, spec, "checked workspace membership for equally ranked directory candidates")
	}
	callerWorkspace, err := workspace(edge.FilePath) // native uses the raw edge path, not the JS/TS fallback
	if err != nil || callerWorkspace == "" {
		return same, err
	}
	// Preserve exact/last duplicates. Native workspace arbitration counts
	// visits, not distinct IDs, and falls back to the first row on a second match.
	var match graph.FileNodeIdentity
	if err := walk(func(file graph.FileNodeIdentity) (bool, error) {
		if !isSameRepo(file) {
			return true, nil
		}
		quality, err := rank(file)
		if err != nil || quality != bestRank {
			return err == nil, err
		}
		member, err := workspace(file.FilePath)
		if err != nil || member != callerWorkspace {
			return err == nil, err
		}
		if match.ID != "" {
			match = graph.FileNodeIdentity{}
			return false, nil
		}
		match = file
		return true, nil
	}); err != nil {
		return graph.FileNodeIdentity{}, err
	}
	if match.ID != "" {
		return match, nil
	}
	return same, nil
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

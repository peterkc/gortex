package resolver

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// resolveRebuildExtern ports the ordinary cold resolveExtern cascade. The
// caller owns the tentative edge and clears terminal stamps when it saves a move.
func resolveRebuildExtern(ctx context.Context, f *rebuildLinkFrontier, edge *graph.Edge, spec string) error {
	q, err := prepareRebuildExtern(ctx, f, edge, spec)
	if err != nil {
		return err
	}
	return q.run(ctx, f)
}

func prepareRebuildExtern(ctx context.Context, f *rebuildLinkFrontier, edge *graph.Edge, spec string) (rebuildMainQuery, error) {
	if err := ctx.Err(); err != nil {
		return rebuildMainQuery{}, err
	}
	sep := strings.LastIndex(spec, "::")
	if sep < 0 {
		edge.To = "external::" + spec
		if spec != "" && !strings.ContainsRune(spec, 0) {
			f.noteExternalTarget(edge, spec)
		}
		return rebuildMainQuery{}, nil
	}
	importPath, symbol := spec[:sep], spec[sep+2:]
	goPath := strings.HasSuffix(edge.FilePath, ".go")
	if goPath {
		if check := f.imports.checkGoExtern; check != nil {
			if err := check(importPath); err != nil {
				return rebuildMainQuery{}, err
			}
		} else if importPath != "" {
			// A tracked module can keep even a complete name miss pending.
			return rebuildMainQuery{}, rebuildExternNeeds(edge, spec, "Go module identities/replacements, package ownership and cold deferral")
		}
	}

	var caller *graph.Node
	if edge.From != "" {
		var err error
		caller, err = f.view.Node(ctx, edge.From)
		if err != nil {
			return rebuildMainQuery{}, err
		}
	}
	if caller == nil && strings.HasPrefix(edge.From, "dep::") {
		// Native seeds Go dependency-contract nodes before ResolveAll. A
		// missing private row cannot certify the caller's repo or language.
		return rebuildMainQuery{}, rebuildExternNeeds(edge, spec, "transient dependency-contract source identity")
	}
	repo, language := "", ""
	if caller != nil {
		repo, language = caller.RepoPrefix, caller.Language
	}
	_, languages := resolverCompatibleLanguages(language)

	if isPythonSourcePath(edge.FilePath) && isDottedPythonModule(importPath) {
		match, err := rebuildPythonExternMatch(ctx, f, languages, repo, importPath, symbol)
		if err != nil {
			return rebuildMainQuery{}, err
		}
		if match != nil {
			edge.To = match.ID
			if repo != "" && match.RepoPrefix != "" && match.RepoPrefix != repo {
				edge.CrossRepo = true
			}
			return rebuildMainQuery{}, nil
		}
	}

	if caller != nil {
		caller = &graph.Node{ID: caller.ID, RepoPrefix: caller.RepoPrefix, Language: caller.Language, FilePath: caller.FilePath, WorkspaceID: caller.WorkspaceID}
	}
	preferLocal := repo != "" && language == "go"
	var precise, match, foreignFallback *graph.Node
	return rebuildMainQuery{request: store_pebble.LinkRequest{Kind: store_pebble.LinkName, Key: symbol, Languages: languages, AllRepos: true}, charge: rebuildMainCallerCharge(caller), consider: func(n *graph.Node) error {
		if precise != nil || (!goPath && match != nil) {
			return nil
		}
		switch n.Kind {
		case graph.KindFunction, graph.KindMethod, graph.KindType, graph.KindInterface:
		default:
			return nil // includes transient KindContract rows, not extern definitions
		}
		var preciseHit bool
		if goPath && n.RepoPrefix == repo && (n.Language == "" || n.Language == "go") {
			if f.imports.checkGoExtern == nil {
				return rebuildExternNeeds(edge, spec, "Go module/replacement eligibility for an empty import path")
			}
			// Reuse native's same-repo predicate. The checked root claim is
			// not a tracked module and there is no replacement directory.
			one := [1]*graph.Node{n}
			preciseHit = goModulePreciseLocal(nil, importPath, one[:], repo, "") != nil
		}
		var dirHit bool
		cross := repo != "" && n.RepoPrefix != "" && n.RepoPrefix != repo
		if match == nil && (!cross || !preferLocal || foreignFallback == nil) {
			dir := filePathDir(n.FilePath)
			if cross {
				dirHit = dirMatchesImport(dir, importPath)
			} else {
				dirHit = strings.HasSuffix(dir, "/"+lastPathComponent(importPath)) || dir == importPath || strings.HasSuffix(dir, importPath)
			}
		}
		if !preciseHit && !dirHit {
			return nil
		}
		if rebuildImportNeedsGoOwnership(caller, n) {
			if f.imports.goOwnership == nil {
				return rebuildExternNeeds(edge, spec, "Go package ownership")
			}
			owner, err := f.imports.goOwnership(GoImportCandidate{
				ImportPath: importPath, ImporterRepoPrefix: repo, ImporterFilePath: caller.FilePath,
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
				return rebuildExternNeeds(edge, spec, "valid Go package ownership")
			}
		}
		switch {
		case preciseHit:
			precise = n
		case cross && preferLocal:
			foreignFallback = n
		default:
			match = n
		}
		return nil
	}, finish: func() error {
		if precise != nil {
			// The native binder clones only when it can stamp go_import. A
			// preexisting malformed spec can still carry terminal/restub keys.
			if !strings.Contains(goModuleEdgeSpec(edge), "::") {
				edge.Meta = maps.Clone(edge.Meta)
			}
			bindGoModuleEdge(edge, precise, repo, "", nil)
			return nil
		}
		if match == nil {
			match = foreignFallback
		}
		if match != nil {
			cross := repo != "" && match.RepoPrefix != "" && match.RepoPrefix != repo
			if cross && candidateWorkspaceID(caller) != candidateWorkspaceID(match) {
				return rebuildExternNeeds(edge, spec, "cross-workspace dependency eligibility")
			}
			edge.To = match.ID
			if cross || preferLocal {
				edge.CrossRepo = cross
			}
			return nil
		}

		if isStdlibLike(importPath) {
			if caller == nil {
				repo = sourceSpellingRepoPrefix(edge.From)
			}
			edge.To = graph.StubID(repo, graph.StubKindStdlib, importPath, symbol)
		} else {
			edge.To = "dep::" + importPath + "::" + symbol
		}
		return nil
	}}, nil
}

func rebuildPythonExternMatch(ctx context.Context, f *rebuildLinkFrontier, languages []string, repo, importPath, symbol string) (*graph.Node, error) {
	type reading struct{ module, name string }
	readings := []reading{{importPath, symbol}}
	if name := pythonImportedName(importPath); name != "" {
		readings = append(readings, reading{importPath[:len(importPath)-len(name)-1], name})
	}
	for _, rd := range readings {
		var match *graph.Node
		ambiguous := false
		if err := visitRebuildExternCandidates(ctx, f, rd.name, languages, func(n *graph.Node) error {
			if (n.Kind != graph.KindFunction && n.Kind != graph.KindType) || !pythonModuleFileMatches(n.FilePath, rd.module) {
				return nil
			}
			switch {
			case match == nil || match.RepoPrefix != repo && n.RepoPrefix == repo:
				match, ambiguous = n, false
			case (match.RepoPrefix == repo) == (n.RepoPrefix == repo):
				ambiguous = true
			}
			return nil
		}); err != nil {
			return nil, err
		}
		if match != nil && !ambiguous {
			return match, nil
		}
	}
	return nil, nil
}

func visitRebuildExternCandidates(ctx context.Context, f *rebuildLinkFrontier, name string, languages []string, visit func(*graph.Node) error) error {
	if name == "" {
		return nil // native cachedFindExternNodesByName does not query an empty name
	}
	// AllRepos exact names alone use the live graph. The candidate seam
	// puts back native (repo, language, ID) order, dropping language from
	// the sort key as well as the filter when the language scope is open.
	req := store_pebble.LinkRequest{Kind: store_pebble.LinkName, Key: name, Languages: languages, AllRepos: true}
	_, err := f.candidates(ctx, []store_pebble.LinkRequest{req}, func(_ int, n *graph.Node) error { return visit(n) })
	return err
}

func rebuildExternNeeds(edge *graph.Edge, spec, fact string) error {
	return fmt.Errorf("%w: extern %q in %q needs %s", errRebuildMainRule, spec, edge.FilePath, fact)
}

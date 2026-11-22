package resolver

import (
	"context"
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// resolveRebuildExtern ports the ordinary cold resolveExtern cascade. The
// caller owns the tentative edge and clears terminal stamps when it saves a move.
func resolveRebuildExtern(ctx context.Context, f *rebuildLinkFrontier, edge *graph.Edge, spec string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sep := strings.LastIndex(spec, "::")
	if sep < 0 {
		edge.To = "external::" + spec
		return nil
	}
	importPath, symbol := spec[:sep], spec[sep+2:]
	goPath := strings.HasSuffix(edge.FilePath, ".go")
	if goPath && importPath != "" {
		// Tracked modules can keep even a complete name miss pending in the
		// cold shadow. A directory hit also cannot rule out the earlier tier.
		return rebuildExternNeeds(edge, spec, "Go module identities/replacements, package ownership and cold deferral")
	}

	var caller *graph.Node
	if edge.From != "" {
		var err error
		caller, err = f.view.Node(ctx, edge.From)
		if err != nil {
			return err
		}
	}
	if caller == nil && strings.HasPrefix(edge.From, "dep::") {
		// Native seeds Go dependency-contract nodes before ResolveAll. A
		// missing private row cannot certify the caller's repo or language.
		return rebuildExternNeeds(edge, spec, "transient dependency-contract source identity")
	}
	repo, language := "", ""
	if caller != nil {
		repo, language = caller.RepoPrefix, caller.Language
	}
	_, languages := resolverCompatibleLanguages(language)

	if isPythonSourcePath(edge.FilePath) && isDottedPythonModule(importPath) {
		match, err := rebuildPythonExternMatch(ctx, f, languages, repo, importPath, symbol)
		if err != nil {
			return err
		}
		if match != nil {
			edge.To = match.ID
			if repo != "" && match.RepoPrefix != "" && match.RepoPrefix != repo {
				edge.CrossRepo = true
			}
			return nil
		}
	}

	preferLocal := repo != "" && language == "go"
	var match, foreignFallback *graph.Node
	if err := visitRebuildExternCandidates(ctx, f, symbol, languages, func(n *graph.Node) error {
		switch n.Kind {
		case graph.KindFunction, graph.KindMethod, graph.KindType, graph.KindInterface:
		default:
			return nil // includes transient KindContract rows, not extern definitions
		}
		if goPath && n.RepoPrefix == repo && (n.Language == "" || n.Language == "go") {
			// An empty import path cannot defer, but a viable local
			// definition can still bind via a caller replacement.
			return rebuildExternNeeds(edge, spec, "Go module/replacement eligibility for an empty import path")
		}
		if match != nil {
			return nil
		}
		dir := filePathDir(n.FilePath)
		cross := repo != "" && n.RepoPrefix != "" && n.RepoPrefix != repo
		if !cross {
			if strings.HasSuffix(dir, "/"+lastPathComponent(importPath)) || dir == importPath || strings.HasSuffix(dir, importPath) {
				match = n
			}
			return nil
		}
		if dirMatchesImport(dir, importPath) {
			if !preferLocal {
				match = n
			} else if foreignFallback == nil {
				foreignFallback = n
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if match == nil {
		match = foreignFallback
	}
	if match != nil {
		cross := repo != "" && match.RepoPrefix != "" && match.RepoPrefix != repo
		if cross && candidateWorkspaceID(caller) != candidateWorkspaceID(match) {
			return rebuildExternNeeds(edge, spec, "cross-workspace dependency eligibility")
		}
		if rebuildImportNeedsGoOwnership(caller, match) {
			return rebuildExternNeeds(edge, spec, "Go package ownership")
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

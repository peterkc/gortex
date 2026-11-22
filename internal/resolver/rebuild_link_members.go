package resolver

import (
	"context"
	"fmt"
	"maps"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// resolveRebuildMember takes the name after "*.". The dispatcher owns type
// precedence, terminal clearing, and committing the whole page. A refusal
// leaves even the tentative edge unchanged.
func resolveRebuildMember(ctx context.Context, f *rebuildLinkFrontier, edge *graph.Edge, name string) error {
	caller, err := f.view.Node(ctx, edge.From)
	if err != nil {
		return err
	}
	var repo, lang, callerRecv, callerScope string
	if caller != nil {
		repo, lang = caller.RepoPrefix, caller.Language
		if caller.Kind == graph.KindMethod {
			callerRecv = nodeReceiverType(caller)
			callerScope = rebuildFieldPackage(caller)
		}
	}
	dir, receiver := filePathDir(edge.FilePath), edgeReceiverType(edge)
	fieldSite := edge.Kind == graph.EdgeReads || edge.Kind == graph.EdgeWrites
	var fields [4]string

	// Four fixed summaries mirror the two retain-on-empty C# applicability
	// filters. Even non-callable name matches count for these filters and for
	// the preceding direct-import filter. No summary holds parsed nodes.
	scope := newRebuildFunctionScope(edge, caller, name)
	all := rebuildMemberChoices{scope: scope}
	typed, arity, typedArity := all, all, all
	_, hasTypeArgs := metaIntValue(edge.Meta["type_arg_count"])
	_, hasArgs := metaIntValue(edge.Meta["arg_count"])
	member, _ := edge.Meta["member_call"].(bool)
	var firstDir string
	mixedDirs := false
	if name != "" {
		_, langs := resolverCompatibleLanguages(lang)
		req := store_pebble.LinkRequest{Kind: store_pebble.LinkName, Key: name, RepoPrefix: repo, Languages: langs}
		_, err = f.candidates(ctx, []store_pebble.LinkRequest{req}, func(_ int, n *graph.Node) error {
			one := [1]*graph.Node{n}
			if len(genericInstantiationOnly(edge, withoutAttributedTerminals(one[:]))) == 0 {
				return nil
			}
			candidateDir, recv := filePathDir(n.FilePath), nodeReceiverType(n)
			if fieldSite && n.Kind == graph.KindField {
				if receiver != "" && recv == receiver {
					if candidateDir == dir {
						keepFirstRebuildField(&fields[0], n.ID)
					}
					keepFirstRebuildField(&fields[1], n.ID)
				}
				if callerRecv != "" && recv == callerRecv && n.Language == lang &&
					(lang != "go" || caller.FilePath != "" && n.FilePath != "") && rebuildFieldPackage(n) == callerScope {
					keepFirstRebuildField(&fields[2], n.ID)
				}
				if candidateDir == dir {
					keepFirstRebuildField(&fields[3], n.ID)
				}
			}
			if all.count == 0 {
				firstDir = candidateDir
			}
			mixedDirs = mixedDirs || firstDir != candidateDir
			exact := receiver != "" && recv == receiver && n.Kind == graph.KindMethod
			callerMethod := !member && callerRecv != "" && recv == callerRecv && n.Kind == graph.KindMethod
			locality := !isCSharpExtension(n) && (!member || n.ID != edge.From)
			all.consider(n, candidateDir == dir, exact, callerMethod, locality)
			typeOK := hasTypeArgs && csharpAcceptsTypeArgCount(edge, n)
			argOK := hasArgs && csharpMethodAcceptsArgCount(edge, n)
			if typeOK {
				typed.consider(n, candidateDir == dir, exact, callerMethod, locality)
			}
			if argOK {
				arity.consider(n, candidateDir == dir, exact, callerMethod, locality)
				if typeOK {
					typedArity.consider(n, candidateDir == dir, exact, callerMethod, locality)
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	for i, id := range fields {
		if id != "" {
			edge.To, edge.Confidence = id, [4]float64{0.95, 0.85, 0.85, 0.6}[i]
			return nil
		}
	}

	refuse := func(reason string) error {
		return fmt.Errorf("%w: %s for wildcard member %q in %q", errRebuildMainRule, reason, name, edge.FilePath)
	}
	if phpTypedReceiverCaller(caller) && (phpEdgeMetaString(edge, "php_property_path") != "" || phpEdgeMetaString(edge, "receiver_fqn") != "") {
		return refuse("PHP property or FQN receiver lookup")
	}

	raw, withArity := &all, &arity
	if all.count > 1 && typed.count != 0 {
		raw, withArity = &typed, &typedArity
	}
	if raw.count > 1 && withArity.count != 0 {
		raw = withArity
	}
	// With one candidate directory, reachability keeps all or discards all
	// and then restores all. With no recorded edge path native skips it.
	// Otherwise scope (unlike the exact tiers) sees the filtered group.
	reachMayNarrow := edge.FilePath != "" && mixedDirs
	e := *edge
	picked := false
	if raw.count != 0 {
		if reachMayNarrow {
			pick, missing := all.scope.pick()
			if pick.id != "" || missing != "" || all.scope.cppCount != 0 {
				return refuse("scope selection needs native page direct-import reachability")
			}
		} else {
			pick, missing := raw.scope.pick()
			if missing != "" {
				return refuse(missing)
			}
			if pick.id != "" {
				picked = true
				e.To, e.Confidence, e.Origin = pick.id, 0.92, graph.OriginASTResolved
				e.Meta = maps.Clone(e.Meta)
				if e.Meta == nil {
					e.Meta = make(map[string]any, 1)
				}
				e.Meta["resolution"] = "scope"
			}
		}
		if !picked {
			switch {
			case lang == "php" && scopeMetaString(edge.Meta, MetaScopeKind) == ScopeKindParent:
				return refuse("PHP parent lookup")
			case raw.localExact != "":
				picked = true
				e.To, e.Confidence, e.Origin = raw.localExact, 0.95, graph.OriginASTResolved
			case raw.exact != "" && !raw.ambiguous:
				picked = true
				e.To, e.Confidence, e.Origin = raw.exact, 0.85, graph.OriginASTResolved
			case receiver != "" && lang == "php":
				return refuse("PHP in-repo receiver and inherited dispatch evidence")
			case reachMayNarrow && (all.method || all.locality):
				return refuse("native page direct-import reachability")
			case receiver != "" && raw.method:
				return refuse("native first-use useClass bindings")
			case raw.callerMethod:
				return refuse("caller-receiver method fallback")
			case lang == "csharp" && raw.extension:
				return refuse("C# extension visibility and hierarchy evidence")
			case receiver == "" && raw.methodCount > 1:
				return nil // native leaves an untyped ambiguous member unresolved
			case raw.locality:
				return refuse("method locality or interface dispatch fallback")
			}
		}
	}
	if !picked {
		// Pinned applyBuiltinIfKnown changes only To. Its repo spelling is
		// different from name lookup only when the source node is missing.
		if language := langFromFilePath(e.FilePath); language != "" {
			if category, ok := classifyBuiltin(name, language); ok {
				if caller == nil {
					repo = sourceSpellingRepoPrefix(e.From)
				}
				e.To = graph.StubID(repo, graph.StubKindBuiltin, language, category, name)
			}
		}
	}
	if e.Kind == graph.EdgeReads && e.To != edge.To {
		e.Kind = graph.EdgeReferences
	}
	*edge = e
	return nil
}

// Field owner equality uses the stored package stamp before the Go-only
// directory fallback, and compares empty non-Go stamps as native does.
func rebuildFieldPackage(n *graph.Node) string {
	if pkg := scopePkgOf(n); pkg != "" {
		return pkg
	}
	if n.Language == "go" && n.FilePath != "" {
		return filePathDir(n.FilePath)
	}
	return ""
}

func keepFirstRebuildField(dst *string, id string) {
	if *dst == "" {
		*dst = id
	}
}

type rebuildMemberChoices struct {
	scope                                                rebuildFunctionScope
	count, methodCount                                   int
	exact, localExact                                    string
	ambiguous, method, callerMethod, extension, locality bool
}

func (s *rebuildMemberChoices) consider(n *graph.Node, sameDir, exact, callerMethod, locality bool) {
	s.count++
	s.scope.consider(n)
	s.method = s.method || n.Kind == graph.KindMethod
	s.callerMethod = s.callerMethod || callerMethod
	s.extension = s.extension || isCSharpExtension(n)
	if locality && (n.Kind == graph.KindFunction || n.Kind == graph.KindMethod) {
		s.locality = true
		if n.Kind == graph.KindMethod {
			s.methodCount++
		}
	}
	if exact {
		if sameDir && s.localExact == "" {
			s.localExact = n.ID
		}
		if s.exact == "" {
			s.exact = n.ID
		} else if s.exact != n.ID {
			s.ambiguous = true
		}
	}
}

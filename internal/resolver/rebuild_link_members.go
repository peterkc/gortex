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
	q, err := prepareRebuildMember(ctx, f, edge, name)
	if err != nil {
		return err
	}
	return q.run(ctx, f)
}

func prepareRebuildMember(ctx context.Context, f *rebuildLinkFrontier, edge *graph.Edge, name string) (rebuildMainQuery, error) {
	caller, err := f.view.Node(ctx, edge.From)
	if err != nil {
		return rebuildMainQuery{}, err
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
	all := newRebuildMemberGroup(scope)
	reachable := newRebuildMemberGroup(scope)
	// D71 callers use the native direct-import cut and ordered member
	// fallbacks. Other callers retain the existing refusal paths.
	selected := lang == "go" || lang == "javascript" || lang == "typescript"
	// The receipt selector already admitted the recording file. A detached
	// dataflow source still has native file reachability, but no caller scope.
	if caller == nil && graph.IsUnresolvedTarget(edge.From) && edge.FilePath != "" &&
		(edge.Kind == graph.EdgeArgOf || edge.Kind == graph.EdgeValueFlow) {
		selected = true
	}
	var dirs map[string]struct{}
	if selected && name != "" {
		dirs, err = f.reachableDirectories(ctx, edge.FilePath)
		if err != nil {
			return rebuildMainQuery{}, err
		}
	}
	_, hasTypeArgs := metaIntValue(edge.Meta["type_arg_count"])
	_, hasArgs := metaIntValue(edge.Meta["arg_count"])
	member, _ := edge.Meta["member_call"].(bool)
	bound := f.providesFor[receiver]
	var firstDir string
	mixedDirs := false
	callerMissing := caller == nil
	callerFile := ""
	if caller != nil {
		callerFile = caller.FilePath
	}
	phpCaller := phpTypedReceiverCaller(caller)
	query := rebuildMainQuery{charge: len(callerRecv) + len(callerScope) + len(callerFile) + len(scope.namespace) + len(scope.owner) + len(scope.callerFile) + len(scope.phpKind)}
	for _, arg := range scope.cppArgs {
		query.charge += len(arg)
	}
	for ns := range scope.cppADL {
		query.charge += len(ns)
	}
	if name != "" {
		_, langs := resolverCompatibleLanguages(lang)
		query.request = store_pebble.LinkRequest{Kind: store_pebble.LinkName, Key: name, RepoPrefix: repo, Languages: langs}
		query.consider = func(n *graph.Node) error {
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
					(lang != "go" || callerFile != "" && n.FilePath != "") && rebuildFieldPackage(n) == callerScope {
					keepFirstRebuildField(&fields[2], n.ID)
				}
				if candidateDir == dir {
					keepFirstRebuildField(&fields[3], n.ID)
				}
			}
			if all.all.count == 0 {
				firstDir = candidateDir
			}
			mixedDirs = mixedDirs || firstDir != candidateDir
			exact := receiver != "" && recv == receiver && n.Kind == graph.KindMethod
			callerMethod := !member && callerRecv != "" && recv == callerRecv && n.Kind == graph.KindMethod
			_, di := bound[recv]
			di = receiver != "" && di && n.Kind == graph.KindMethod
			locality := !isCSharpExtension(n) && (!member || n.ID != edge.From)
			typeOK := hasTypeArgs && csharpAcceptsTypeArgCount(edge, n)
			argOK := hasArgs && csharpMethodAcceptsArgCount(edge, n)
			all.consider(n, candidateDir == dir, exact, di, callerMethod, locality, typeOK, argOK)
			if _, ok := dirs[candidateDir]; ok {
				reachable.consider(n, candidateDir == dir, exact, di, callerMethod, locality, typeOK, argOK)
			}
			return nil
		}
	}
	query.finish = func() error {
		for i, id := range fields {
			if id != "" {
				edge.To, edge.Confidence = id, [4]float64{0.95, 0.85, 0.85, 0.6}[i]
				return nil
			}
		}

		refuse := func(reason string) error {
			return fmt.Errorf("%w: %s for wildcard member %q in %q", errRebuildMainRule, reason, name, edge.FilePath)
		}
		if phpCaller && (phpEdgeMetaString(edge, "php_property_path") != "" || phpEdgeMetaString(edge, "receiver_fqn") != "") {
			return refuse("PHP property or FQN receiver lookup")
		}

		raw := all.narrow()
		filtered := raw
		// Native first filters the whole compatible name group, then applies
		// applicability independently to both groups. A filter miss keeps all.
		if reachable.all.count != 0 {
			filtered = reachable.narrow()
		}
		// With one candidate directory, reachability keeps all or discards all
		// and then restores all. With no recorded edge path native skips it.
		// Otherwise scope (unlike the exact tiers) sees the filtered group.
		reachMayNarrow := !selected && edge.FilePath != "" && mixedDirs
		e := *edge
		picked := false
		if raw.count != 0 {
			if reachMayNarrow {
				pick, missing := all.all.scope.pick()
				if pick.id != "" || missing != "" || all.all.scope.cppCount != 0 {
					return refuse("scope selection needs native page direct-import reachability")
				}
			} else {
				pick, missing := filtered.scope.pick()
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
				case reachMayNarrow && (all.all.method || all.all.locality):
					return refuse("native page direct-import reachability")
				case receiver != "" && raw.method && (!selected || f.providesFor == nil):
					return refuse("native first-use useClass bindings")
				case selected && filtered.diMethod != "":
					picked = true
					e.To, e.Confidence = filtered.diMethod, 0.9
					e.Meta = maps.Clone(e.Meta)
					if e.Meta == nil {
						e.Meta = make(map[string]any, 1)
					}
					e.Meta["resolution"] = "useClass_binding"
				case filtered.callerMethod && !selected:
					return refuse("caller-receiver method fallback")
				case selected && filtered.dirCallerMethod != "":
					picked = true
					e.To, e.Confidence = filtered.dirCallerMethod, 0.9
				case selected && filtered.firstCallerMethod != "":
					picked = true
					e.To, e.Confidence = filtered.firstCallerMethod, 0.8
				case lang == "csharp" && raw.extension:
					return refuse("C# extension visibility and hierarchy evidence")
				case receiver == "" && filtered.methodCount > 1:
					return nil // native leaves an untyped ambiguous member unresolved
				case filtered.locality && !selected:
					return refuse("method locality or interface dispatch fallback")
				case filtered.locality:
					// Native marks a typed ambiguous interface pick before locality.
					if receiver != "" && filtered.methodCount > 1 {
						isInterface, err := rebuildMemberReceiverIsInterface(ctx, f, edge, receiver, repo, lang)
						if err != nil {
							return err
						}
						if isInterface {
							e.Meta = maps.Clone(e.Meta)
							if e.Meta == nil {
								e.Meta = make(map[string]any, 1)
							}
							e.Meta["dispatch"] = "interface"
							e.Origin = graph.OriginASTInferred
						}
					}
					if filtered.methodCount == 1 && loneMemberLang(filtered.firstMethodLanguage) && e.Origin == "" {
						e.Origin = graph.OriginASTInferred
						if e.Confidence == 0 {
							e.Confidence = 0.7
						}
					}
					for _, id := range [...]string{filtered.dirMethod, filtered.firstMethod, filtered.dirFunc, filtered.firstFunc} {
						if id != "" {
							picked, e.To = true, id
							if e.Origin == "" {
								e.Origin = graph.OriginTextMatched
							}
							break
						}
					}
				}
			}
		}
		if !picked {
			// Pinned applyBuiltinIfKnown changes only To. Its repo spelling is
			// different from name lookup only when the source node is missing.
			if language := langFromFilePath(e.FilePath); language != "" {
				if category, ok := classifyBuiltin(name, language); ok {
					if callerMissing {
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
	return query, nil
}

// The native receiver interface predicate is a compatible same-repo name
// lookup, not a test of structural method satisfaction or reachability.
func rebuildMemberReceiverIsInterface(ctx context.Context, f *rebuildLinkFrontier, edge *graph.Edge, receiver, repo, lang string) (bool, error) {
	_, langs := resolverCompatibleLanguages(lang)
	req := store_pebble.LinkRequest{Kind: store_pebble.LinkName, Key: receiver, RepoPrefix: repo, Languages: langs}
	var found bool
	_, err := f.candidates(ctx, []store_pebble.LinkRequest{req}, func(_ int, n *graph.Node) error {
		one := [1]*graph.Node{n}
		if n.Kind == graph.KindInterface && len(genericInstantiationOnly(edge, withoutAttributedTerminals(one[:]))) != 0 {
			found = true
		}
		return nil
	})
	return found, err
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

// Each group independently keeps the four applicability summaries; no
// candidate nodes survive the callback.
type rebuildMemberGroup struct {
	all, typed, arity, typedArity rebuildMemberChoices
}

func newRebuildMemberGroup(scope rebuildFunctionScope) rebuildMemberGroup {
	base := rebuildMemberChoices{scope: scope}
	return rebuildMemberGroup{base, base, base, base}
}

func (g *rebuildMemberGroup) consider(n *graph.Node, sameDir, exact, di, callerMethod, locality, typeOK, argOK bool) {
	g.all.consider(n, sameDir, exact, di, callerMethod, locality)
	if typeOK {
		g.typed.consider(n, sameDir, exact, di, callerMethod, locality)
	}
	if argOK {
		g.arity.consider(n, sameDir, exact, di, callerMethod, locality)
		if typeOK {
			g.typedArity.consider(n, sameDir, exact, di, callerMethod, locality)
		}
	}
}

func (g *rebuildMemberGroup) narrow() *rebuildMemberChoices {
	choices, withArity := &g.all, &g.arity
	if choices.count > 1 && g.typed.count != 0 {
		choices, withArity = &g.typed, &g.typedArity
	}
	if choices.count > 1 && withArity.count != 0 {
		choices = withArity
	}
	return choices
}

type rebuildMemberChoices struct {
	scope                                                rebuildFunctionScope
	count, methodCount                                   int
	exact, localExact                                    string
	dirMethod, firstMethod, firstMethodLanguage          string
	dirFunc, firstFunc                                   string
	diMethod, dirCallerMethod, firstCallerMethod         string
	ambiguous, method, callerMethod, extension, locality bool
}

func (s *rebuildMemberChoices) consider(n *graph.Node, sameDir, exact, di, callerMethod, locality bool) {
	s.count++
	s.scope.consider(n)
	s.method = s.method || n.Kind == graph.KindMethod
	if di && s.diMethod == "" {
		s.diMethod = n.ID
	}
	if callerMethod {
		s.callerMethod = true
		if s.firstCallerMethod == "" {
			s.firstCallerMethod = n.ID
		}
		if sameDir && s.dirCallerMethod == "" {
			s.dirCallerMethod = n.ID
		}
	}
	s.extension = s.extension || isCSharpExtension(n)
	if locality && (n.Kind == graph.KindFunction || n.Kind == graph.KindMethod) {
		s.locality = true
		if n.Kind == graph.KindMethod {
			s.methodCount++
			if s.firstMethod == "" {
				s.firstMethod, s.firstMethodLanguage = n.ID, n.Language
			}
			if sameDir && s.dirMethod == "" {
				s.dirMethod = n.ID
			}
		} else {
			if s.firstFunc == "" {
				s.firstFunc = n.ID
			}
			if sameDir && s.dirFunc == "" {
				s.dirFunc = n.ID
			}
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

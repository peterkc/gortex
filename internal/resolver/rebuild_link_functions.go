package resolver

import (
	"context"
	"fmt"
	"go/token"
	"maps"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// resolveRebuildFunction handles the default bare-name branch and checked
// full-name misses for typed extractor markers. The caller owns dispatch,
// terminal clearing on a move, and applying the complete page. A refused call
// leaves even this tentative edge unchanged.
func resolveRebuildFunction(ctx context.Context, f *rebuildLinkFrontier, edge *graph.Edge, target string) error {
	q, err := prepareRebuildFunction(ctx, f, edge, target)
	if err != nil {
		return err
	}
	return q.run(ctx, f)
}

func prepareRebuildFunction(ctx context.Context, f *rebuildLinkFrontier, edge *graph.Edge, target string) (rebuildMainQuery, error) {
	checkedMiss := ""
	if edge.Kind == graph.EdgeRendersChild || strings.ContainsAny(target, `.\:`) {
		name, _ := edge.Meta["name"].(string)
		switch {
		case edge.Kind == graph.EdgeReads && edge.Origin == graph.OriginSpeculative &&
			edge.Meta["via"] == valueRefCandidateVia && isDistinctiveValueName(name) &&
			!strings.ContainsAny(name, `.\:`) && target == "valueref::"+name:
			checkedMiss = "value-ref"
		default:
			checkedMiss = rebuildFrameworkFullNameMissFamily(edge, target)
			if checkedMiss == "" {
				checkedMiss = rebuildGoFullNameMissFamily(edge, target)
			}
		}
		if checkedMiss == "" {
			return rebuildMainQuery{}, fmt.Errorf("%w: non-bare call %q in %q", errRebuildMainRule, target, edge.FilePath)
		}
	}
	e := *edge
	if checkedMiss == "" {
		csharpUsing, _ := e.Meta["resolution"].(string)
		if csharpUsing == "using_static" {
			e.Meta = maps.Clone(e.Meta)
			csharpDropStaleUsingStaticTag(&e)
		}
		if target == "" || csharpLocalShadowed(&e) {
			*edge = e
			return rebuildMainQuery{}, nil
		}
	}

	caller, err := f.view.Node(ctx, e.From)
	if err != nil {
		return rebuildMainQuery{}, err
	}
	var repo, lang string
	if caller != nil {
		repo, lang = caller.RepoPrefix, caller.Language
	}
	_, langs := resolverCompatibleLanguages(lang)
	req := store_pebble.LinkRequest{Kind: store_pebble.LinkName, Key: target, RepoPrefix: repo, Languages: langs}
	if checkedMiss != "" {
		// Only a checked full-name miss permits the original row to remain unchanged.
		matched := false
		return rebuildMainQuery{request: req, consider: func(n *graph.Node) error {
			matched = matched || rebuildFunctionCandidateEligible(edge, n)
			return nil
		}, finish: func() error {
			if matched {
				return fmt.Errorf("%w: non-bare %s candidate %q in %q", errRebuildMainRule, checkedMiss, target, edge.FilePath)
			}
			return nil
		}}, nil
	}

	resolution, _ := e.Meta["resolution"].(string)
	receiver, _ := e.Meta["receiver_type"].(string)
	builtin, _ := e.Meta["receiver_builtin"].(string)
	member, _ := e.Meta["member_call"].(bool)
	member = member || receiver != "" || builtin != "" || resolution == "extension_method"
	_, hasTypeArgs := metaIntValue(e.Meta["type_arg_count"])
	_, hasArgs := metaIntValue(e.Meta["arg_count"])
	web := isJSTSPath(e.FilePath)
	dir := filePathDir(e.FilePath)
	var all, typed, arity, typedArity rebuildFunctionChoices
	var extension bool
	scope := newRebuildFunctionScope(&e, caller, target)
	if scope.lang == "cpp" {
		scope.rankedCpp = &rebuildCppOverload{argHints: scope.cppArgs}
	}
	charge := len(scope.namespace) + len(scope.owner) + len(scope.callerFile) + len(scope.phpKind)
	for _, arg := range scope.cppArgs {
		charge += len(arg)
	}
	for ns := range scope.cppADL {
		charge += len(ns)
	}
	return rebuildMainQuery{request: req, charge: charge, consider: func(n *graph.Node) error {
		if !rebuildFunctionCandidateEligible(&e, n) {
			return nil
		}
		scope.consider(n)
		extension = extension || isCSharpExtension(n)
		all.consider(&e, n, target, dir, web, member)
		typeOK := hasTypeArgs && csharpAcceptsTypeArgCount(&e, n)
		argOK := hasArgs && csharpMethodAcceptsArgCount(&e, n)
		if typeOK {
			typed.consider(&e, n, target, dir, web, member)
		}
		if argOK {
			arity.consider(&e, n, target, dir, web, member)
			if typeOK {
				typedArity.consider(&e, n, target, dir, web, member)
			}
		}
		return nil
	}, finish: func() error {
		if all.count == 0 {
			*edge = e
			return nil
		}
		var pick rebuildFunctionPick
		if scope.rankedCpp != nil {
			var err error
			pick, err = scope.rankedCpp.pick(ctx, f, req, &e)
			if err != nil {
				return err
			}
		}
		if pick.id == "" {
			var missing string
			pick, missing = scope.pick()
			if missing != "" {
				return fmt.Errorf("%w: %s for %q in %q", errRebuildMainRule, missing, target, e.FilePath)
			}
		}
		tag := ""
		if pick.id != "" {
			e.Origin, e.Confidence, tag = graph.OriginASTResolved, 0.92, "scope"
		} else {
			if member && lang == "csharp" && extension {
				return fmt.Errorf("%w: C# extension evidence for %q in %q", errRebuildMainRule, target, e.FilePath)
			}
			if resolution == "extension_method" {
				e.Meta = maps.Clone(e.Meta)
				delete(e.Meta, "resolution")
			}

			// Each applicability filter narrows only a multi-candidate set
			// and keeps the input on a miss. It counts non-callable nodes too.
			choices, withArity := &all, &arity
			if all.count > 1 && typed.count > 0 {
				choices, withArity = &typed, &typedArity
			}
			if choices.count > 1 && withArity.count > 0 {
				choices = withArity
			}
			switch {
			case choices.file.id != "":
				pick = choices.file
				if choices.fileCount == 1 && e.Kind == graph.EdgeCalls && e.Origin == "" {
					e.Origin, e.Confidence = graph.OriginASTResolved, 0.9
				}
			case !member && strings.HasSuffix(e.FilePath, ".cs"):
				// Native reads (and caches) visibility even before checking
				// whether any surviving candidate is using-static importable.
				return fmt.Errorf("%w: C# first-use visibility for %q in %q", errRebuildMainRule, target, e.FilePath)
			case choices.importable && !choices.localShadow:
				return fmt.Errorf("%w: JS/TS import closure for %q in %q", errRebuildMainRule, target, e.FilePath)
			case choices.dir.id != "":
				pick = choices.dir
				if choices.dirCount == 1 && e.Kind == graph.EdgeCalls && e.Origin == "" {
					e.Origin, e.Confidence = graph.OriginASTResolved, 0.9
				}
			case choices.first.id != "":
				pick = choices.first
				if e.Origin == "" {
					e.Origin = graph.OriginTextMatched
				}
			case choices.dirValues == 1 || choices.values == 1:
				pick = choices.value
				if choices.dirValues == 1 {
					pick = choices.dirValue
				}
				e.Origin, e.Confidence, tag = graph.OriginASTInferred, 0.7, "value_callee"
			}
		}

		if tag != "" {
			e.Meta = maps.Clone(e.Meta)
			if e.Meta == nil {
				e.Meta = make(map[string]any, 1)
			}
			e.Meta["resolution"] = tag
		}
		if pick.id != "" && pick.id != e.To {
			e.To = pick.id
			if e.Kind == graph.EdgeReads && (pick.kind == graph.KindFunction || pick.kind == graph.KindMethod) {
				e.Kind = graph.EdgeReferences
			}
		}
		*edge = e
		return nil
	}}, nil
}

// The Go extractor emits executor-field Calls from keyed string composites
// and AST-inferred Sends/Recvs from the channel expression's source text.
// This identifies only those typed shapes, not arbitrary qualified names.
func rebuildGoFullNameMissFamily(edge *graph.Edge, target string) string {
	if !strings.HasSuffix(edge.FilePath, ".go") || edge.Meta["resolution"] == "using_static" || csharpLocalShadowed(edge) {
		return ""
	}
	switch edge.Kind {
	case graph.EdgeCalls:
		if edge.Meta["via"] == "temporal.executor-field" {
			typ, _ := edge.Meta["executor_type"].(string)
			field, _ := edge.Meta["executor_field"].(string)
			_, valueOK := edge.Meta["executor_value"].(string)
			if valueOK && token.IsIdentifier(typ) && token.IsIdentifier(field) && target == "temporal-executor::"+typ+"::"+field {
				return "temporal executor-field"
			}
		}
	case graph.EdgeRecvs, graph.EdgeSends:
		if edge.Origin == graph.OriginASTInferred && strings.Contains(target, ".") &&
			!strings.ContainsAny(target, `\:`) && strings.TrimSpace(target) == target {
			return "channel expression"
		}
	}
	return ""
}

// Native main uses the full name for these placeholders. Their ordinary
// miss must keep the original row for the graph-wide framework pass, but a
// real name candidate still requires its own binding rule.
func rebuildFrameworkFullNameMissFamily(edge *graph.Edge, target string) string {
	if edge.Meta["resolution"] == "using_static" || csharpLocalShadowed(edge) {
		return ""
	}
	if edge.Kind == graph.EdgeCalls && isJSTSPath(edge.FilePath) && edge.Meta["via"] == rnNativeVia {
		module, _ := edge.Meta["rn_module"].(string)
		method, _ := edge.Meta["rn_method"].(string)
		if module != "" && method != "" && target == "rn::"+module+"::"+method {
			return "React Native bridge"
		}
	}
	goFile, javaFile := strings.HasSuffix(edge.FilePath, ".go"), strings.HasSuffix(edge.FilePath, ".java")
	if !goFile && !javaFile {
		return ""
	}
	kind, _ := edge.Meta["temporal_kind"].(string)
	name, _ := edge.Meta["temporal_name"].(string)
	if edge.Kind == graph.EdgeCalls && name != "" && target == "temporal::"+kind+"::"+name {
		switch edge.Meta["via"] {
		case "temporal.stub":
			if kind == "workflow" || (goFile && kind == "activity") {
				return "Temporal dispatch"
			}
		case "temporal.start":
			if javaFile && kind == "workflow" {
				return "Temporal start"
			}
		}
	}
	// Go emits a separate same-target Spawn with mode but without the
	// Call's Temporal metadata. Native applies the same full-name lookup.
	if goFile && edge.Kind == graph.EdgeSpawns && edge.Origin == graph.OriginASTResolved && edge.Meta["mode"] == "goroutine" {
		if name, ok := strings.CutPrefix(target, "temporal::workflow::"); ok && name != "" {
			return "Temporal spawn"
		}
		if name, ok := strings.CutPrefix(target, "temporal::activity::"); ok && name != "" {
			return "Temporal spawn"
		}
	}
	return ""
}

type rebuildFunctionPick struct {
	id   string
	kind graph.NodeKind
}

type rebuildFunctionChoices struct {
	first, file, dir, value, dirValue rebuildFunctionPick
	count, fileCount, dirCount        int
	values, dirValues                 int
	localShadow, importable           bool
}

func (c *rebuildFunctionChoices) consider(e *graph.Edge, n *graph.Node, target, dir string, web, member bool) {
	c.count++
	pick := rebuildFunctionPick{n.ID, n.Kind}
	sameDir := filePathDir(n.FilePath) == dir
	if web {
		c.localShadow = c.localShadow || (n.FilePath == e.FilePath && n.Kind != graph.KindImport)
		c.importable = c.importable || isImportableCallee(n, target)
		if (n.Kind == graph.KindVariable || n.Kind == graph.KindConstant) && n.ID == n.FilePath+"::"+target {
			c.value = pick
			c.values++
			if sameDir {
				c.dirValue = pick
				c.dirValues++
			}
		}
	}
	if (n.Kind != graph.KindFunction && n.Kind != graph.KindMethod) || (member && isCSharpExtension(n)) {
		return
	}
	if c.first.id == "" {
		c.first = pick
	}
	if n.FilePath != "" && n.FilePath == e.FilePath {
		if c.file.id == "" {
			c.file = pick
		}
		c.fileCount++
	}
	if sameDir {
		if c.dir.id == "" {
			c.dir = pick
		}
		c.dirCount++
	}
}

// Scope reads the caller and raw eligible name group, before applicability or
// locality. Keep only the first match per native tier.
type rebuildFunctionScope struct {
	lang, callerFile, namespace, owner, phpKind string
	cppArgs                                     []string
	cppADL                                      map[string]struct{}
	cppCount                                    int
	rankedCpp                                   *rebuildCppOverload
	cppOverload, sameScope, adl                 rebuildFunctionPick
	method, phpAlias                            bool
}

func newRebuildFunctionScope(e *graph.Edge, caller *graph.Node, target string) (s rebuildFunctionScope) {
	if caller == nil {
		return s
	}
	s.lang = caller.Language
	switch s.lang {
	case "c":
		s.callerFile = caller.FilePath
	case "cpp":
		s.namespace = scopeMetaString(caller.Meta, MetaScopeNamespace)
		s.cppArgs = scopePositionalArgHints(e.Meta)
		for _, hint := range scopeArgTypeHints(e.Meta) {
			if ns := splitNamespaceFromQualifiedName(hint); ns != "" {
				if s.cppADL == nil {
					s.cppADL = map[string]struct{}{}
				}
				s.cppADL[ns] = struct{}{}
			}
		}
	case "java":
		s.owner = callerEnclosingClass(caller)
		if rt := edgeReceiverType(e); rt != "" && rt != s.owner {
			s.owner = ""
		}
	case "php":
		s.phpKind = scopeMetaString(e.Meta, MetaScopeKind)
		switch s.phpKind {
		case ScopeKindSelf:
			s.owner = callerEnclosingClass(caller)
		case ScopeKindParent:
		default:
			s.namespace = scopeMetaString(caller.Meta, MetaScopeNamespace)
			s.phpAlias = scopeUseAliases(e.Meta)[target] != ""
		}
	}
	return s
}

func (s *rebuildFunctionScope) consider(n *graph.Node) {
	if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
		return
	}
	pick := rebuildFunctionPick{n.ID, n.Kind}
	s.method = s.method || n.Kind == graph.KindMethod
	match := false
	switch s.lang {
	case "c":
		match = n.Kind == graph.KindFunction && n.FilePath == s.callerFile && scopeMetaBool(n.Meta, MetaScopeStatic)
	case "cpp":
		if s.rankedCpp != nil {
			s.rankedCpp.consider(n)
		} else if sig, ok := parseCppCandidate(n); ok && cppArityCompatible(sig, len(s.cppArgs)) {
			s.cppOverload = pick
			s.cppCount++
		}
		ns := scopeMetaString(n.Meta, MetaScopeNamespace)
		match = s.namespace != "" && s.namespace == ns
		if _, found := s.cppADL[ns]; found && s.adl.id == "" {
			s.adl = pick
		}
	case "java":
		match = s.enclosingMatch(n)
	case "php":
		match = s.enclosingMatch(n) || (n.Kind == graph.KindFunction && s.namespace != "" &&
			scopeMetaString(n.Meta, MetaScopeNamespace) == s.namespace)
	}
	if match && s.sameScope.id == "" {
		s.sameScope = pick
	}
}

func (s *rebuildFunctionScope) enclosingMatch(n *graph.Node) bool {
	return s.owner != "" && n.Kind == graph.KindMethod &&
		(scopeMetaString(n.Meta, MetaScopeClass) == s.owner || receiverEquals(n, s.owner))
}

func (s *rebuildFunctionScope) pick() (rebuildFunctionPick, string) {
	switch {
	case s.phpAlias:
		return rebuildFunctionPick{}, "PHP function alias"
	case s.lang == "php" && s.phpKind == ScopeKindParent && s.method:
		return rebuildFunctionPick{}, "PHP parent resolution"
	case s.cppCount == 1:
		return s.cppOverload, ""
	case s.cppCount > 1 && len(s.cppArgs) != 0:
		return rebuildFunctionPick{}, "C++ ranked overloads"
	case s.sameScope.id != "":
		return s.sameScope, ""
	case s.lang == "java" && s.owner != "" && s.method:
		return rebuildFunctionPick{}, "Java parent class resolution"
	}
	return s.adl, ""
}

package resolver

import (
	"context"
	"fmt"
	"iter"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildExternalCallSynthesis elects over live post-framework candidates.
// Receipts and skipped-pass candidates only prove whether that election is safe.
func RunRebuildExternalCallSynthesis(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error], enabled bool, requested, selection FrameworkSynthesizerSelection, rewriteNpm func(string, string) (string, error)) (uint64, error) {
	if !enabled {
		return 0, nil
	}
	return b.RebuildExternalCallSynthesis(ctx, receipts, store_pebble.RebuildExternalCallRules{
		Candidate: func(e *graph.Edge, caller *graph.Node) (store_pebble.RebuildExternalCallCandidate, bool) {
			return rebuildExternalCallCandidate(e.To, externalCallCallerLanguage(caller, e.FilePath))
		},
		Maybe: maybeRebuildExternalCall,
		MayChangeLive: func(ctx context.Context, view store_pebble.RebuildView, e *graph.Edge, caller *graph.Node) (bool, bool, error) {
			return mayChangeRebuildExternalCall(ctx, view, e, caller, requested, selection)
		},
		IndexedEdge: rebuildExternalIndexedEdge,
		IndexedNode: func(n *graph.Node) bool {
			_, _, ok := parseExternalCallTarget(n.ID)
			return ok
		},
		IndexedCandidate: rebuildExternalIndexedCandidate,
		Extra: func(ctx context.Context, view store_pebble.RebuildView, yield func(*graph.Edge, store_pebble.RebuildExternalCallCandidate) error) error {
			if err := visitRebuildAliasedExternalCalls(ctx, view, receipts, rewriteNpm, yield); err != nil {
				return err
			}
			return visitRebuildSkippedExternalCalls(ctx, view, selection, yield)
		},
		Rebind: rebindExternalCallEdge,
	})
}

func rebuildExternalCallCandidate(to, language string) (store_pebble.RebuildExternalCallCandidate, bool) {
	ecosystem, path, ok := parseExternalCallTarget(to)
	if !ok || isLanguageStdlib(language, path) {
		return store_pebble.RebuildExternalCallCandidate{}, false
	}
	id := externalCallNodeID(ecosystem, path)
	return store_pebble.RebuildExternalCallCandidate{
		Node: newExternalCallNode(id, ecosystem, path, language), Language: language, OldTo: to,
	}, true
}

func maybeRebuildExternalCall(e *graph.Edge, caller *graph.Node, extractor string) []store_pebble.RebuildExternalCallCandidate {
	if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeReferences || !deferredExternalCallExtractor(extractor) {
		return nil
	}
	// Main never rebinds a resolved terminal. The skipped framework turns
	// have their own checked live-target tests below.
	if !graph.IsUnresolvedTarget(e.To) {
		return nil
	}
	lang := externalCallCallerLanguage(caller, e.FilePath)
	name := graph.UnresolvedName(e.To)
	var to string
	switch {
	case strings.HasPrefix(name, "extern::"):
		spec := strings.TrimPrefix(name, "extern::")
		path, symbol, sep := splitExternSpec(spec)
		switch {
		case !sep:
			to = "external::" + spec
		case isStdlibLike(path):
			to = graph.StubID(stubRepoPrefixForCaller(e.From, caller), graph.StubKindStdlib, path, symbol)
		default:
			to = "dep::" + spec
		}
	case strings.HasPrefix(name, "import::") && !isGodotResPlaceholder(e.To) && !isLuaRequireEdge(e):
		to = "external::" + strings.TrimPrefix(name, "import::")
	}
	candidate, ok := rebuildExternalCallCandidate(to, lang)
	if !ok {
		return nil
	}
	return []store_pebble.RebuildExternalCallCandidate{candidate}
}

func deferredExternalCallExtractor(language string) bool {
	return language != "" && !store_pebble.IsRebuildExternalCallRetained(language)
}

// A configured extractor can record a Call or Reference at a JS/TS path.
// The native alias query uses that edge path, not the extractor's language.
func visitRebuildAliasedExternalCalls(ctx context.Context, view store_pebble.RebuildView,
	receipts iter.Seq2[store_pebble.ParseFacts, error], rewrite func(string, string) (string, error),
	yield func(*graph.Edge, store_pebble.RebuildExternalCallCandidate) error,
) error {
	if rewrite == nil {
		return nil
	}
	for f, err := range receipts {
		if err != nil {
			return err
		}
		if !deferredExternalCallExtractor(f.EffectiveLanguage) {
			continue
		}
		for _, e := range f.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeReferences || !IsNpmAliasImporter(e.FilePath) ||
				isGodotResPlaceholder(e.To) || isLuaRequireEdge(e) {
				continue
			}
			spec, ok := strings.CutPrefix(graph.UnresolvedName(e.To), "import::")
			if !ok || spec == "" || strings.HasPrefix(spec, ".") || strings.HasPrefix(spec, "/") {
				continue
			}
			real, err := rewrite(e.FilePath, spec)
			if err != nil {
				return err
			}
			if real == "" || real == spec {
				continue
			}
			caller, err := view.ExternalCallNode(ctx, e.From)
			if err != nil {
				return err
			}
			if c, ok := rebuildExternalCallCandidate("external::"+real, externalCallCallerLanguage(caller, e.FilePath)); ok {
				if err := yield(e, c); err != nil {
					return err
				}
			}
		}
	}
	return ctx.Err()
}

// A configured extractor can give an ordinary declaration a terminal-shaped ID.
// Collect only the facts that can select that declaration. The store owns the
// disk-backed receipt projection and joins it to one node scan.
func rebuildExternalIndexedEdge(raw *graph.Edge) (e *graph.Edge, names, quals []string, directory bool) {
	if raw == nil || raw.Kind != graph.EdgeCalls && raw.Kind != graph.EdgeReferences && raw.Kind != graph.EdgeReads {
		return nil, nil, nil, false
	}
	name := graph.UnresolvedName(raw.To)
	// Main does not bind a resolved target. The gRPC and Python relative
	// import branches leave the key for their dedicated passes, and the
	// latter only changes Imports, not a Call or Reference. Native Vue
	// template import binding normally targets file::symbol IDs (including
	// basename-matched component declarations), not package terminals.
	// Arbitrary configured-extractor IDs are not proved by that spelling.
	if name == "" || strings.HasPrefix(name, "grpc::") || strings.HasPrefix(name, "pyrel::") {
		return nil, nil, nil, false
	}
	e = &graph.Edge{From: raw.From, To: raw.To, Kind: raw.Kind, FilePath: raw.FilePath, Line: raw.Line}
	for _, key := range [...]string{"eloquent_method", "fqn", "target_fqn", "rust_path"} {
		if value, ok := raw.Meta[key].(string); ok {
			if e.Meta == nil {
				e.Meta = make(map[string]any)
			}
			e.Meta[key] = value
		}
	}
	// Keep the lookup keys a superset of every branch of
	// rebuildExternalIndexedLookup, including empty name/qualname keys
	// where a malformed terminal can match them. The join still runs
	// that predicate on each candidate.
	if method := phpEdgeMetaString(e, "eloquent_method"); method != "" {
		names = append(names, method)
	}
	for _, key := range [...]string{"fqn", "target_fqn"} {
		if leaf := rebuildExternalPHPName(e, key); leaf != "" {
			names = append(names, leaf)
		}
	}
	switch {
	case strings.HasPrefix(name, "extern::"):
		spec := strings.TrimPrefix(name, "extern::")
		if path, leaf, ok := splitExternSpec(spec); ok {
			names = append(names, leaf)
			if imported := pythonImportedName(path); imported != "" && isPythonSourcePath(e.FilePath) && isDottedPythonModule(path) {
				names = append(names, imported)
			}
		}
	case strings.HasPrefix(name, "import::"):
		if !isGodotResPlaceholder(e.To) && !isLuaRequireEdge(e) {
			spec := strings.TrimPrefix(name, "import::")
			path, leaf := splitImportSpecSymbol(spec)
			quals = append(quals, spec, path)
			if leaf != "" {
				names = append(names, leaf)
			}
			directory = true
		}
	case strings.HasPrefix(name, "*."):
		names = append(names, strings.TrimPrefix(name, "*."))
	default:
		names = append(names, name)
	}
	return e, names, quals, directory
}

func rebuildExternalPHPName(e *graph.Edge, key string) string {
	fqn := phpEdgeMetaString(e, key)
	if i := strings.LastIndex(fqn, `\`); i >= 0 {
		return fqn[i+1:]
	}
	return fqn
}

func rebuildExternalIndexedCandidate(ctx context.Context, view store_pebble.RebuildView, raw *graph.Edge, n *graph.Node) (*graph.Edge, store_pebble.RebuildExternalCallCandidate, bool, error) {
	var none store_pebble.RebuildExternalCallCandidate
	if !rebuildExternalIndexedLookup(raw, n) {
		return nil, none, false, nil
	}
	caller, err := view.ExternalCallNode(ctx, raw.From)
	if err != nil || caller != nil && n.RepoPrefix != caller.RepoPrefix {
		return nil, none, false, err
	}
	name := graph.UnresolvedName(raw.To)
	if !strings.HasPrefix(name, "import::") {
		// The main name readers discard stdlib/dep synthetic IDs;
		// imports use qualified-name and file identity readers instead.
		if !strings.HasPrefix(name, "extern::") && len(withoutAttributedTerminals([]*graph.Node{n})) == 0 {
			return nil, none, false, nil
		}
		if caller != nil {
			_, languages := resolverCompatibleLanguages(caller.Language)
			if len(languages) != 0 && !slices.Contains(languages, n.Language) {
				return nil, none, false, nil
			}
		}
	}
	e := *raw
	if e.Kind == graph.EdgeReads {
		e.Kind = graph.EdgeReferences
	}
	c, ok := rebuildExternalCallCandidate(n.ID, externalCallCallerLanguage(caller, e.FilePath))
	return &e, c, ok, nil
}

func rebuildExternalIndexedLookup(e *graph.Edge, n *graph.Node) bool {
	if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeReferences && e.Kind != graph.EdgeReads {
		return false
	}
	name := graph.UnresolvedName(e.To)
	if name == "" || strings.HasPrefix(name, "grpc::") || strings.HasPrefix(name, "pyrel::") {
		return false
	}
	callable := n.Kind == graph.KindFunction || n.Kind == graph.KindMethod
	// Main promotes Reads only in its bare-name and selector branches,
	// and only on a callable. Extern and import keep their original kind.
	if e.Kind == graph.EdgeReads && (!callable || strings.HasPrefix(name, "extern::") || strings.HasPrefix(name, "import::")) {
		return false
	}
	if n.Language == "php" {
		if method := phpEdgeMetaString(e, "eloquent_method"); n.Kind == graph.KindMethod && method != "" && method == n.Name {
			return true
		}
		for _, key := range [...]string{"fqn", "target_fqn"} {
			leaf := rebuildExternalPHPName(e, key)
			if leaf != "" && leaf == n.Name && (n.Kind == graph.KindType || n.Kind == graph.KindInterface || n.Kind == graph.KindFunction) {
				return true
			}
		}
	}
	if spec, ok := strings.CutPrefix(name, "extern::"); ok {
		path, leaf, ok := splitExternSpec(spec)
		if !ok {
			return false
		}
		// Python also reads a module attribute named by the final segment
		// of the path, not the alias spelling at the Call site.
		if isPythonSourcePath(e.FilePath) && isDottedPythonModule(path) && (n.Kind == graph.KindFunction || n.Kind == graph.KindType) {
			if imported := pythonImportedName(path); imported != "" && n.Name == imported &&
				pythonModuleFileMatches(n.FilePath, path[:len(path)-len(imported)-1]) {
				return true
			}
		}
		return leaf == n.Name && (callable || n.Kind == graph.KindType || n.Kind == graph.KindInterface)
	}
	if spec, ok := strings.CutPrefix(name, "import::"); ok {
		if isGodotResPlaceholder(e.To) || isLuaRequireEdge(e) {
			return false
		}
		path, leaf := splitImportSpecSymbol(spec)
		if spec == n.QualName || path == n.QualName || leaf != "" && leaf == n.Name {
			return true
		}
		if n.Kind != graph.KindFile {
			return false
		}
		dir := filePathDir(n.FilePath)
		return dir == path || strings.HasSuffix(dir, "/"+lastPathComponent(path)) || dirMatchesImport(dir, path)
	}
	if method, ok := strings.CutPrefix(name, "*."); ok {
		return callable && n.Name == method
	}
	if rustPath, _ := e.Meta["rust_path"].(string); e.Kind != graph.EdgeReferences && strings.Contains(rustPath, "::") {
		return false // Native sends qualified Rust calls to the Rust scope turn.
	}
	// The JS/TS value-callee fallback accepts only ID == file::name.
	// Such a user node cannot be terminal-shaped unless the recording
	// file spelling itself is a package terminal. Compare that spelling.
	value := isJSTSPath(e.FilePath) && (n.Kind == graph.KindVariable || n.Kind == graph.KindConstant) && n.ID == n.FilePath+"::"+name
	return n.Name == name && (callable || value || e.Kind == graph.EdgeReferences &&
		(n.Kind == graph.KindType || n.Kind == graph.KindInterface))
}

func mayChangeRebuildExternalCall(ctx context.Context, view store_pebble.RebuildView, e *graph.Edge, caller *graph.Node,
	requested, selection FrameworkSynthesizerSelection,
) (remove, refresh bool, err error) {
	// Godot scene connections (84) can rewrite even a resolved reference.
	if selection.allows(SynthGodotConnection) && e.Kind == graph.EdgeReferences {
		target, err := rebuildExternalGodotTarget(ctx, view, e)
		return target != "" && target != e.To, target == e.To, err
	}
	// C# receiver demotion (97) can refresh a qualifying call's payload.
	if requested.none() || e.Kind != graph.EdgeCalls || caller == nil || caller.Language != "csharp" {
		return false, false, nil
	}
	refresh, err = rebuildExternalCSharpDemotion(ctx, view, e, caller)
	return false, refresh, err
}

func rebuildExternalGodotTarget(ctx context.Context, view store_pebble.RebuildView, e *graph.Edge) (string, error) {
	connection, _ := e.Meta["godot_connection"].(bool)
	method, _ := e.Meta["godot_method"].(string)
	if !connection || method == "" {
		return "", nil
	}
	visitRefs := func(id string, yield func(*graph.Edge) (bool, error)) error {
		return view.VisitGuardOutgoing(ctx, id, graph.EdgeReferences, func(e *graph.Edge) error {
			// Keep validating adjacency rows even after the script is ambiguous.
			_, err := yield(e)
			return err
		})
	}
	readNode := func(id string) (*graph.Node, error) { return view.ExternalCallNode(ctx, id) }
	script, err := gdScriptOfSceneNode(e.From, visitRefs, readNode)
	if err != nil || script == "" {
		return "", err
	}
	for _, kind := range gdScriptMemberKinds {
		var id string
		err := view.VisitExternalCallNodes(ctx, func(n *graph.Node) bool {
			file, ok := gdScriptMemberFile(n)
			return ok && file == script && n.Kind == kind && n.Name == method
		}, func(n *graph.Node) error {
			if id == "" || n.ID < id {
				id = n.ID
			}
			return nil
		})
		if err != nil || id != "" {
			return id, err
		}
	}
	return "", nil
}

func visitRebuildSkippedExternalCalls(ctx context.Context, view store_pebble.RebuildView, selection FrameworkSynthesizerSelection,
	yield func(*graph.Edge, store_pebble.RebuildExternalCallCandidate) error,
) error {
	if selection.allows(SynthPascalFormName) {
		if err := visitRebuildPascalExternalCalls(ctx, view, yield); err != nil {
			return err
		}
	}
	var godot, rust bool
	if selection.allows(SynthGodotConnection) || selection.allows(SynthRustScope) {
		if err := view.VisitExternalCallNodes(ctx, func(n *graph.Node) bool {
			_, _, terminal := parseExternalCallTarget(n.ID)
			return terminal && (n.Kind == graph.KindFunction || n.Kind == graph.KindMethod) &&
				(n.Language == "gdscript" && selection.allows(SynthGodotConnection) || n.Language == "rust" && selection.allows(SynthRustScope))
		}, func(n *graph.Node) error {
			godot = godot || n.Language == "gdscript"
			rust = rust || n.Language == "rust"
			return nil
		}); err != nil {
			return err
		}
	}
	if !godot && !rust {
		return nil
	}
	return view.VisitExternalCallEdges(ctx, func(id graph.EdgeIdentity) bool {
		return godot && id.Kind == graph.EdgeReferences || rust && id.Kind == graph.EdgeCalls && graph.IsUnresolvedTarget(id.To)
	}, func(e *graph.Edge, readCaller func() (*graph.Node, error)) (bool, error) {
		if e.Kind == graph.EdgeReferences {
			connection, _ := e.Meta["godot_connection"].(bool)
			method, _ := e.Meta["godot_method"].(string)
			return connection && method != "", nil
		}
		if !rustScopeEdgeCandidate(e) {
			return false, nil
		}
		caller, err := readCaller()
		return caller != nil && caller.Language == "rust", err
	}, func(e *graph.Edge) error {
		if e.Kind == graph.EdgeReferences {
			to, err := rebuildExternalGodotTarget(ctx, view, e)
			if err != nil || to == "" {
				return err
			}
			caller, err := view.ExternalCallNode(ctx, e.From)
			if err != nil {
				return err
			}
			if c, ok := rebuildExternalCallCandidate(to, externalCallCallerLanguage(caller, e.FilePath)); ok {
				return yield(e, c)
			}
			return nil
		}
		caller, err := view.ExternalCallNode(ctx, e.From)
		if err != nil || caller == nil {
			return err
		}
		path, _ := e.Meta["rust_path"].(string)
		qualifier, leaf, qualified := splitExternSpec(path)
		if _, tail, ok := splitExternSpec(qualifier); ok {
			qualifier = tail
		}
		selector := selectorCallName(e.To)
		receiver, _ := e.Meta["receiver_type"].(string)
		recv, _ := e.Meta["rust_recv"].(string)
		expr, _ := e.Meta["rust_recv_expr"].(string)
		if qualified && leaf != "" && receiver == "" && recv != "self" && recv != "Self" &&
			!strings.HasPrefix(expr, "self.") && qualifier != "Self" && !isRustTypeName(qualifier) {
			return visitRebuildRustFreeExternalCall(ctx, view, e, caller, leaf, yield)
		}
		return view.VisitExternalCallNodes(ctx, func(n *graph.Node) bool {
			_, _, terminal := parseExternalCallTarget(n.ID)
			return terminal && n.RepoPrefix == caller.RepoPrefix && n.Language == "rust" &&
				(n.Kind == graph.KindFunction || n.Kind == graph.KindMethod) &&
				(n.Name == leaf && leaf != "" || n.Name == selector && selector != "")
		}, func(n *graph.Node) error {
			if c, ok := rebuildExternalCallCandidate(n.ID, caller.Language); ok {
				return yield(e, c)
			}
			return nil
		})
	})
}

func visitRebuildPascalExternalCalls(ctx context.Context, view store_pebble.RebuildView,
	yield func(*graph.Edge, store_pebble.RebuildExternalCallCandidate) error,
) error {
	return view.VisitExternalCallNodes(ctx, func(n *graph.Node) bool {
		_, _, ok := parseExternalCallTarget(n.ID)
		return ok && n.Kind == graph.KindFile && pascalFormRole(n.FilePath) == pascalDefinitionFile
	}, func(form *graph.Node) error {
		var unit graph.Node
		var lastFormID string
		key := pascalFormKey(form.FilePath)
		if err := view.VisitExternalCallNodes(ctx, func(n *graph.Node) bool {
			return n.Kind == graph.KindFile && pascalFormRole(n.FilePath) != 0 && pascalFormKey(n.FilePath) == key
		}, func(n *graph.Node) error {
			// SQLite fills both maps in ascending file-ID order, last wins.
			if pascalFormRole(n.FilePath) == pascalUnitFile {
				if n.ID > unit.ID {
					unit = *n
				}
			} else if n.ID > lastFormID {
				lastFormID = n.ID
			}
			return nil
		}); err != nil {
			return err
		}
		if lastFormID != form.ID || unit.ID == "" {
			return nil
		}
		e := pascalFormPair(
			graph.FileNodeIdentity{ID: unit.ID, FilePath: unit.FilePath, WorkspaceID: unit.WorkspaceID},
			graph.FileNodeIdentity{ID: form.ID, WorkspaceID: form.WorkspaceID},
		)
		if e == nil {
			return nil
		}
		if c, ok := rebuildExternalCallCandidate(e.To, externalCallCallerLanguage(&unit, e.FilePath)); ok {
			return yield(e, c)
		}
		return nil
	})
}

func rebuildExternalCSharpDemotion(ctx context.Context, view store_pebble.RebuildView, e *graph.Edge, caller *graph.Node) (bool, error) {
	receiver, _ := e.Meta["receiver_type"].(string)
	if receiver == "" || graph.IsUnresolvedTarget(e.To) || e.IsSpeculative() || graph.OriginRank(e.EffectiveOrigin()) > graph.OriginRank(graph.OriginASTInferred) {
		return false, nil
	}
	light, err := view.ExternalCallNode(ctx, e.To)
	if err != nil || light == nil || light.Kind != graph.KindMethod || light.Language != "csharp" {
		return false, err
	}
	target, err := view.ExternalCallTarget(ctx, e.To)
	if err != nil || target == nil {
		return false, err
	}
	owner, _ := target.Meta["receiver"].(string)
	if owner == "" || owner == receiver || isCSharpExtension(target) {
		return false, nil
	}
	types := map[string][]string{}
	for _, name := range [...]string{receiver, owner} {
		if err := view.VisitExternalCallNodes(ctx, func(n *graph.Node) bool {
			return n.Name == name && n.Language == "csharp" && (n.Kind == graph.KindType || n.Kind == graph.KindInterface)
		}, func(n *graph.Node) error {
			if len(types[name]) == frameworkScopeRetainedRowCap {
				return fmt.Errorf("%w: C# external-call receiver types", store_pebble.ErrRebuildLinkInputSize)
			}
			types[name] = append(types[name], n.ID)
			return nil
		}); err != nil {
			return false, err
		}
		if len(types[name]) == 0 {
			return false, nil
		}
	}
	up := map[string][]string{}
	incomplete := map[string]bool{}
	roots := make(map[string]bool, len(types[receiver]))
	for _, id := range types[receiver] {
		roots[id] = true
	}
	queued := map[string]bool{}
	queue := make([]string, 0, len(types[receiver])+len(types[owner]))
	for _, ids := range types {
		for _, id := range ids {
			if !queued[id] {
				queued[id] = true
				queue = append(queue, id)
			}
		}
	}
	arcs := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, kind := range [...]graph.EdgeKind{graph.EdgeExtends, graph.EdgeImplements} {
			if err := view.VisitGuardOutgoing(ctx, id, kind, func(hop *graph.Edge) error {
				if graph.IsUnresolvedTarget(hop.To) {
					if roots[id] {
						incomplete[receiver] = true
					}
					return nil
				}
				arcs++
				if arcs > frameworkScopeRetainedRowCap {
					return fmt.Errorf("%w: C# external-call receiver hierarchy", store_pebble.ErrRebuildLinkInputSize)
				}
				up[id] = append(up[id], hop.To)
				if !queued[hop.To] {
					queued[hop.To] = true
					queue = append(queue, hop.To)
				}
				return nil
			}); err != nil {
				return false, err
			}
		}
	}
	return csharpShouldDemote(map[string]*graph.Node{caller.ID: caller, target.ID: target}, e, types, up, incomplete), nil
}

// This native module-path branch accepts only an unshadowed free function.
// Count up to two at each locality tier; no candidate slice is needed.
func visitRebuildRustFreeExternalCall(ctx context.Context, view store_pebble.RebuildView, e *graph.Edge, caller *graph.Node, name string,
	yield func(*graph.Edge, store_pebble.RebuildExternalCallCandidate) error,
) error {
	var shadowed bool
	if err := view.VisitExternalCallNodes(ctx, func(n *graph.Node) bool {
		if n.Kind != graph.KindParam || n.Name != name {
			return false
		}
		owner, rust := rustParameterOwner(n)
		return rust && owner == e.From
	}, func(*graph.Node) error {
		shadowed = true
		return nil
	}); err != nil || shadowed {
		return err
	}
	callerDir := rustParentDir(caller.FilePath)
	var election rustFreeFuncElection
	if err := view.VisitExternalCallNodes(ctx, func(n *graph.Node) bool {
		return n.Language == "rust" && n.Kind == graph.KindFunction && n.Name == name && n.RepoPrefix == caller.RepoPrefix
	}, func(n *graph.Node) error {
		election.add(n, caller.FilePath, callerDir)
		return nil
	}); err != nil {
		return err
	}
	if c, ok := rebuildExternalCallCandidate(election.target(), caller.Language); ok {
		return yield(e, c)
	}
	return nil
}

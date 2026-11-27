package resolver

import (
	"context"
	"fmt"
	"iter"
	"path/filepath"
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
		Extra: func(ctx context.Context, view store_pebble.RebuildView, yield func(*graph.Edge, store_pebble.RebuildExternalCallCandidate) error) error {
			if err := visitRebuildAliasedExternalCalls(ctx, view, receipts, rewriteNpm, yield); err != nil {
				return err
			}
			if err := visitRebuildDeferredIndexedExternalCalls(ctx, view, receipts, yield); err != nil {
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
// Match declaration facts, rather than requiring that ID to resemble the text.
func visitRebuildDeferredIndexedExternalCalls(ctx context.Context, view store_pebble.RebuildView,
	receipts iter.Seq2[store_pebble.ParseFacts, error],
	yield func(*graph.Edge, store_pebble.RebuildExternalCallCandidate) error,
) error {
	return view.VisitExternalCallNodes(ctx, func(n *graph.Node) bool {
		_, _, ok := parseExternalCallTarget(n.ID)
		return ok
	}, func(n *graph.Node) error {
		for f, err := range receipts {
			if err != nil {
				return err
			}
			if !deferredExternalCallExtractor(f.EffectiveLanguage) {
				continue
			}
			for _, raw := range f.Edges {
				if err := ctx.Err(); err != nil {
					return err
				}
				if !rebuildExternalIndexedLookup(raw, n) {
					continue
				}
				caller, err := view.ExternalCallNode(ctx, raw.From)
				if err != nil {
					return err
				}
				if caller != nil && n.RepoPrefix != caller.RepoPrefix {
					continue
				}
				name := graph.UnresolvedName(raw.To)
				if !strings.HasPrefix(name, "import::") {
					// The main name readers discard stdlib/dep synthetic IDs;
					// imports use qualified-name and file identity readers instead.
					if !strings.HasPrefix(name, "extern::") && len(withoutAttributedTerminals([]*graph.Node{n})) == 0 {
						continue
					}
					if caller != nil {
						_, languages := resolverCompatibleLanguages(caller.Language)
						if len(languages) != 0 && !slices.Contains(languages, n.Language) {
							continue
						}
					}
				}
				e := *raw
				if e.Kind == graph.EdgeReads {
					e.Kind = graph.EdgeReferences
				}
				if c, ok := rebuildExternalCallCandidate(n.ID, externalCallCallerLanguage(caller, e.FilePath)); ok {
					if err := yield(&e, c); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
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
	if n.Language == "php" {
		if method := phpEdgeMetaString(e, "eloquent_method"); n.Kind == graph.KindMethod && method != "" && method == n.Name {
			return true
		}
		for _, key := range [...]string{"fqn", "target_fqn"} {
			fqn := phpEdgeMetaString(e, key)
			if i := strings.LastIndex(fqn, `\`); i >= 0 {
				fqn = fqn[i+1:]
			}
			if fqn != "" && fqn == n.Name && (n.Kind == graph.KindType || n.Kind == graph.KindInterface || n.Kind == graph.KindFunction) {
				return true
			}
		}
	}
	if spec, ok := strings.CutPrefix(name, "extern::"); ok {
		_, leaf, ok := splitExternSpec(spec)
		return ok && leaf == n.Name && (callable || n.Kind == graph.KindType || n.Kind == graph.KindInterface)
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
	findScript := func(from string) (string, error) {
		var script string
		ambiguous := false
		err := view.VisitGuardOutgoing(ctx, from, graph.EdgeReferences, func(ref *graph.Edge) error {
			if graph.IsUnresolvedTarget(ref.To) || ambiguous {
				return nil
			}
			n, err := view.ExternalCallNode(ctx, ref.To)
			if err == nil && n != nil && n.Kind == graph.KindFile && n.Language == "gdscript" {
				ambiguous = script != "" && script != n.ID
				script = n.ID
			}
			return err
		})
		if ambiguous {
			script = ""
		}
		return script, err
	}
	script, err := findScript(e.From)
	if err != nil {
		return "", err
	}
	if script == "" {
		if root, _, ok := strings.Cut(e.From, "::"); ok {
			script, err = findScript(root)
		}
	}
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
		if !rustScopeEdgeCandidate(e) {
			return nil
		}
		caller, err := view.ExternalCallNode(ctx, e.From)
		if err != nil || caller == nil || caller.Language != "rust" {
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

func rebuildPascalFileRole(n *graph.Node) int {
	if n.Kind != graph.KindFile {
		return 0
	}
	switch strings.ToLower(filepath.Ext(n.FilePath)) {
	case ".pas", ".pp", ".dpr", ".lpr":
		return 1
	case ".dfm", ".lfm", ".fmx":
		return 2
	}
	return 0
}

func visitRebuildPascalExternalCalls(ctx context.Context, view store_pebble.RebuildView,
	yield func(*graph.Edge, store_pebble.RebuildExternalCallCandidate) error,
) error {
	return view.VisitExternalCallNodes(ctx, func(n *graph.Node) bool {
		_, _, ok := parseExternalCallTarget(n.ID)
		return ok && rebuildPascalFileRole(n) == 2
	}, func(form *graph.Node) error {
		var unit, lastForm graph.Node
		key := pascalFormKey(form.FilePath)
		if err := view.VisitExternalCallNodes(ctx, func(n *graph.Node) bool {
			return rebuildPascalFileRole(n) != 0 && pascalFormKey(n.FilePath) == key
		}, func(n *graph.Node) error {
			// Native fills both maps in ascending file-ID order, last wins.
			if rebuildPascalFileRole(n) == 1 {
				if n.ID > unit.ID {
					unit = *n
				}
			} else if n.ID > lastForm.ID {
				lastForm = *n
			}
			return nil
		}); err != nil {
			return err
		}
		if lastForm.ID != form.ID || unit.ID == "" || unit.WorkspaceID != form.WorkspaceID {
			return nil
		}
		e := &graph.Edge{From: unit.ID, To: form.ID, Kind: graph.EdgeReferences, FilePath: unit.FilePath}
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
		return n.Language == "rust" && n.Kind == graph.KindParam && n.Name == name && enclosingFunctionForBinding(n.ID) == e.From
	}, func(*graph.Node) error {
		shadowed = true
		return nil
	}); err != nil || shadowed {
		return err
	}
	var all, file, dir struct {
		count int
		id    string
	}
	if err := view.VisitExternalCallNodes(ctx, func(n *graph.Node) bool {
		return n.Language == "rust" && n.Kind == graph.KindFunction && n.Name == name && n.RepoPrefix == caller.RepoPrefix
	}, func(n *graph.Node) error {
		all.count, all.id = min(2, all.count+1), n.ID
		if n.FilePath == caller.FilePath {
			file.count, file.id = min(2, file.count+1), n.ID
		}
		if rustParentDir(n.FilePath) == rustParentDir(caller.FilePath) {
			dir.count, dir.id = min(2, dir.count+1), n.ID
		}
		return nil
	}); err != nil {
		return err
	}
	var id string
	switch {
	case all.count == 1:
		id = all.id
	case file.count == 1:
		id = file.id
	case file.count == 0 && dir.count == 1:
		id = dir.id
	}
	if c, ok := rebuildExternalCallCandidate(id, caller.Language); ok {
		return yield(e, c)
	}
	return nil
}

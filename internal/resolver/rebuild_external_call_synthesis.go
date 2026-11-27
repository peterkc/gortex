package resolver

import (
	"context"
	"iter"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildExternalCallSynthesis elects over live post-framework candidates.
// Receipts and skipped-pass candidates only prove whether that election is safe.
func RunRebuildExternalCallSynthesis(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error], enabled bool, rewriteNpm func(string, string) (string, error)) (uint64, error) {
	if !enabled {
		return 0, nil
	}
	return b.RebuildExternalCallSynthesis(ctx, receipts, store_pebble.RebuildExternalCallRules{
		Candidate: func(e *graph.Edge, caller *graph.Node) (store_pebble.RebuildExternalCallCandidate, bool) {
			return rebuildExternalCallCandidate(e.To, externalCallCallerLanguage(caller, e.FilePath))
		},
		Maybe: maybeRebuildExternalCall,
		Extra: func(ctx context.Context, view store_pebble.RebuildView, yield func(*graph.Edge, store_pebble.RebuildExternalCallCandidate) error) error {
			if err := visitRebuildAliasedExternalCalls(ctx, view, receipts, rewriteNpm, yield); err != nil {
				return err
			}
			return visitRebuildSkippedExternalCalls(ctx, view, yield)
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

func maybeRebuildExternalCall(e *graph.Edge, caller *graph.Node, extractor string, _ bool) []store_pebble.RebuildExternalCallCandidate {
	if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeReferences {
		return nil
	}
	lang := externalCallCallerLanguage(caller, e.FilePath)
	deferred := deferredExternalCallExtractor(extractor)
	// #39 reads the tag even after main binding. #97 can change the
	// payload of an actual terminal that doubles as a C# method ID.
	connection, _ := e.Meta["godot_connection"].(bool)
	method, _ := e.Meta["godot_method"].(string)
	receiver, _ := e.Meta["receiver_type"].(string)
	skipped := e.Kind == graph.EdgeReferences && connection && method != "" ||
		e.Kind == graph.EdgeCalls && lang == "csharp" && receiver != "" && !e.IsSpeculative() &&
			graph.OriginRank(e.EffectiveOrigin()) <= graph.OriginRank(graph.OriginASTInferred)
	if !deferred && !skipped {
		return nil
	}
	if candidate, ok := rebuildExternalCallCandidate(e.To, lang); ok {
		return []store_pebble.RebuildExternalCallCandidate{candidate}
	}
	if !deferred || !graph.IsUnresolvedTarget(e.To) {
		return nil
	}
	name := graph.UnresolvedName(e.To)
	var to string
	switch {
	case strings.HasPrefix(name, "extern::"):
		spec := strings.TrimPrefix(name, "extern::")
		path, symbol, sep := cutRebuildExternSpec(spec)
		switch {
		case !sep:
			to = "external::" + spec
		case isStdlibLike(path):
			to = graph.StubID(graph.RepoPrefixOfID(e.From), graph.StubKindStdlib, path, symbol)
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
	return language != "" && language != "go" && language != "javascript" && language != "typescript"
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
			if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeReferences || !isJSTSPath(e.FilePath) ||
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
			caller, err := view.Node(ctx, e.From)
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

func cutRebuildExternSpec(spec string) (path, symbol string, ok bool) {
	if i := strings.LastIndex(spec, "::"); i >= 0 {
		return spec[:i], spec[i+2:], true
	}
	return "", "", false
}

// The skipped scene and Rust turns can resolve to indexed member IDs. The
// Pascal turn adds form-file IDs. A configured extractor can spell one of
// those IDs as a package terminal, so include it as a possible entrant.
func visitRebuildSkippedExternalCalls(ctx context.Context, view store_pebble.RebuildView,
	yield func(*graph.Edge, store_pebble.RebuildExternalCallCandidate) error,
) error {
	if err := view.VisitFrameworkGateNodes(ctx, func(form *graph.Node) error {
		if form.Kind != graph.KindFile {
			return nil
		}
		if _, _, ok := parseExternalCallTarget(form.ID); !ok {
			return nil
		}
		switch strings.ToLower(filepath.Ext(form.FilePath)) {
		case ".dfm", ".lfm", ".fmx":
		default:
			return nil
		}
		return view.VisitFrameworkGateNodes(ctx, func(unit *graph.Node) error {
			if unit.Kind != graph.KindFile || unit.WorkspaceID != form.WorkspaceID {
				return nil
			}
			switch strings.ToLower(filepath.Ext(unit.FilePath)) {
			case ".pas", ".pp", ".dpr", ".lpr":
			default:
				return nil
			}
			if pascalFormKey(unit.FilePath) != pascalFormKey(form.FilePath) {
				return nil
			}
			e := &graph.Edge{From: unit.ID, To: form.ID, Kind: graph.EdgeReferences, FilePath: unit.FilePath}
			if c, ok := rebuildExternalCallCandidate(e.To, externalCallCallerLanguage(unit, e.FilePath)); ok {
				return yield(e, c)
			}
			return nil
		})
	}); err != nil {
		return err
	}
	return view.VisitEdges(ctx, func(e *graph.Edge) error {
		if e.Kind == graph.EdgeReferences {
			if connection, _ := e.Meta["godot_connection"].(bool); connection {
				if method, _ := e.Meta["godot_method"].(string); method != "" {
					return visitRebuildGodotExternalCall(ctx, view, e, method, yield)
				}
			}
		}
		if e.Kind != graph.EdgeCalls || !graph.IsUnresolvedTarget(e.To) || !rustScopeEdgeCandidate(e) {
			return nil
		}
		caller, err := view.Node(ctx, e.From)
		if err != nil || caller == nil || caller.Language != "rust" {
			return err
		}
		path, _ := e.Meta["rust_path"].(string)
		_, leaf, _ := cutRebuildExternSpec(path)
		selector := selectorCallName(e.To)
		for i, name := range [...]string{selector, leaf} {
			if name == "" || i == 1 && name == selector {
				continue
			}
			if err := view.VisitNameCandidates(ctx, name, []string{"rust"}, func(n *graph.Node) error {
				if n.RepoPrefix != caller.RepoPrefix || n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
					return nil
				}
				if c, ok := rebuildExternalCallCandidate(n.ID, caller.Language); ok {
					return yield(e, c)
				}
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

func visitRebuildGodotExternalCall(ctx context.Context, view store_pebble.RebuildView, e *graph.Edge, method string,
	yield func(*graph.Edge, store_pebble.RebuildExternalCallCandidate) error,
) error {
	return view.VisitNameCandidates(ctx, method, []string{"gdscript"}, func(n *graph.Node) error {
		if !slices.Contains(gdScriptMemberKinds[:], n.Kind) {
			return nil
		}
		if _, _, ok := parseExternalCallTarget(n.ID); !ok {
			return nil
		}
		caller, err := view.Node(ctx, e.From)
		if err != nil {
			return err
		}
		if c, ok := rebuildExternalCallCandidate(n.ID, externalCallCallerLanguage(caller, e.FilePath)); ok {
			return yield(e, c)
		}
		return nil
	})
}

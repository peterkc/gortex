package resolver

import (
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// C# file evidence mirrors csharpFileNamespaceSet without retaining a
// workspace map of project directories, global usings, or caller files.
// It builds only one caller's visibility at a time. The final working rows
// are authoritative; the cold native resolver clears its main-page cache.
func (s *rebuildGuardStage) csharpVisibility(file string) (csharpFileNS, error) {
	visible := csharpFileNS{
		enclosing: map[string]struct{}{}, imported: map[string]struct{}{}, statics: map[string]struct{}{},
	}
	var bytes int
	add := func(set map[string]struct{}, name string) error {
		if _, ok := set[name]; ok {
			return nil
		}
		bytes += 96 + len(name) // charge entries as well as string bytes
		if bytes > rebuildGuardPageBytes {
			return fmt.Errorf("%w: guard C# visibility for %q", store_pebble.ErrRebuildLinkInputSize, file)
		}
		set[name] = struct{}{}
		return nil
	}
	f, err := s.view.Node(s.ctx, file)
	if err != nil {
		return visible, err
	}
	if f != nil {
		for _, u := range csharpMetaStrings(f.Meta["usings"]) {
			if err := add(visible.imported, u); err != nil {
				return visible, err
			}
		}
		for _, u := range csharpMetaStrings(f.Meta["using_static"]) {
			if err := add(visible.statics, u); err != nil {
				return visible, err
			}
		}
		for _, u := range csharpMetaStrings(f.Meta["scoped_usings"]) {
			scope, name, ok := strings.Cut(u, "|")
			if !ok || name == "" {
				continue
			}
			if visible.scoped == nil {
				visible.scoped = map[string]map[string]struct{}{}
			}
			if visible.scoped[scope] == nil {
				bytes += len(scope) + 128
				visible.scoped[scope] = map[string]struct{}{}
			}
			if err := add(visible.scoped[scope], name); err != nil {
				return visible, err
			}
		}
	}
	hasStamp := len(visible.imported) > 0
	if err := s.loadCSharpGlobals(); err != nil {
		return visible, err
	}
	global := func(dir string) error {
		if err := s.usings.VisitGlobals(s.ctx, dir, false, func(name string) error {
			if err := add(visible.imported, name); err != nil {
				return err
			}
			if visible.scoped == nil {
				return nil
			}
			if visible.scoped[""] == nil {
				visible.scoped[""] = map[string]struct{}{}
			}
			return add(visible.scoped[""], name)
		}); err != nil {
			return err
		}
		return s.usings.VisitGlobals(s.ctx, dir, true, func(name string) error { return add(visible.statics, name) })
	}
	dir := csharpDirOf(file)
	project, err := s.csharpProjectDir(dir)
	if err != nil {
		return visible, err
	}
	if project != "" {
		if err := global(project); err != nil {
			return visible, err
		}
	} else {
		for {
			if err := global(dir); err != nil {
				return visible, err
			}
			if dir == "." {
				break
			}
			dir = csharpDirOf(dir)
		}
	}
	if !hasStamp {
		err := s.view.VisitGuardOutgoing(s.ctx, file, graph.EdgeImports, func(e *graph.Edge) error {
			for _, prefix := range [...]string{"unresolved::import::", "external::"} {
				if name, ok := strings.CutPrefix(e.To, prefix); ok && name != "" {
					return add(visible.imported, strings.ReplaceAll(name, "/", "."))
				}
			}
			return nil
		})
		if err != nil {
			return visible, err
		}
	}
	err = s.view.VisitGuardOutgoing(s.ctx, file, graph.EdgeDefines, func(e *graph.Edge) error {
		n, err := s.view.Node(s.ctx, e.To)
		if err != nil || n == nil {
			return err
		}
		for scope := rebuildGuardScopeNS(n); scope != ""; scope = rebuildGuardParentNS(scope) {
			if err := add(visible.enclosing, scope); err != nil {
				return err
			}
		}
		return nil
	})
	return visible, err
}

func (s *rebuildGuardStage) loadCSharpGlobals() error {
	if s.usings != nil {
		return nil
	}
	usings, err := s.build.OpenRebuildGuardUsings(s.ctx)
	if err != nil {
		return err
	}
	s.usings = usings
	isProject := func(id string) bool { return strings.HasSuffix(strings.ToLower(id), ".csproj") }
	if err := s.view.VisitNodes(s.ctx, func(n *graph.Node) error {
		if n.Kind == graph.KindFile && isProject(n.ID) {
			return usings.RecordProject(csharpDirOf(n.ID))
		}
		return nil
	}); err != nil {
		return err
	}
	return s.view.VisitNodes(s.ctx, func(n *graph.Node) error {
		if n.Kind != graph.KindFile || isProject(n.ID) {
			return nil
		}
		globals := csharpMetaStrings(n.Meta["global_usings"])
		statics := csharpMetaStrings(n.Meta["global_using_static"])
		if len(globals)+len(statics) == 0 {
			return nil
		}
		dir := csharpDirOf(n.ID)
		project, err := s.csharpProjectDir(dir)
		if err != nil {
			return err
		}
		if project != "" {
			dir = project
		}
		for _, name := range globals {
			if err := usings.RecordGlobal(dir, name, false); err != nil {
				return err
			}
		}
		for _, name := range statics {
			if err := usings.RecordGlobal(dir, name, true); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *rebuildGuardStage) csharpProjectDir(dir string) (string, error) {
	for {
		if err := s.ctx.Err(); err != nil {
			return "", err
		}
		ok, err := s.usings.HasProject(dir)
		if err != nil || ok {
			return dir, err
		}
		if dir == "." {
			return "", nil
		}
		dir = csharpDirOf(dir)
	}
}

func rebuildGuardExtensionVisible(caller, target *graph.Node, visible csharpFileNS, ns string) bool {
	if csharpUsingStaticAdmits(visible, target, ns) || ns == "" {
		return true
	}
	callerNS := rebuildGuardScopeNS(caller)
	if callerNS == "" {
		if _, enclosing := visible.enclosing[ns]; enclosing {
			return true
		}
	}
	for scope := callerNS; scope != ""; scope = rebuildGuardParentNS(scope) {
		if scope == ns || (visible.scoped != nil && rebuildGuardHasNamespace(visible.scoped[scope], ns)) {
			return true
		}
	}
	if visible.scoped != nil && caller != nil {
		return rebuildGuardHasNamespace(visible.scoped[""], ns)
	}
	return rebuildGuardHasNamespace(visible.imported, ns)
}

func rebuildGuardHasNamespace(set map[string]struct{}, ns string) bool {
	_, ok := set[ns]
	return ok
}

func rebuildGuardScopeNS(n *graph.Node) string {
	if n == nil {
		return ""
	}
	scope, _ := n.Meta["scope_ns"].(string)
	return scope
}

func rebuildGuardParentNS(ns string) string {
	if i := strings.LastIndex(ns, "."); i >= 0 {
		return ns[:i]
	}
	return ""
}

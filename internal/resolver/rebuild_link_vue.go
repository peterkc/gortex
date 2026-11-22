package resolver

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// resolveRebuildVue handles only the Vue template and rendered-child main
// branches. A deliberate unresolved template import is handled, not a miss.
func resolveRebuildVue(ctx context.Context, f *rebuildLinkFrontier, edge *graph.Edge, target string) error {
	var id string
	var handled bool
	var err error
	if edge.Kind == graph.EdgeRendersChild {
		id, err = rebuildRenderedImport(ctx, f.view, edge.FilePath, target)
	} else {
		id, handled, err = rebuildVueTemplateImport(ctx, f, edge, target)
	}
	if err != nil {
		return err
	}
	if id == "" {
		if handled {
			return nil
		}
		return resolveRebuildType(ctx, f, edge, target, true)
	}
	edge.To, edge.Origin, edge.Confidence = id, graph.OriginASTResolved, 0.95
	edge.Meta = maps.Clone(edge.Meta)
	if edge.Meta == nil {
		edge.Meta = make(map[string]any, 1)
	}
	edge.Meta["resolution"] = "import_binding"
	return nil
}

func rebuildRenderedImport(ctx context.Context, view store_pebble.RebuildView, file, name string) (string, error) {
	var path, export string
	var line int
	err := view.VisitGuardOutgoing(ctx, file, graph.EdgeImports, func(imp *graph.Edge) error {
		p, local, symbol := importBindingOf(imp)
		// SQLite uses (line, rowid). The stream is in complete-key order:
		// keep its first eligible key at the lowest line (D27 rule b).
		if local == name && p != "" && (path == "" || imp.Line < line) {
			path, export, line = p, symbol, imp.Line
		}
		return nil
	})
	if err != nil || !strings.HasPrefix(path, ".") {
		return "", err
	}
	dir := ""
	if i := strings.LastIndexByte(file, '/'); i >= 0 {
		dir = file[:i]
	}
	base := joinRelativePath(dir, path)
	if base == "" {
		return "", nil
	}
	// Unlike the Vue template path, the native JSX probe appends these
	// extensions even to an explicitly extended specifier, and tests IDs
	// directly without requiring a file node.
	for _, entry := range [...]string{"", "/index"} {
		for _, ext := range [...]string{".tsx", ".ts", ".jsx", ".js"} {
			id := base + entry + ext + "::" + export
			n, err := view.Node(ctx, id)
			if err != nil {
				return "", err
			}
			if n != nil {
				return id, nil
			}
		}
	}
	return "", nil
}

func rebuildVueTemplateImport(ctx context.Context, f *rebuildLinkFrontier, edge *graph.Edge, name string) (id string, handled bool, err error) {
	raw, _ := edge.Meta["template_name"].(string)
	var path string
	var locals [2]string // a matching local is either the target or the raw tag
	invalid := false
	err = f.view.VisitGuardOutgoing(ctx, edge.FilePath, graph.EdgeImports, func(light *graph.Edge) error {
		imp, err := f.view.Edge(ctx, graph.EdgeIdentityFor(light))
		if err != nil {
			return err
		}
		if imp == nil {
			return fmt.Errorf("%w: Vue import changed during lookup in %q", errRebuildMainRule, edge.FilePath)
		}
		local, _ := imp.Meta["vue_default_import"].(string)
		if local == "" || (local != name && local != raw) {
			return nil
		}
		p, _, _ := importBindingOf(imp)
		p = strings.TrimPrefix(p, "external::")
		// Different nonempty paths conflict in either order. An empty
		// specifier cannot resolve and is itself a hard native refusal.
		if p == "" || (path != "" && path != p) {
			invalid = true
		}
		path = p
		if local == name {
			locals[0] = local
		} else {
			locals[1] = local
		}
		return nil
	})
	if err != nil || invalid || path == "" {
		return "", invalid, err
	}

	n, err := f.view.Node(ctx, path)
	if err != nil {
		return "", false, err
	}
	file := path
	if n == nil || n.Kind != graph.KindFile {
		spec, _ := splitImportSpecSymbol(path)
		if spec != "" && !strings.HasPrefix(spec, "./") && !strings.HasPrefix(spec, "../") {
			// The native expander can bind even bare packages via paths or
			// baseUrl. A missing file is not a negative alias receipt.
			return "", false, fmt.Errorf("%w: Vue template import %q in %q needs frozen path-alias evidence", errRebuildMainRule, path, edge.FilePath)
		}
		file = resolveJSTSImportTarget(func(id string) *graph.Node {
			if err != nil {
				return nil
			}
			var n *graph.Node
			n, err = f.view.Node(ctx, id)
			return n
		}, nil, edge.FilePath, path)
		if err != nil {
			return "", false, err
		}
	}
	if file == "" {
		return rebuildVueTemplateTail(ctx, f, edge, path)
	}
	for _, local := range locals {
		if local == "" {
			continue
		}
		pick := ""
		for _, symbol := range [...]string{local, "default", importPathBasename(file)} {
			n, err := f.view.Node(ctx, file+"::"+symbol)
			if err != nil {
				return "", false, err
			}
			if n != nil {
				pick = n.ID
				break
			}
		}
		// For a known file every match must select the same symbol. A
		// missing symbol is already a native refusal, in either order.
		if pick == "" || (id != "" && id != pick) {
			return "", true, nil
		}
		id = pick
	}
	return id, true, nil
}

func rebuildVueTemplateTail(ctx context.Context, f *rebuildLinkFrontier, edge *graph.Edge, path string) (id string, handled bool, err error) {
	if !strings.HasPrefix(path, ".") {
		return "", true, nil
	}
	base := importPathBasename(path)
	if base == "" {
		return "", strings.HasSuffix(path, ".vue"), nil
	}
	source, err := f.view.Node(ctx, edge.From)
	if err != nil {
		return "", false, err
	}
	req := store_pebble.LinkRequest{Kind: store_pebble.LinkName, Key: base}
	if source != nil {
		req.RepoPrefix = source.RepoPrefix
		_, req.Languages = resolverCompatibleLanguages(source.Language)
	}
	tail := strings.TrimSuffix(strings.TrimPrefix(path, "./"), ".vue") + ".vue"
	ambiguous := false
	_, err = f.candidates(ctx, []store_pebble.LinkRequest{req}, func(_ int, n *graph.Node) error {
		if n.Language != "vue" || n.Meta["component"] != true ||
			(n.FilePath != tail && !strings.HasSuffix(n.FilePath, "/"+tail)) {
			return nil
		}
		one := [1]*graph.Node{n}
		if len(genericInstantiationOnly(edge, withoutAttributedTerminals(one[:]))) == 0 {
			return nil
		}
		if id != "" && id != n.ID {
			ambiguous = true
		}
		id = n.ID
		return nil
	})
	if err != nil || ambiguous {
		return "", ambiguous, err
	}
	return id, id != "" || strings.HasSuffix(path, ".vue"), nil
}

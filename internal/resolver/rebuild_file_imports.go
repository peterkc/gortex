package resolver

import (
	"context"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildFileImports binds relative imports, Lua/Luau requires and Godot
// resource paths at the native settle point in a single frozen checkout.
// Run this after main resolution, on the import rows it left. A nil cpp
// refuses only lookups that reach the ordered C include directory probe.
// The next pass must read the updated rows, not cached import adjacency.
func RunRebuildFileImports(ctx context.Context, b *store_pebble.BuildStore, spillAt int, cpp RebuildCppIncludePaths) (store_pebble.RebuildFileImportStats, error) {
	return b.RebuildFileImports(ctx, spillAt,
		func(files *store_pebble.RebuildFileImportIndex, e *graph.Edge) (*graph.Edge, error) {
			return rebuildRelativeImport(ctx, files, e, cpp)
		},
		func(files *store_pebble.RebuildFileImportIndex, e *graph.Edge) (string, error) {
			return rebuildLuaRequire(ctx, files, e)
		},
		func(files *store_pebble.RebuildFileImportIndex, e *graph.Edge) (string, error) {
			if !isGodotResPlaceholder(e.To) {
				return "", nil
			}
			p := strings.Trim(strings.TrimPrefix(e.To, godotResPlaceholderPrefix), "/")
			if p == "" {
				return "", nil
			}
			return rebuildUniqueFileSuffix(ctx, files, p)
		})
}

func rebuildLuaRequire(ctx context.Context, files *store_pebble.RebuildFileImportIndex, e *graph.Edge) (string, error) {
	name, ok := strings.CutPrefix(e.To, "unresolved::import::")
	if !ok || name == "" {
		return "", nil
	}
	caller, err := files.File(ctx, e.From)
	if err != nil || (caller.Language != "lua" && caller.Language != "luau") {
		return "", err
	}
	if _, isRoblox := e.Meta["roblox_path"]; isRoblox {
		var match rebuildFileMatch
		for _, ext := range [...]string{".lua", ".luau"} {
			if err := files.VisitBasename(ctx, name+ext, match.add); err != nil {
				return "", err
			}
		}
		return match.unique(), nil
	}
	if isLuauPathRequire(name) {
		p, ok := luauRequireModulePath(e.From, name)
		if !ok {
			return "", nil
		}
		var match rebuildFileMatch
		for _, c := range luaFileCandidates(p) {
			file, err := files.File(ctx, c)
			if err != nil {
				return "", err
			}
			if file.ID != "" {
				match.add(file.ID)
			}
		}
		return match.unique(), nil
	}
	candidates := luaModuleCandidates(name)
	// Exact candidates all outrank any suffix, in native extension order.
	for _, c := range candidates {
		file, err := files.File(ctx, c)
		if err != nil || file.ID != "" {
			return file.ID, err
		}
	}
	for _, c := range candidates {
		id, err := rebuildUniqueFileSuffix(ctx, files, c)
		if err != nil || id != "" {
			return id, err
		}
	}
	return "", nil
}

func rebuildUniqueFileSuffix(ctx context.Context, files *store_pebble.RebuildFileImportIndex, p string) (string, error) {
	base, suffix := p[strings.LastIndexByte(p, '/')+1:], "/"+p
	var match rebuildFileMatch
	err := files.VisitBasename(ctx, base, func(id string) {
		if id == p || strings.HasSuffix(id, suffix) {
			match.add(id)
		}
	})
	if err != nil {
		return "", err
	}
	return match.unique(), nil
}

type rebuildFileMatch struct {
	id        string
	ambiguous bool
}

func (m *rebuildFileMatch) add(id string) {
	m.ambiguous = m.ambiguous || (m.id != "" && m.id != id)
	m.id = id
}

func (m rebuildFileMatch) unique() string {
	if m.ambiguous {
		return ""
	}
	return m.id
}

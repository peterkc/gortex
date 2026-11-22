package resolver

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// ErrRebuildCppIncludeInput marks a lookup without verified C include paths.
var ErrRebuildCppIncludeInput = errors.New("rebuild requires checked frozen C include directories")

// RebuildCppIncludePaths visits verified search directories synchronously, in
// native order. For an empty or missing TU list it supplies the sorted union,
// or the heuristic order when no TU exists. It stops on a hit or error and
// reports whether the repo has any TU entry, even one with no directories.
type RebuildCppIncludePaths func(ctx context.Context, importingFile string, visit func(dir string) (stop bool, err error)) (usingCompileDB bool, err error)

func rebuildRelativeImport(ctx context.Context, files *store_pebble.RebuildFileImportIndex, e *graph.Edge, cpp RebuildCppIncludePaths) (*graph.Edge, error) {
	stem, pyrel := strings.CutPrefix(e.To, "unresolved::pyrel::")
	uri, external := strings.CutPrefix(e.To, "external::")
	path, stub := strings.CutPrefix(e.To, "unresolved::import::")
	if !pyrel && !external && !stub {
		return nil, nil
	}
	caller, err := files.File(ctx, e.From)
	if err != nil || caller.ID == "" {
		return nil, err
	}
	if pyrel {
		if caller.Language == "python" {
			target, err := rebuildPythonRelative(ctx, files, stem)
			if err != nil || target != "" {
				return rebuildResolvedRelative(e, target), err
			}
		}
		// The pinned pass downgrades even a pyrel marker on a non-Python file
		// once the language gate is open. Neither downgrade promotes origin.
		moved := *e
		moved.To = "external::" + stem
		return &moved, nil
	}
	if external {
		var target string
		var err error
		switch caller.Language {
		case "python":
			target, err = rebuildPythonRelative(ctx, files, uri)
		case "dart":
			if uri != "" && !strings.HasPrefix(uri, "dart:") && !strings.HasPrefix(uri, "package:") {
				target, err = rebuildFirstRelativeFile(ctx, files, joinRelativePath(rebuildRelativeDir(e.From), uri))
			}
		}
		return rebuildResolvedRelative(e, target), err
	}
	var target string
	switch caller.Language {
	case "php":
		if !isPhpPathInclude(path) {
			return nil, nil
		}
		if !strings.HasSuffix(strings.ToLower(path), ".php") {
			path += ".php"
		}
		target, err = rebuildFirstRelativeFile(ctx, files,
			joinRelativePath(rebuildRelativeDir(e.From), path), joinRelativePath("", path))
		if err == nil && target == "" {
			target, err = rebuildUniqueFileSuffix(ctx, files, path)
		}
	case "c", "cpp", "objc":
		return rebuildCppInclude(ctx, files, e, path, cpp)
	}
	return rebuildResolvedRelative(e, target), err
}

func rebuildCppInclude(ctx context.Context, files *store_pebble.RebuildFileImportIndex, e *graph.Edge, rel string, paths RebuildCppIncludePaths) (*graph.Edge, error) {
	kind, _ := e.Meta["include_kind"].(string)
	if rel == "" || (kind != "quoted" && kind != "system") || (kind == "system" && IsCppStdlibHeader(rel)) {
		return nil, nil
	}
	dir := rebuildRelativeDir(e.From)
	target, err := rebuildFirstRelativeFile(ctx, files, joinRelativePath(dir, rel), joinRelativePath(dir, "./"+rel))
	if err != nil || target != "" {
		return rebuildResolvedRelative(e, target), err
	}
	if paths == nil {
		return nil, fmt.Errorf("%w: %s -> %s", ErrRebuildCppIncludeInput, e.From, e.To)
	}
	hasExt := strings.Contains(rel[strings.LastIndexByte(rel, '/')+1:], ".")
	var hitDir string
	usingDB, err := paths(ctx, e.From, func(dir string) (bool, error) {
		cand := joinRelativePath(dir, rel)
		if cand == "" {
			return false, nil
		}
		for _, ext := range [...]string{"", ".h", ".hpp", ".hh", ".hxx"} {
			if ext != "" && hasExt {
				break
			}
			file, err := files.File(ctx, cand+ext)
			if err != nil {
				return false, err
			}
			if file.ID != "" {
				target, hitDir = file.ID, dir
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	if target == "" && strings.Contains(rel, "/") {
		target, err = rebuildUniqueFileSuffix(ctx, files, rel)
		if err != nil {
			return nil, err
		}
	}
	moved := rebuildResolvedRelative(e, target)
	if hitDir != "" {
		moved.Meta = maps.Clone(e.Meta)
		moved.Meta["include_dir"] = hitDir
		via := "heuristic"
		if usingDB {
			via = "compile_db"
		}
		moved.Meta["resolved_via"] = via
	}
	return moved, nil
}

func rebuildPythonRelative(ctx context.Context, files *store_pebble.RebuildFileImportIndex, stem string) (string, error) {
	if !strings.Contains(stem, "/") {
		return "", nil
	}
	return rebuildFirstRelativeFile(ctx, files, stem+".py", stem+"/__init__.py")
}

func rebuildFirstRelativeFile(ctx context.Context, files *store_pebble.RebuildFileImportIndex, candidates ...string) (string, error) {
	for _, id := range candidates {
		if id == "" {
			continue
		}
		file, err := files.File(ctx, id)
		if err != nil || file.ID != "" {
			return file.ID, err
		}
	}
	return "", nil
}

func rebuildRelativeDir(file string) string {
	if slash := strings.LastIndexByte(file, '/'); slash >= 0 {
		return file[:slash]
	}
	return ""
}

func rebuildResolvedRelative(e *graph.Edge, target string) *graph.Edge {
	if target == "" {
		return nil
	}
	moved := *e
	moved.To, moved.Origin = target, graph.OriginASTResolved
	return &moved
}

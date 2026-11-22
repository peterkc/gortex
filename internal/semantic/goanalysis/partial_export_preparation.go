package goanalysis

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

func fileCompilerInputs(repoPrefix, repoRoot string, filePaths []string) (absRoot, sourceRoot string, requestedFiles, patterns []string, fileLoadDir string, err error) {
	absRoot, err = filepath.Abs(repoRoot)
	if err != nil {
		return "", "", nil, nil, "", fmt.Errorf("absolute path: %w", err)
	}

	prefix := normalizeRelPath(repoPrefix)
	seen := make(map[string]struct{}, len(filePaths))
	requestedFiles = make([]string, 0, len(filePaths))
	for _, filePath := range filePaths {
		graphPath := normalizeRelPath(filePath)
		if graphPath == "" {
			continue
		}
		if _, ok := seen[graphPath]; ok {
			continue
		}
		seen[graphPath] = struct{}{}
		requestedFiles = append(requestedFiles, graphPath)
	}
	sort.Strings(requestedFiles)
	if len(requestedFiles) == 0 {
		return absRoot, absRoot, requestedFiles, nil, "", nil
	}

	// Use canonical source paths for directory queries while keeping absRoot as
	// the binding-index owner (callers may release that owner through a symlink).
	sourceRoot = absRoot
	if resolved, err := filepath.EvalSymlinks(absRoot); err == nil {
		sourceRoot = resolved
	}
	patterns = make([]string, 0, len(requestedFiles))
	for _, graphPath := range requestedFiles {
		relPath := graphPath
		if prefix != "" {
			relPath = strings.TrimPrefix(relPath, prefix+"/")
		}
		patterns = append(patterns, "file="+filepath.Join(sourceRoot, filepath.FromSlash(relPath)))
	}
	// The file= patterns are absolute, but Dir still selects the module whose
	// build list resolves them: a repository whose module lives in a
	// subdirectory resolves nothing from the root. absRoot remains the
	// relativization base for graph paths.
	fileLoadDir, _ = goLoadDir(absRoot)
	return absRoot, sourceRoot, requestedFiles, patterns, fileLoadDir, nil
}

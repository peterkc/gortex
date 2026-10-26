package indexer

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/resolver"
)

type npmPackageFileStamp struct{ size, modified int64 }

type npmPackageSnapshot struct {
	revision          uint64
	roots, workspaces map[string]string
	files             map[string]npmPackageFileStamp
	lookup            resolver.NpmPackageLookup
}

func npmPackageStamp(file string) npmPackageFileStamp {
	if info, err := os.Stat(file); err == nil {
		return npmPackageFileStamp{info.Size(), info.ModTime().UnixNano()}
	}
	return npmPackageFileStamp{}
}

func (s *npmPackageSnapshot) unchanged() bool {
	for file, stamp := range s.files {
		if npmPackageStamp(file) != stamp {
			return false
		}
	}
	return true
}

// Watch every directory prefix of each supported workspace glob. New members,
// including package.json added to an existing empty directory, invalidate it.
// Glob expansion runs only on a rebuild, never on an unchanged resolver pass.
func watchNpmWorkspacePaths(root string, patterns []string, watch func(string)) {
	for _, pattern := range patterns {
		if filepath.IsAbs(pattern) || hasDotDotSegment(pattern) {
			continue
		}
		parts := strings.Split(filepath.ToSlash(strings.TrimSuffix(pattern, "/")), "/")
		for depth := 1; depth <= len(parts); depth++ {
			prefix := filepath.Join(root, filepath.FromSlash(strings.Join(parts[:depth], "/")))
			watch(prefix)
			matches, _ := filepath.Glob(prefix)
			for _, dir := range matches {
				info, err := os.Stat(dir)
				if err != nil || !info.IsDir() {
					continue
				}
				watch(dir)
				if depth == len(parts) {
					watch(filepath.Join(dir, "package.json"))
				}
			}
		}
	}
}

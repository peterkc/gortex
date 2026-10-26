package indexer

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/resolver"
)

type npmPackageFileStamp struct {
	size, modified int64
	content        string
	kind           os.FileMode
	valid          bool
}

type npmPackageSnapshot struct {
	revision          uint64
	roots, workspaces map[string]string
	files             map[string]npmPackageFileStamp
	lookup            resolver.NpmPackageLookup
}

func npmPackageStamp(file string) npmPackageFileStamp {
	info, err := os.Stat(file)
	if os.IsNotExist(err) {
		return npmPackageFileStamp{valid: true}
	}
	if err != nil {
		return npmPackageFileStamp{}
	}
	var content []byte
	if info.IsDir() {
		entries, err := os.ReadDir(file)
		if err != nil {
			return npmPackageFileStamp{}
		}
		for _, entry := range entries {
			kind := entry.Type()
			if kind&os.ModeSymlink != 0 {
				if target, err := os.Stat(filepath.Join(file, entry.Name())); err == nil && target.IsDir() {
					kind |= os.ModeDir
				}
			}
			content = append(content, entry.Name()...)
			content = append(content, 0)
			content = append(content, kind.String()...)
			content = append(content, 0)
		}
	} else {
		content, err = os.ReadFile(file)
		if err != nil {
			return npmPackageFileStamp{}
		}
	}
	return npmPackageFileStamp{size: info.Size(), modified: info.ModTime().UnixNano(), content: contentHashForSource(content), kind: info.Mode().Type(), valid: true}
}

// Size and mtime can collide on coarse clocks; compare bytes/listings without
// reparsing manifests or expanding workspace globs on an unchanged pass.
func (s *npmPackageSnapshot) unchanged() bool {
	for file, stamp := range s.files {
		if !stamp.valid || npmPackageStamp(file) != stamp {
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

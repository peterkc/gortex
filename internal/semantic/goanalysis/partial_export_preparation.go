package goanalysis

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
	"golang.org/x/tools/go/packages"
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

// AcquireFileExportAdmission holds the existing compiler lease across both
// startup stages. Each stage still owns its own cancellation and deadline.
func (p *Provider) AcquireFileExportAdmission(ctx context.Context) (func(context.Context) context.Context, func(), error) {
	started := time.Now()
	release, err := p.acquireHeavy(ctx, false)
	if err != nil {
		return nil, nil, err
	}
	p.logger.Info("go-types: startup file stages admitted", zap.Duration("admission_wait", time.Since(started)))
	return func(stage context.Context) context.Context {
		return context.WithValue(stage, startupCompilerAdmission{}, p)
	}, release, nil
}

// PrepareFileExportsContext uses the binding loader's native cache inputs without
// retaining a typed program or publishing graph/freshness state.
func (p *Provider) PrepareFileExportsContext(ctx context.Context, repoPrefix, repoRoot string, filePaths []string) error {
	if goTypesNeedDepsClosure() {
		return ctx.Err()
	}
	_, _, requested, patterns, dir, err := fileCompilerInputs(repoPrefix, repoRoot, filePaths)
	if err != nil || len(requested) == 0 {
		return err
	}
	started := time.Now()
	release, err := p.acquireHeavy(ctx, false)
	if err != nil {
		return err
	}
	defer release()
	p.logger.Info("go-types: export preparation admitted",
		zap.String("stage", "export_preparation"),
		zap.Duration("admission_wait", time.Since(started)),
	)
	defer p.beginCompilerLoad(dir)()
	cfg := &packages.Config{
		Context: ctx, Dir: dir, Tests: p.includeTest,
		Mode:    packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedImports | packages.NeedExportFile | packages.NeedModule,
		Overlay: committedOverlay(ctx), Env: committedEnv(ctx),
	}
	boundCompilerSubprocess(cfg)
	pkgs, err := loadFilePackages(cfg, p.loadCompilerPackages, patterns)
	clearGoPackageCompilerRoots(pkgs)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("load exports for %d files: %w", len(requested), err)
	}
	return nil
}

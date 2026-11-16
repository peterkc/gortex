package serverstack

import (
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/resolver"
	"github.com/zzet/gortex/internal/semantic/lsp"
)

// IsFalsyEnv reports whether the named env var holds a falsy value
// (0/false/no/off/n, case-insensitive). Empty/unset is NOT falsy.
func IsFalsyEnv(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "0", "false", "no", "off", "n":
		return true
	default:
		return false
	}
}

// LspDisabledSet builds the set of LSP spec names that should NOT be
// auto-registered by Router.RegisterAvailable, merging per-spec config
// opt-outs (semantic.providers with enabled:false that resolve to a
// known LSP spec) with the comma-separated GORTEX_LSP_DISABLE env var.
// The special key "__all__" (from the literal "all"/"*") signals
// "skip auto-register everywhere" and is checked separately by callers.
func LspDisabledSet(providers []config.SemanticProviderConfig, envVar string) map[string]bool {
	out := map[string]bool{}
	for _, pc := range providers {
		if pc.Enabled {
			continue
		}
		if lsp.SpecByName(pc.Name) != nil {
			out[pc.Name] = true
		}
	}
	for _, raw := range strings.Split(envVar, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if strings.EqualFold(name, "all") || name == "*" {
			out["__all__"] = true
			continue
		}
		out[name] = true
	}
	return out
}

// RepoLikelyHasTypeScriptIntent reports whether a repo root carries one
// of the canonical TS/JS project markers, used to decide whether to wire
// the resolve-time tsserver helper for a tracked repo.
func RepoLikelyHasTypeScriptIntent(absRoot string) bool {
	for _, marker := range []string{"tsconfig.json", "jsconfig.json", "package.json"} {
		if _, err := os.Stat(filepath.Join(absRoot, marker)); err == nil {
			return true
		}
	}
	return false
}

// RepoLikelyHasPythonIntent reports whether a repo root carries one of the
// standard Python project markers, used to decide whether to wire the
// resolve-time pyright helper for a tracked repo.
func RepoLikelyHasPythonIntent(absRoot string) bool {
	for _, marker := range []string{
		"pyproject.toml",
		"setup.py",
		"setup.cfg",
		"requirements.txt",
		"requirements.in",
		"Pipfile",
		"poetry.lock",
		"uv.lock",
		"tox.ini",
		".python-version",
	} {
		if _, err := os.Stat(filepath.Join(absRoot, marker)); err == nil {
			return true
		}
	}
	if matches, err := filepath.Glob(filepath.Join(absRoot, "*.py")); err == nil && len(matches) > 0 {
		return true
	}
	return false
}

// RepoLikelyHasCSharpIntent reports whether a repo root carries a root-level
// .NET project marker (a solution or project file), used to decide whether to
// wire the resolve-time C# helper for a tracked repo. Root-level only, like
// the TS/Python probes.
func RepoLikelyHasCSharpIntent(absRoot string) bool {
	for _, pattern := range []string{"*.sln", "*.slnx", "*.csproj"} {
		if matches, err := filepath.Glob(filepath.Join(absRoot, pattern)); err == nil && len(matches) > 0 {
			return true
		}
	}
	return false
}

// CSharpResolverEnv opts the resolve-time C# LSP helper in. OFF by default —
// unlike tsserver/pyright, the Roslyn workspace load behind csharp-ls costs
// minutes and hundreds of MB on a large solution, so repos only pay it when
// the operator asks for LSP-grade C# resolution ("1" / "true").
const CSharpResolverEnv = "GORTEX_LSP_RESOLVER_CSHARP"

// csharpResolverHelperEnabled reports whether the resolve-time C# helper may
// be wired for repos with C# intent.
func csharpResolverHelperEnabled() bool {
	// Set-and-not-falsy, mirroring the sibling GORTEX_LSP_RESOLVER's
	// value vocabulary — "on"/"yes" enable instead of failing silently.
	return strings.TrimSpace(os.Getenv(CSharpResolverEnv)) != "" && !IsFalsyEnv(CSharpResolverEnv)
}

// BuildResolverLSPHelper constructs the resolve-time LSP helper for a
// workspace, choosing the router-cached lazy path (poolSize <= 1, reuses
// the router's idle reaper) or the fresh-spawn pool path (poolSize > 1,
// opt-in via GORTEX_LSP_POOL_SIZE; keeps every spawn alive, so only for
// small tracked-workspace counts).
func BuildResolverLSPHelper(router *lsp.Router, spec *lsp.ServerSpec, absRoot string, poolSize int, logger *zap.Logger) *lsp.ResolverHelper {
	if poolSize <= 1 {
		return lsp.NewPinnedLazyResolverHelper(
			func() (*lsp.Provider, func(), error) {
				return router.AcquireSpecWorkspace(spec, absRoot)
			},
			absRoot,
			spec.Extensions,
			0,
			logger,
		)
	}
	return lsp.NewPooledResolverHelper(
		func() (*lsp.Provider, error) {
			return lsp.SpawnProviderForResolver(spec, absRoot, logger)
		},
		absRoot,
		spec.Extensions,
		0,
		poolSize,
		logger,
	)
}

// BuildResolverLSPHelperForRepo constructs a per-repo helper that can route
// each file extension to the appropriate language server. The returned spec
// names are the helpers included in the mux, for logging and diagnostics.
func BuildResolverLSPHelperForRepo(router *lsp.Router, absRoot string, poolSize int, logger *zap.Logger) (resolver.LSPHelper, []string) {
	if router == nil {
		return nil, nil
	}

	var helpers []resolver.LSPHelper
	var specs []string
	add := func(name string, enabled bool) {
		if !enabled {
			return
		}
		spec := lsp.SpecByName(name)
		if spec == nil || !router.Available(spec) {
			return
		}
		helpers = append(helpers, BuildResolverLSPHelper(router, spec, absRoot, poolSize, logger))
		specs = append(specs, name)
	}

	add(preferredSpecName(router.Available, ".ts"), RepoLikelyHasTypeScriptIntent(absRoot))
	add(preferredSpecName(router.Available, ".py"), RepoLikelyHasPythonIntent(absRoot))
	// C# is opt-in (CSharpResolverEnv): the Roslyn workspace load behind the
	// helper is far heavier than the TS/Python servers'.
	add(preferredSpecName(router.Available, ".cs"),
		csharpResolverHelperEnabled() && RepoLikelyHasCSharpIntent(absRoot))

	return lsp.NewResolverHelperMux(helpers...), specs
}

// preferredSpecName returns the name of the highest-priority registered
// spec covering ext that the given availability check reports live, so
// resolve-time helpers agree with the registry's routing preference
// (tsgo over typescript-language-server, pyright over pyrefly). Falls
// back to the priority winner when none is available — the caller
// re-checks availability and skips registration anyway.
func preferredSpecName(available func(*lsp.ServerSpec) bool, ext string) string {
	specs := lsp.SpecsForExtension(ext)
	for _, spec := range specs {
		if available(spec) {
			return spec.Name
		}
	}
	if len(specs) > 0 {
		return specs[0].Name
	}
	return ""
}

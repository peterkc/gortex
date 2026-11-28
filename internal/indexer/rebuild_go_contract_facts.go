package indexer

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/zzet/gortex/internal/contracts"
)

// The existing seed projection uses module IDs only. The late Contracts
// pass needs the native version and source line from the same owner read.
type rebuildGoContractDep struct {
	Module  string `json:"module"`
	Version string `json:"version"`
	Line    int    `json:"line"`
}

func (f *rebuildGoManifest) captureContractDeps(data []byte) error {
	var modules []string
	for _, dep := range (&contracts.GoModExtractor{}).Extract("go.mod", data, nil, nil) {
		mp, ok := strings.CutPrefix(dep.ID, "dep::")
		version, hasVersion := dep.Meta["version"].(string)
		if !ok || !rebuildGoModulePath(mp) || strings.Contains(mp, "::") || !hasVersion || version == "" || !utf8.ValidString(version) {
			return fmt.Errorf("%w: unsupported root dependency record", errRebuildGoEvidence)
		}
		modules = append(modules, mp)
		f.ContractDeps = append(f.ContractDeps, rebuildGoContractDep{Module: mp, Version: version, Line: dep.Line})
	}
	slices.Sort(modules)
	f.DirectDeps = slices.Compact(modules)
	return nil
}

// Older checked manifests have only the direct dependency ID projection.
// They still serve the linker. Late contract replay distinguishes a checked
// empty set from missing payload for a nonempty projection.
func (f rebuildGoManifest) visitContractDeps(visit func(contracts.Contract) error) error {
	if f.Path != "go.mod" || len(f.DirectDeps) != 0 && len(f.ContractDeps) == 0 {
		return fmt.Errorf("%w: full dependency records for %q not covered", errRebuildGoEvidence, f.Path)
	}
	for _, dep := range f.ContractDeps {
		if err := visit(contracts.Contract{
			ID: "dep::" + dep.Module, Type: contracts.ContractDependency, Role: contracts.RoleConsumer,
			FilePath: f.Path, Line: dep.Line, Confidence: 1,
			Meta: map[string]any{"module": dep.Module, "version": dep.Version},
		}); err != nil {
			return err
		}
	}
	return nil
}

func validRebuildGoContractDeps(direct []string, deps []rebuildGoContractDep) bool {
	if len(deps) == 0 {
		return true // legacy seed-only checked fact
	}
	seen := make(map[string]struct{}, len(direct))
	var line int
	for _, dep := range deps {
		if dep.Line <= line || dep.Version == "" || !utf8.ValidString(dep.Version) {
			return false
		}
		if _, ok := slices.BinarySearch(direct, dep.Module); !ok {
			return false
		}
		line = dep.Line
		seen[dep.Module] = struct{}{}
	}
	return len(seen) == len(direct)
}

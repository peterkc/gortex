package indexer

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/zzet/gortex/internal/indexer/merkle"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/modules"
	"github.com/zzet/gortex/internal/parser/tsalias"
	"github.com/zzet/gortex/internal/search"
)

func isRebuildResolverConfig(key string) bool {
	if key == rebuildPnpmWorkspaceFile || key == rebuildCargoWorkspaceFile || isRebuildModuleLock(key) {
		return true
	}
	switch path.Base(key) {
	case "go.mod", "go.work", "jsconfig.json", "tsconfig.json", "package.json":
		return true
	default:
		return false
	}
}

// captureRebuildCheckoutResolverInputs binds root name selection and the Go,
// TS/JS and npm facts before the owner offers the lease for reuse. Reads of a
// root manifest are shared; only its normalizer (or name-only read) charges it.
func captureRebuildCheckoutResolverInputs(ctx context.Context, src *source.FilesystemSource, epoch rebuildCheckoutInputs,
	literalRoot bool, raw *int64, emit func(rebuildSidecarRow) error,
) (string, error) {
	return captureRebuildCheckoutFacts(ctx, src, epoch, literalRoot, false, raw, emit)
}

func captureRebuildCheckoutFacts(ctx context.Context, src *source.FilesystemSource, epoch rebuildCheckoutInputs,
	literalRoot, captureModules bool, raw *int64, emit func(rebuildSidecarRow) error,
) (string, error) {
	if src == nil || epoch.namespace == nil || raw == nil || *raw < 0 || *raw > rebuildCppRawLimit || emit == nil {
		return "", fmt.Errorf("%w: incomplete checkout resolver capture", errRebuildInputSidecar)
	}
	var goUsed, jsUsed bool
	for _, in := range epoch.files {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		switch in.Language {
		case "go":
			goUsed = true
		case "javascript", "typescript":
			jsUsed = true
		}
	}
	const goPath, npmPath = "go.mod", "package.json"
	readRoot := func(name string, limit int64) (*rebuildRootManifest, error) {
		m, err := readCheckedRebuildRootManifest(ctx, src, epoch, name, min(limit, *raw))
		if err == nil && m != nil && m.row.Kind != 0 {
			err = emit(m.row)
		}
		if err != nil && captureModules {
			err = fmt.Errorf("%w: %s: %w", errRebuildModuleInput, name, err)
		}
		return m, err
	}
	goLimit := int64(rebuildCppEntryLimit)
	if goUsed {
		goLimit = rebuildGoManifestLimit
	}
	rootMod, err := readRoot(goPath, goLimit)
	if err != nil {
		return "", err
	}
	if captureModules {
		if err := emitRebuildModuleFact(goPath, rootMod, emit); err != nil {
			return "", err
		}
	}
	if !goUsed {
		// Native seeds root dependencies even without a Go parse receipt.
		// No Go ownership queries are needed in this case.
		row := rebuildGoManifest{Path: goPath}
		if rootMod != nil {
			row.Present, row.Digest = true, rootMod.target.Content
			*raw -= int64(len(rootMod.data))
			if err := row.captureContractDeps(rootMod.data); err != nil {
				return "", err
			}
			if rebuildGoManifestCharge(row) > rebuildGoFactLimit {
				return "", errRebuildGoCaptureLimit
			}
		}
		if err := emit(rebuildGoManifestSidecarRow(row)); err != nil {
			return "", err
		}
	}
	if err := captureRebuildResolverConfigRows(ctx, src.FrozenRoot(), literalRoot, epoch.namespace,
		epoch.files, epoch.admitted, rootMod, epoch.configObservations, raw, emit); err != nil {
		return "", err
	}
	writeNpm := func(key string, identity rebuildInput, data []byte) error {
		identity.Path = key // a root link retains the effective target identity under the logical key
		fact, err := normalizeRebuildNpmManifest(key, identity, data, raw)
		if err != nil {
			return err
		}
		row, err := rebuildNpmManifestSidecarRow(fact)
		if err != nil {
			return err
		}
		return emit(row)
	}
	var pkgConsulted, pkgHasWorkspaces bool
	readPackage := func() ([]byte, error) {
		pkgConsulted = true
		m, err := readRoot(npmPath, rebuildNpmFactBytes-int64(len(npmPath)))
		if err != nil {
			return nil, err
		}
		if captureModules {
			if err := emitRebuildModuleFact(npmPath, m, emit); err != nil {
				return nil, err
			}
		}
		if m == nil {
			return nil, nil
		}
		pkgHasWorkspaces = len(modules.ParsePackageJSONWorkspaces(m.data)) != 0
		return m.data, writeNpm(npmPath, m.target, m.data)
	}
	name, err := search.PortableProjectName(func(key string) ([]byte, error) {
		if key == npmPath {
			return readPackage()
		}
		if rootMod == nil {
			return nil, nil
		}
		return rootMod.data, nil
	})
	if err != nil {
		return "", err
	}
	if captureModules {
		if !pkgConsulted {
			if _, err := readPackage(); err != nil {
				return "", err
			}
		}
		for _, spec := range rootManifests() {
			if !isRebuildModuleLock(spec.path) {
				continue
			}
			m, err := readRoot(spec.path, rebuildCppEntryLimit)
			if err != nil {
				return "", err
			}
			if m != nil {
				*raw -= int64(len(m.data))
			}
			if err := emitRebuildModuleFact(spec.path, m, emit); err != nil {
				return "", err
			}
		}
	}
	if !goUsed && !jsUsed && !captureModules {
		return name, ctx.Err()
	}
	// If the selector did not need package.json, capture only a positive
	// admitted candidate. The namespace carries denied/unknown answers to
	// replay; they must not prevent parsing or invent a negative.
	if _, observed := epoch.configObservations[npmPath]; !pkgConsulted && (epoch.admitted[npmPath] != nil || observed) {
		if _, err := readPackage(); err != nil {
			return "", err
		}
	}
	// Read lower-priority workspace declarations only after a proved miss
	// at package.json. An unproved root will refuse when membership is used.
	if kind, err := epoch.namespace.lookup(npmPath); !pkgHasWorkspaces &&
		(pkgConsulted || err == nil && (kind == rebuildNamespaceAbsent || kind == rebuildNamespaceDirectory)) {
		if err := captureRebuildWorkspaceConfigs(ctx, src, epoch, raw, emit); err != nil {
			return "", err
		}
	}
	if !jsUsed {
		return name, ctx.Err()
	}
	// A post-parse npm probe may name any admitted nested package manifest.
	// Emit one positive fact per regular manifest; the namespace owns misses.
	var nested []string
	for key, in := range epoch.admitted {
		if key != npmPath && path.Base(key) == npmPath && in.Kind == merkle.RegularFile {
			nested = append(nested, key)
		}
	}
	for key := range epoch.configObservations {
		if key != npmPath && path.Base(key) == npmPath {
			nested = append(nested, key)
		}
	}
	slices.Sort(nested)
	for _, key := range nested {
		limit := min(*raw, rebuildNpmFactBytes-int64(len(key)))
		if in := epoch.admitted[key]; in != nil {
			data, err := readCheckedRebuildRegularManifest(ctx, src, *in, limit)
			if err != nil {
				return "", err
			}
			if err := writeNpm(key, *in, data); err != nil {
				return "", err
			}
			continue
		}
		kind, err := epoch.namespace.lookup(key)
		if err != nil {
			return "", err
		}
		if kind != rebuildNamespaceRegular {
			return "", fmt.Errorf("%w: npm observation %q is not a checked regular file", errRebuildResolverNamespace, key)
		}
		m, err := readRebuildConfigObservation(ctx, epoch.configObservations[key], limit)
		if err != nil {
			if errors.Is(err, source.ErrRegularFileTooLarge) {
				err = errors.Join(errRebuildCppCaptureLimit, err)
			}
			return "", err
		}
		if err := writeNpm(key, m.target, m.data); err != nil {
			return "", err
		}
	}
	return name, ctx.Err()
}

// captureRebuildResolverConfigRows writes the selected callers' normalized
// configs into the same lease as their checked namespace. The enumerating
// owner must supply inputs and manifest identities from that namespace epoch.
// Followed manifest rows belong to the owner, not this config projection.
func captureRebuildResolverConfigRows(ctx context.Context, root *os.Root, literalRoot bool, ns *rebuildResolverNamespace,
	inputs []rebuildInput, identities map[string]*rebuildInput, rootMod *rebuildRootManifest,
	configObservations map[string]source.FrozenRootPath, rawRemaining *int64, emit func(rebuildSidecarRow) error,
) error {
	if emit == nil {
		return fmt.Errorf("%w: missing resolver config row sink", errRebuildInputSidecar)
	}
	var goUsed, tsUsed bool
	for _, in := range inputs {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch in.Language {
		case "go":
			goUsed = true
		case "typescript", "javascript":
			tsUsed = true
		default:
			continue
		}
		if in.Kind != merkle.RegularFile || ns == nil {
			return fmt.Errorf("%w: unproved selected caller %q", errRebuildResolverNamespace, in.Path)
		}
		kind, err := ns.lookup(in.Path)
		if err != nil {
			return err
		}
		if ns.entries[path.Dir(in.Path)].close == rebuildNamespaceUnknown {
			return fmt.Errorf("%w: unclosed selected caller parent %q", errRebuildResolverNamespace, in.Path)
		}
		if kind != rebuildNamespaceRegular {
			return fmt.Errorf("%w: caller %q differs from namespace", ErrBuildInputChanged, in.Path)
		}
	}
	if goUsed {
		if err := captureRebuildGoObservations(ctx, root, ns, inputs, identities, rootMod, configObservations, rawRemaining, func(row rebuildGoManifest) error {
			return emit(rebuildGoManifestSidecarRow(row))
		}); err != nil {
			return err
		}
	}
	if !tsUsed {
		return ctx.Err()
	}
	configs, err := captureRebuildTSAliasConfigs(ctx, root, literalRoot, ns, inputs, identities, configObservations, rawRemaining)
	if err != nil {
		return err
	}
	for _, key := range slices.Sorted(maps.Keys(configs)) {
		if err := ctx.Err(); err != nil {
			return err
		}
		row, err := rebuildTSAliasConfigSidecarRow(configs[key])
		if err != nil {
			return err
		}
		if err := emit(row); err != nil {
			return err
		}
	}
	return nil
}

// Paths and content digests belong to the row, not the normalized payload.
// The payload strings have already been parsed as module paths or JSON strings.
// The writer rejects invalid UTF-8 in them; the row key is always raw bytes.
type rebuildResolverConfigPayload struct {
	RootClaim    string                     `json:"root_claim,omitempty"`
	OwnerModule  string                     `json:"owner_module,omitempty"`
	DirectDeps   []string                   `json:"direct_deps,omitempty"`
	Replace      []rebuildGoReplace         `json:"replace,omitempty"`
	Alias        *tsalias.FrozenAliasConfig `json:"alias,omitempty"`
	ContractDeps []rebuildGoContractDep     `json:"contract_deps,omitempty"`
}

func rebuildGoManifestSidecarRow(f rebuildGoManifest) rebuildSidecarRow {
	r := rebuildSidecarRow{Kind: rebuildGoManifestFact, Path: []byte(f.Path), Present: f.Present, Digest: f.Digest}
	if f.RootClaim != "" || f.OwnerModule != "" || len(f.DirectDeps) != 0 || len(f.Replace) != 0 || len(f.ContractDeps) != 0 {
		r.Config = &rebuildResolverConfigPayload{
			RootClaim: f.RootClaim, OwnerModule: f.OwnerModule, DirectDeps: f.DirectDeps, Replace: f.Replace,
			ContractDeps: f.ContractDeps,
		}
	}
	return r
}

func rebuildTSAliasConfigSidecarRow(f rebuildTSAliasFact) (rebuildSidecarRow, error) {
	if f.Kind != rebuildTSAliasConfig {
		return rebuildSidecarRow{}, fmt.Errorf("%w: expected TS/JS config, not a probe", errRebuildInputSidecar)
	}
	r := rebuildSidecarRow{Kind: rebuildTSAliasConfigFact, Path: []byte(f.Path), Present: f.Present, Digest: f.Digest}
	if f.Config != nil {
		r.Config = &rebuildResolverConfigPayload{Alias: f.Config}
	}
	return r, nil
}

func (r rebuildSidecarRow) isResolverConfig() bool {
	return r.Kind == rebuildGoManifestFact || r.Kind == rebuildTSAliasConfigFact
}

func (r rebuildSidecarRow) checkResolverConfig() error {
	key := string(r.Path)
	if !validRebuildNamespaceKey(key) || len(r.Dirs) != 0 || r.Present != (r.Digest != (rebuildDigest{})) ||
		(!r.Present && r.Config != nil) {
		return fmt.Errorf("%w: inconsistent resolver config row %q", errRebuildInputSidecar, key)
	}
	base := path.Base(key)
	p := r.Config
	switch r.Kind {
	case rebuildGoManifestFact:
		if (base == "go.mod" || base == "go.work" && p == nil) &&
			(p == nil || p.validGo(key == "go.mod")) {
			return nil
		}
	case rebuildTSAliasConfigFact:
		if (base == "jsconfig.json" || base == "tsconfig.json") &&
			(p == nil || !p.hasGoFields() && validRebuildSidecarAlias(p.Alias)) {
			return nil
		}
	}
	return fmt.Errorf("%w: invalid normalized resolver config %q", errRebuildInputSidecar, key)
}

func (p *rebuildResolverConfigPayload) hasGoFields() bool {
	return p.RootClaim != "" || p.OwnerModule != "" || len(p.DirectDeps) != 0 || len(p.Replace) != 0 || len(p.ContractDeps) != 0
}

func (p *rebuildResolverConfigPayload) validGo(root bool) bool {
	if p.Alias != nil || !p.hasGoFields() ||
		(!root && (p.RootClaim != "" || len(p.DirectDeps) != 0 || len(p.Replace) != 0 || len(p.ContractDeps) != 0)) ||
		(p.RootClaim != "" && !rebuildGoModulePath(p.RootClaim)) ||
		(p.OwnerModule != "" && (!rebuildGoModulePath(p.OwnerModule) || len(p.Replace) != 0)) {
		return false
	}
	for i, dep := range p.DirectDeps {
		if !rebuildGoModulePath(dep) || i > 0 && dep <= p.DirectDeps[i-1] {
			return false
		}
	}
	if !validRebuildGoContractDeps(p.DirectDeps, p.ContractDeps) {
		return false
	}
	for i, rep := range p.Replace {
		if !rebuildGoModulePath(rep.Module) {
			return false
		}
		if i > 0 {
			prev := p.Replace[i-1]
			if prev.Module > rep.Module || prev.Module == rep.Module && (prev.Local || !rep.Local) {
				return false
			}
		}
	}
	return true
}

// Check the shape emitted by ParseFrozenAliasConfig, not by a raw config
// parser. Never sort patterns or targets or clean wildcard fragments in replay.
func validRebuildSidecarAlias(cfg *tsalias.FrozenAliasConfig) bool {
	if cfg == nil {
		return false
	}
	if len(cfg.Entries) == 0 {
		return cfg.BaseURL == "" && cfg.Entries == nil // selected empty scope, not no-scope
	}
	if cfg.BaseURL != "" && (!validRebuildAliasFragment(cfg.BaseURL) || path.Clean(cfg.BaseURL) != cfg.BaseURL) {
		return false
	}
	prevRank, prevPattern := 0, ""
	for i, a := range cfg.Entries {
		if !utf8.ValidString(a.AliasPrefix) || !utf8.ValidString(a.AliasSuffix) || strings.ContainsRune(a.AliasPrefix, '*') ||
			(!a.HasWildcard && a.AliasSuffix != "") || len(a.Targets) == 0 ||
			a.TargetPrefix != a.Targets[0].Prefix || a.TargetSuffix != a.Targets[0].Suffix {
			return false
		}
		pattern := a.AliasPrefix
		if a.HasWildcard {
			pattern += "*" + a.AliasSuffix
		}
		rank := len(a.AliasPrefix)
		if i > 0 && (rank > prevRank || rank == prevRank && pattern <= prevPattern) {
			return false
		}
		prevRank, prevPattern = rank, pattern
		for _, target := range a.Targets {
			if !validRebuildAliasFragment(target.Prefix) || strings.ContainsRune(target.Prefix, '*') ||
				!utf8.ValidString(target.Suffix) || strings.ContainsAny(target.Suffix, "\\\x00") ||
				(!a.HasWildcard && target.Suffix != "") {
				return false
			}
		}
	}
	return true
}

func validRebuildAliasFragment(s string) bool {
	return utf8.ValidString(s) && !path.IsAbs(s) && !strings.ContainsAny(s, "\\\x00") && (len(s) < 2 || s[1] != ':')
}

type rebuildGoManifests map[string]rebuildGoManifest

func (rows rebuildGoManifests) lookup(key string) (rebuildGoManifest, error) {
	if row, ok := rows[key]; ok {
		return row, nil
	}
	return rebuildGoManifest{}, fmt.Errorf("%w: manifest row %q not covered", errRebuildGoEvidence, key)
}

// openRebuildResolverConfigFacts reads the private sidecar only. Stage bounded
// rows during the complete visit, but build neither reply map until the digest
// and framing have passed. Missing keys remain unproved, including on nil maps.
func openRebuildResolverConfigFacts(ctx context.Context, lease string, expected rebuildDigest) (rebuildGoManifests, rebuildTSAliasConfigs, error) {
	var goRows []rebuildGoManifest
	var tsRows []rebuildTSAliasFact
	var followedGoMod rebuildDigest
	goLeft, tsLeft := rebuildGoFactLimit, rebuildTSAliasConfigRetainedLimit
	if err := visitRebuildInputSidecar(ctx, lease, expected, func(r rebuildSidecarRow) error {
		switch r.Kind {
		case rebuildGoManifestFact:
			f := rebuildGoManifest{Path: string(r.Path), Present: r.Present, Digest: r.Digest}
			if p := r.Config; p != nil {
				f.RootClaim, f.OwnerModule, f.DirectDeps, f.Replace = p.RootClaim, p.OwnerModule, p.DirectDeps, p.Replace
				f.ContractDeps = p.ContractDeps
			}
			goLeft -= rebuildGoManifestCharge(f)
			if len(goRows) == rebuildGoKeyLimit || goLeft < 0 {
				return fmt.Errorf("%w: replay Go manifest rows: %w", errRebuildInputSidecar, errRebuildGoCaptureLimit)
			}
			goRows = append(goRows, f)
		case rebuildTSAliasConfigFact:
			f := rebuildTSAliasFact{Kind: rebuildTSAliasConfig, Path: string(r.Path), Present: r.Present, Digest: r.Digest}
			if r.Config != nil {
				f.Config = r.Config.Alias
			}
			tsLeft -= len(f.Path) + rebuildTSAliasConfigCharge(f.Config)
			if len(tsRows) == rebuildTSAliasConfigRowLimit || tsLeft < 0 {
				return fmt.Errorf("%w: replay TS/JS config rows: %w", errRebuildInputSidecar, errRebuildTSAliasCaptureLimit)
			}
			tsRows = append(tsRows, f)
		case rebuildFollowedManifest:
			if string(r.Path) == "go.mod" {
				if followedGoMod != (rebuildDigest{}) {
					return fmt.Errorf("%w: duplicate followed go.mod", errRebuildInputSidecar)
				}
				followedGoMod = r.Digest
			}
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	goFacts := make(rebuildGoManifests, len(goRows))
	for _, f := range goRows {
		if _, duplicate := goFacts[f.Path]; duplicate {
			return nil, nil, fmt.Errorf("%w: duplicate Go manifest %q", errRebuildInputSidecar, f.Path)
		}
		if f.Path == "go.mod" && followedGoMod != (rebuildDigest{}) && f.Digest != followedGoMod {
			return nil, nil, fmt.Errorf("%w: conflicting followed go.mod content digest", errRebuildInputSidecar)
		}
		goFacts[f.Path] = f
	}
	tsFacts := make(rebuildTSAliasConfigs, len(tsRows))
	for _, f := range tsRows {
		if _, duplicate := tsFacts[f.Path]; duplicate {
			return nil, nil, fmt.Errorf("%w: duplicate TS/JS config %q", errRebuildInputSidecar, f.Path)
		}
		tsFacts[f.Path] = f
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return goFacts, tsFacts, nil
}

// Match the normalized manifest charge in captureRebuildGoManifests. Capture
// also charges its temporary directory set, which replay does not reconstruct.
func rebuildGoManifestCharge(f rebuildGoManifest) int {
	charge := len(f.Path) + len(f.RootClaim) + len(f.OwnerModule)
	for _, dep := range f.DirectDeps {
		charge += len(dep) + 16
	}
	for _, dep := range f.ContractDeps {
		charge += len(dep.Module) + len(dep.Version) + 32
	}
	for _, rep := range f.Replace {
		charge += len(rep.Module) + 16
	}
	return charge
}

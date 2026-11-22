package indexer

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/zzet/gortex/internal/indexer/merkle"
	"github.com/zzet/gortex/internal/parser/tsalias"
)

// captureRebuildResolverConfigRows writes the selected callers' normalized
// configs into the same lease as their checked namespace. The enumerating
// owner must supply inputs and manifest identities from that namespace epoch.
// Followed manifest rows belong to the owner, not this config projection.
func captureRebuildResolverConfigRows(ctx context.Context, root *os.Root, literalRoot bool, ns *rebuildResolverNamespace,
	inputs []rebuildInput, identities map[string]*rebuildInput, followed *rebuildFollowedGoMod,
	rawRemaining *int64, emit func(rebuildSidecarRow) error,
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
		if err := captureRebuildGoObservations(ctx, root, ns, inputs, identities, followed, rawRemaining, func(row rebuildGoManifest) error {
			return emit(rebuildGoManifestSidecarRow(row))
		}); err != nil {
			return err
		}
	}
	if !tsUsed {
		return ctx.Err()
	}
	configs, err := captureRebuildTSAliasConfigs(ctx, root, literalRoot, ns, inputs, identities, rawRemaining)
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
	RootClaim   string                     `json:"root_claim,omitempty"`
	OwnerModule string                     `json:"owner_module,omitempty"`
	DirectDeps  []string                   `json:"direct_deps,omitempty"`
	Replace     []rebuildGoReplace         `json:"replace,omitempty"`
	Alias       *tsalias.FrozenAliasConfig `json:"alias,omitempty"`
}

func rebuildGoManifestSidecarRow(f rebuildGoManifest) rebuildSidecarRow {
	r := rebuildSidecarRow{Kind: rebuildGoManifestFact, Path: []byte(f.Path), Present: f.Present, Digest: f.Digest}
	if f.RootClaim != "" || f.OwnerModule != "" || len(f.DirectDeps) != 0 || len(f.Replace) != 0 {
		r.Config = &rebuildResolverConfigPayload{
			RootClaim: f.RootClaim, OwnerModule: f.OwnerModule, DirectDeps: f.DirectDeps, Replace: f.Replace,
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
	return p.RootClaim != "" || p.OwnerModule != "" || len(p.DirectDeps) != 0 || len(p.Replace) != 0
}

func (p *rebuildResolverConfigPayload) validGo(root bool) bool {
	if p.Alias != nil || !p.hasGoFields() ||
		(!root && (p.RootClaim != "" || len(p.DirectDeps) != 0 || len(p.Replace) != 0)) ||
		(p.RootClaim != "" && !rebuildGoModulePath(p.RootClaim)) ||
		(p.OwnerModule != "" && (!rebuildGoModulePath(p.OwnerModule) || len(p.Replace) != 0)) {
		return false
	}
	for i, dep := range p.DirectDeps {
		if !rebuildGoModulePath(dep) || i > 0 && dep <= p.DirectDeps[i-1] {
			return false
		}
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
	for _, rep := range f.Replace {
		charge += len(rep.Module) + 16
	}
	return charge
}

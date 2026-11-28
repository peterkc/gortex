package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/indexer/merkle"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser/tsalias"
)

const (
	rebuildTSAliasConfigRowLimit      = 32 << 10
	rebuildTSAliasConfigRowCharge     = 128
	rebuildTSAliasConfigRetainedLimit = 8 << 20
)

// Present means the exact name is a file in the native config walk, not a
// stat claim. A checked directory, absent name or path below a directory
// link supplies no config. A present row with nil Config was read but did
// not define a scope. A missing row is never negative authority.
type rebuildTSAliasConfigs map[string]rebuildTSAliasFact

func (rows rebuildTSAliasConfigs) lookup(rel string) (rebuildTSAliasFact, error) {
	if row, ok := rows[rel]; ok {
		return row, nil
	}
	return rebuildTSAliasFact{}, fmt.Errorf("%w: config row %q not covered", tsalias.ErrFrozenAliasUnproved, rel)
}

// config is the read-only provider for ResolveFrozenAlias. A nil map fails
// closed; neither it nor a missing key can reopen the checkout.
func (rows rebuildTSAliasConfigs) config(rel string) (*tsalias.FrozenAliasConfig, error) {
	row, err := rows.lookup(rel)
	return row.Config, err
}

// resolve re-evaluates a post-parse alias from the verified sidecar. Native
// probe order and fallback remain in tsalias; there is no live resolver read.
func (rows rebuildTSAliasConfigs) resolve(probe func(string) (bool, error), caller, spec string) (tsalias.FrozenAliasStem, error) {
	return tsalias.ResolveFrozenAlias(caller, spec, rows.config, probe)
}

// captureRebuildTSAliasConfigs collects only config candidates in the literal
// ancestry of admitted TS/JS files. The namespace, caller list and identities
// must describe the same owner epoch before reuse. literalRoot certifies that
// root is the native WalkDir directory (not a followed root link) and that
// rooted reads of listed spellings name the checked objects. The owner must
// bind or discard the result with that epoch; listing is not an atomic snapshot.
// identities contains independently admitted, frozen regular config identities.
// A positive name/type alone does not authorize reading its bytes.
func captureRebuildTSAliasConfigs(ctx context.Context, root *os.Root, literalRoot bool, n *rebuildResolverNamespace,
	inputs []rebuildInput, identities map[string]*rebuildInput, configObservations map[string]source.FrozenRootPath, rawRemaining *int64,
) (rebuildTSAliasConfigs, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows := make(rebuildTSAliasConfigs)
	walked := make(map[string]bool)
	left := rebuildTSAliasConfigRetainedLimit
	var collect func(string) error
	collect = func(dir string) error {
		if _, seen := walked[dir]; seen {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		walk := true // Only directory names within the held root affect the walk.
		if dir != "." {
			parent := path.Dir(dir)
			if err := collect(parent); err != nil {
				return err
			}
			walk = walked[parent] && !tsalias.SkipDirName(path.Base(dir))
			if walk {
				switch n.entries[dir].kind {
				case rebuildNamespaceDirectory:
				case rebuildNamespaceUnsupported:
					// A checked non-directory is never descended by WalkDir.
					// This proves no config discovery, not a stat miss.
					walk = false
				default:
					return fmt.Errorf("%w: config walk directory %q not admitted or unproved", tsalias.ErrFrozenAliasUnproved, dir)
				}
			}
		}
		if walk && n.entries[dir].close == rebuildNamespaceUnknown {
			return fmt.Errorf("%w: config walk directory %q not closed", tsalias.ErrFrozenAliasUnproved, dir)
		}
		left -= len(dir)
		// Native WalkDir compares exact listed bytes, even on filesystems
		// where stat would accept a different spelling. Both closure kinds
		// suffice here. Only follow-stat needs its own name-equality proof.
		for _, name := range [...]string{"jsconfig.json", "tsconfig.json"} {
			key := path.Join(dir, name)
			if len(rows) == rebuildTSAliasConfigRowLimit || !validRebuildNamespaceKey(key) || len(key)+rebuildTSAliasConfigRowCharge > left {
				return fmt.Errorf("%w: retained config row %q", errRebuildTSAliasCaptureLimit, key)
			}
			row := rebuildTSAliasFact{Kind: rebuildTSAliasConfig, Path: key}
			if walk {
				switch n.entries[key].kind {
				case rebuildNamespaceUnknown, rebuildNamespaceDirectory:
					// Exact name not listed as a file in this closed directory.
					if identities[key] != nil {
						return fmt.Errorf("%w: config identity conflicts with listing for %q", ErrBuildInputChanged, key)
					}
				case rebuildNamespaceRegular:
					if err := readRebuildTSAliasConfig(ctx, root, identities[key], configObservations[key], rawRemaining, &row); err != nil {
						return err
					}
				case rebuildNamespaceDenied:
					return fmt.Errorf("%w: config leaf %q: %w", tsalias.ErrFrozenAliasUnproved, key, errRebuildResolverInputDenied)
				default:
					// WalkDir offers link/special leaves to ReadFile. Neither a
					// denied leaf nor a non-regular type proves no scope.
					return fmt.Errorf("%w: config leaf %q denied or unsupported", tsalias.ErrFrozenAliasUnproved, key)
				}
			}
			left -= len(key) + rebuildTSAliasConfigCharge(row.Config)
			if left < 0 {
				return fmt.Errorf("%w: retained normalized config %q", errRebuildTSAliasCaptureLimit, key)
			}
			rows[key] = row
		}
		walked[strings.Clone(dir)] = walk
		return nil
	}
	for _, in := range inputs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if in.Language != "typescript" && in.Language != "javascript" {
			continue
		}
		if !validRebuildNamespaceKey(in.Path) {
			return nil, fmt.Errorf("%w: nonportable TS/JS caller %q", tsalias.ErrFrozenAliasUnproved, in.Path)
		}
		if root == nil || !literalRoot {
			return nil, fmt.Errorf("%w: native walk root or rooted byte spelling not proved", tsalias.ErrFrozenAliasUnproved)
		}
		if n == nil || !n.frozen || n.invalid != nil {
			return nil, fmt.Errorf("%w: checked config namespace not supplied", tsalias.ErrFrozenAliasUnproved)
		}
		if rawRemaining == nil || *rawRemaining < 0 || *rawRemaining > rebuildCppRawLimit {
			return nil, fmt.Errorf("%w: missing or invalid lease raw allowance", errRebuildTSAliasCaptureLimit)
		}
		if err := collect(path.Dir(in.Path)); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return rows, nil
}

func readRebuildTSAliasConfig(ctx context.Context, root *os.Root, in *rebuildInput, obs source.FrozenRootPath,
	rawRemaining *int64, row *rebuildTSAliasFact,
) error {
	key := row.Path
	limit := min(*rawRemaining, tsalias.MaxFrozenAliasConfigBytes)
	var data []byte
	if in == nil && obs.Info() != nil && obs.Path() == key {
		m, err := readRebuildConfigObservation(ctx, obs, limit)
		if errors.Is(err, source.ErrRegularFileTooLarge) {
			return fmt.Errorf("%w: raw config %q", errRebuildTSAliasCaptureLimit, key)
		}
		if err != nil {
			return fmt.Errorf("%w: read config %q: %w", tsalias.ErrFrozenAliasUnproved, key, err)
		}
		in, data = &m.target, m.data
	} else {
		if in == nil || in.Path != key || in.Kind != merkle.RegularFile || in.Size < 0 || in.Content == (rebuildDigest{}) {
			return fmt.Errorf("%w: no admitted regular identity for config %q", tsalias.ErrFrozenAliasUnproved, key)
		}
		if in.Size > limit {
			return fmt.Errorf("%w: raw config %q", errRebuildTSAliasCaptureLimit, key)
		}
		observed, err := source.ObserveFrozenLiteralRootPath(ctx, root, key)
		if err != nil {
			return fmt.Errorf("%w: observe config %q: %w", ErrBuildInputChanged, key, err)
		}
		if observed.Path() != key || !observed.Info().Mode().IsRegular() || observed.Info().Size() != in.Size {
			return fmt.Errorf("%w: config identity %q", ErrBuildInputChanged, key)
		}
		data, err = observed.Read(ctx, in.Size)
		if errors.Is(err, source.ErrFrozenRootChanged) {
			return fmt.Errorf("%w: read config %q: %w", ErrBuildInputChanged, key, err)
		}
		if err != nil {
			return fmt.Errorf("%w: read config %q: %w", tsalias.ErrFrozenAliasUnproved, key, err)
		}
	}
	*rawRemaining -= int64(len(data))
	if int64(len(data)) != in.Size || rebuildDigest(blake3.Sum256(data)) != in.Content {
		return fmt.Errorf("%w: config bytes %q", ErrBuildInputChanged, key)
	}
	cfg, err := tsalias.ParseFrozenAliasConfig(data)
	if err != nil {
		return fmt.Errorf("parse config %q: %w", key, err)
	}
	if cfg != nil {
		// The parser can trim BaseURL by slicing a much larger string.
		cfg.BaseURL = strings.Clone(cfg.BaseURL)
	}
	row.Present, row.Digest, row.Config = true, in.Content, cfg
	return nil
}

// Charge the retained slice capacities and string bytes, not just raw JSON
// length. Constants cover fixed row/payload and slice element storage; this
// is not an RSS cap or a substitute for the sidecar's encoded size limits.
func rebuildTSAliasConfigCharge(cfg *tsalias.FrozenAliasConfig) int {
	if cfg == nil {
		return rebuildTSAliasConfigRowCharge
	}
	charge := rebuildTSAliasConfigRowCharge + len(cfg.BaseURL) + cap(cfg.Entries)*128
	for _, alias := range cfg.Entries {
		charge += len(alias.AliasPrefix) + len(alias.AliasSuffix) + 1 + cap(alias.Targets)*32
		for _, target := range alias.Targets {
			charge += len(target.Prefix) + len(target.Suffix) + 1
		}
	}
	return charge
}

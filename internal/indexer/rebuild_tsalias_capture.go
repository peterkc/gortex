package indexer

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser/tsalias"
)

var errRebuildTSAliasCaptureLimit = errors.New("TS alias input capture exceeds byte limit")

type rebuildTSAliasFactKind uint8

const (
	rebuildTSAliasConfig rebuildTSAliasFactKind = iota + 1
	rebuildTSAliasProbe
)

// Path is the exact requested slash path, not its resolved link target.
// A present config names the verified read bytes by digest. Its Config can
// still be nil (native no-scope). A negative config or any probe has no payload.
type rebuildTSAliasFact struct {
	Kind    rebuildTSAliasFactKind
	Path    string
	Present bool
	Digest  rebuildDigest
	Config  *tsalias.FrozenAliasConfig
}

// captureRebuildTSAlias records the consulted source facts for one module
// specifier in pinned checkout loader order. callerRel is already admitted
// and checkout-relative; specifier has no ::binding suffix. Relative imports
// need only the native graph probes and produce no source facts here.
//
// readConfig proves native WalkDir membership and returns verified admitted
// bytes within the requested limit, or a proved no-file (nil, false, nil).
// probeFile proves the native stat result against the same frozen admission.
// The callbacks own rooted link, file, and absence proofs. They must refuse
// ignored, outside, changed or unknown inputs, not turn them into misses.
//
// rawRemaining is shared with the other input groups. yield accounts encoded
// rows in the owning capture, in order and including repetitions. The caller
// discards all tentative output if this call returns an error.
func captureRebuildTSAlias(ctx context.Context, callerRel, specifier string, rawRemaining *int64,
	readConfig func(context.Context, string, int64) ([]byte, bool, error),
	probeFile func(context.Context, string) (bool, error),
	yield func(rebuildTSAliasFact) error,
) (tsalias.FrozenAliasStem, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if specifier == "" || strings.HasPrefix(specifier, "./") || strings.HasPrefix(specifier, "../") {
		return "", nil
	}
	if rawRemaining == nil || *rawRemaining < 0 ||
		len(callerRel) > tsalias.MaxFrozenAliasConfigBytes || len(specifier) > tsalias.MaxFrozenAliasConfigBytes {
		return "", errRebuildTSAliasCaptureLimit
	}
	if readConfig == nil || yield == nil {
		return "", fmt.Errorf("TS alias capture callback: %w", tsalias.ErrFrozenAliasUnproved)
	}
	config := func(rel string) (*tsalias.FrozenAliasConfig, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		limit := min(*rawRemaining, tsalias.MaxFrozenAliasConfigBytes)
		data, present, err := readConfig(ctx, rel, limit)
		if errors.Is(err, source.ErrRegularFileTooLarge) {
			return nil, errors.Join(errRebuildTSAliasCaptureLimit, err)
		}
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if int64(len(data)) > limit {
			return nil, errRebuildTSAliasCaptureLimit
		}
		fact := rebuildTSAliasFact{Kind: rebuildTSAliasConfig, Path: rel, Present: present}
		if present {
			*rawRemaining -= int64(len(data))
			fact.Digest = blake3.Sum256(data)
			fact.Config, err = tsalias.ParseFrozenAliasConfig(data)
			if err != nil {
				return nil, err
			}
		} else if len(data) != 0 {
			return nil, fmt.Errorf("negative config returned bytes: %w", tsalias.ErrFrozenAliasUnproved)
		}
		return fact.Config, yield(fact)
	}
	probe := func(rel string) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if len(rel) > tsalias.MaxFrozenAliasConfigBytes {
			return false, errRebuildTSAliasCaptureLimit
		}
		if probeFile == nil {
			return false, fmt.Errorf("TS alias probe callback: %w", tsalias.ErrFrozenAliasUnproved)
		}
		present, err := probeFile(ctx, rel)
		if err != nil {
			return false, err
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		return present, yield(rebuildTSAliasFact{Kind: rebuildTSAliasProbe, Path: rel, Present: present})
	}
	return tsalias.ResolveFrozenAlias(callerRel, specifier, config, probe)
}

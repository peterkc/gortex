package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// FreezeRebuildFastAPIInputs captures the native pre-loop FastAPI candidates.
func FreezeRebuildFastAPIInputs(ctx context.Context, build *store_pebble.BuildStore, selection FrameworkSynthesizerSelection) error {
	if !selection.allows(SynthFastAPIResolve) {
		return nil
	}
	return build.FreezeFastAPIInputs(ctx, fastapiCandidateEdge)
}

// RunRebuildFastAPI runs FastAPI's residual dependency and router pass.
func RunRebuildFastAPI(ctx context.Context, build *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (uint64, error) {
	if !selection.allows(SynthFastAPIResolve) {
		return 0, nil
	}
	return build.RebuildFastAPI(ctx, receipts, store_pebble.RebuildConventionRules{
		Shape:    rebuildFastAPIShape,
		Source:   fastapiResolveSource,
		Eligible: isConventionResolvable,
		Rank:     rebuildConventionRank,
		Rebind:   fastapiResolveRebind,
	})
}

func rebuildFastAPIShape(target, via string) ([]string, []string) {
	name, dirs, ok := fastapiResolveShape(target, via)
	if !ok || name == "" {
		return nil, nil
	}
	return []string{name}, dirs
}

func rebuildConventionRank(file, caller string, dirs []string) int {
	rank := 0
	if dirMatchesAny(file, dirs) {
		rank = 2
	}
	if dir := dirOf(caller); dir != "" && dirOf(file) == dir {
		rank++
	}
	return rank
}

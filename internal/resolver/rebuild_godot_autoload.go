package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildGodotAutoload applies the native autoload turn after Rails.
func RunRebuildGodotAutoload(ctx context.Context, build *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (uint64, error) {
	if !selection.allows(SynthGodotAutoload) {
		return 0, nil
	}
	return build.RebuildGodotAutoload(ctx, receipts, store_pebble.RebuildGodotAutoloadRules{
		Decl:       gdAutoloadDecl,
		MemberFile: gdScriptMemberFile,
		Call:       gdScriptCallKey,
		Rebind:     gdAutoloadRebind,
	})
}

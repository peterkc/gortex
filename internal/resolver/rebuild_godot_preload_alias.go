package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildGodotPreloadAlias binds the residual Calls after autoload.
func RunRebuildGodotPreloadAlias(ctx context.Context, build *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection,
) (uint64, error) {
	if !selection.allows(SynthGodotPreloadAlias) {
		return 0, nil
	}
	return build.RebuildGodotPreloadAlias(ctx, receipts, store_pebble.RebuildGodotPreloadAliasRules{
		Decl:            gdPreloadAliasDecl,
		MemberFile:      gdScriptMemberFile,
		Call:            gdScriptCallKey,
		SourceFile:      gdEdgeSourceFile,
		Rebind:          gdPreloadAliasRebind,
		DeclCandidate:   gdPreloadAliasDeclCandidate,
		ScriptFile:      isGDScriptFile,
		MemberKinds:     gdScriptMemberKinds,
		ReceiptLanguage: func(language string) bool { return language == "gdscript" },
	})
}

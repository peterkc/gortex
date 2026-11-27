package resolver

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func runRebuildCSharpPartial(ctx context.Context, build *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error]) error {
	return build.RebuildCSharpPartial(ctx, receipts, store_pebble.RebuildCSharpPartialRules{
		Group: func(n *graph.Node) (string, string, string) {
			if !csharpPartialType(n) {
				return "", "", ""
			}
			key := csharpTypeKeyOf(n)
			return key.repo, key.ns, key.name
		},
		Retarget: csharpPartialRetarget,
		CopyFrom: csharpPartialCopyFrom,
		Stamp: func(n *graph.Node, canonical string) bool {
			if n.ID == canonical {
				return csharpPartialStamp(n, "partial", true)
			}
			return csharpPartialStamp(n, metaPartialMergedInto, canonical)
		},
	})
}

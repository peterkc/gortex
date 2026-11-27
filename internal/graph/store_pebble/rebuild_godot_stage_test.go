package store_pebble

import (
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildGodotStageBoundaries(t *testing.T) {
	const source, script, language = "game/main.gd", "game/clock.gd", "gdscript"
	large := strings.Repeat("x", 16<<20)
	for _, mode := range []string{"nil input", "scratch owner", "declaration size", "file size", "member size", "Call size", "unrelated file size", "raw identity", "untracked move"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			alias := &graph.Node{ID: source + "::Clock", Name: "Clock", Kind: graph.KindVariable, Language: language, FilePath: source,
				Meta: map[string]any{"script": "clock.gd"}}
			file := &graph.Node{ID: script, Kind: graph.KindFile, Language: language, FilePath: script}
			member := &graph.Node{ID: script + "::tick", Name: "tick", Kind: graph.KindMethod, Language: language, FilePath: script}
			unrelated := &graph.Node{ID: "other.gd", Kind: graph.KindFile, Language: language, FilePath: "other.gd"}
			call := &graph.Edge{From: source, To: "unresolved::tick", Kind: graph.EdgeCalls, FilePath: source, Line: 2}
			switch mode {
			case "declaration size":
				alias.Meta["large"] = large
			case "file size":
				file.Meta = map[string]any{"large": large}
			case "member size":
				member.Meta = map[string]any{"large": large}
			case "Call size":
				call.Meta = map[string]any{"large": large}
			case "unrelated file size":
				unrelated.Meta = map[string]any{"large": large}
			}
			facts := []ParseFacts{
				{File: graph.FileMetaRow{FilePath: source}, EffectiveLanguage: language, Nodes: []*graph.Node{alias}, Edges: []*graph.Edge{call}},
				{File: graph.FileMetaRow{FilePath: script}, EffectiveLanguage: language, Nodes: []*graph.Node{file, member}},
				{File: graph.FileMetaRow{FilePath: unrelated.FilePath}, EffectiveLanguage: language, Nodes: []*graph.Node{unrelated}},
			}
			b, receipts := seedSyntheticCustody(t, facts...)
			prefix := rebuildGodotPreloadAliasPrefix
			switch mode {
			case "nil input":
				receipts = nil
			case "scratch owner":
				require.NoError(t, b.pending.Set(prefix, []byte("owned"), pebble.NoSync))
			case "raw identity":
				raw := *call
				raw.Line++
				facts[0].Edges = []*graph.Edge{&raw}
				require.NoError(t, b.putEdgeWithCustody(call, rebuildEdgeCustody{raw: pendingEdgeKey(&raw)}))
			case "untracked move":
				require.NoError(t, b.putEdgeWithCustody(call, rebuildEdgeCustody{proof: rebuildProofUntrackedMove}))
			}
			rules := RebuildGodotPreloadAliasRules{
				Decl: func(n *graph.Node) (string, string) {
					path, _ := n.Meta["script"].(string)
					return n.FilePath, path
				},
				MemberFile: func(n *graph.Node) (string, bool) { return n.FilePath, true },
				Call:       func(*graph.Edge) (string, string) { return "Clock", "tick" },
				SourceFile: func(e *graph.Edge) string { return e.FilePath },
				Rebind: func(e *graph.Edge, target string) graph.EdgeReindex {
					old := e.To
					e.To = target
					return graph.EdgeReindex{Edge: e, OldTo: old}
				},
				DeclCandidate:   func(n *graph.Node) bool { return n.Kind == graph.KindVariable },
				ScriptFile:      func(n *graph.Node) bool { return n.Kind == graph.KindFile },
				MemberKinds:     [2]graph.NodeKind{graph.KindMethod, graph.KindFunction},
				ReceiptLanguage: func(value string) bool { return value == language },
			}
			count, err := b.RebuildGodotPreloadAlias(ctx, receipts, rules)
			if mode == "unrelated file size" {
				require.NoError(t, err)
				require.EqualValues(t, 1, count)
				installed := *call
				installed.To = member.ID
				requireRebuildLinkCustody(t, b, &installed, rebuildEdgeCustody{raw: pendingEdgeKey(call)})
			} else {
				require.ErrorIs(t, err, ErrRebuildGodotPreloadAlias)
				require.Zero(t, count)
				switch mode {
				case "raw identity", "untracked move":
					require.ErrorIs(t, err, ErrRebuildUnknownProvenance)
				case "declaration size", "file size", "member size", "Call size":
					require.ErrorIs(t, err, ErrRebuildLinkInputSize)
				}
				stored, err := b.pendingEdge(pendingEdgeKey(call))
				require.NoError(t, err)
				require.Equal(t, call, stored)
			}
			busy, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
			require.NoError(t, err)
			require.Equal(t, mode == "scratch owner", busy)
			if busy {
				data, err := pendingGet(b.pending, prefix, copyValue)
				require.NoError(t, err)
				require.Equal(t, "owned", string(data))
			}
		})
	}
}

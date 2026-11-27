package store_pebble

import (
	"path"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func testExpressRules() RebuildExpressRules {
	return RebuildExpressRules{
		Handler: func(e *graph.Edge) (string, string, string, bool) {
			class, _ := e.Meta["class"].(string)
			method, _ := e.Meta["method"].(string)
			name, _ := e.Meta["handler"].(string)
			return class, method, name, graph.IsUnresolvedTarget(e.To) && (class != "" || name != "")
		},
		Owner:      func(e *graph.Edge) (string, bool) { return e.To, e.To != "" },
		SimpleName: func(id string) string { return id },
		Candidate:  func(n *graph.Node) bool { return n.Kind == graph.KindFunction },
		Names:      func(name, _ string) []string { return []string{name} },
		Preferred:  func(_, _ string) bool { return false },
		Directory:  path.Dir,
		Rebind: func(e *graph.Edge, id string) bool {
			e.To = id
			return id != ""
		},
	}
}

func TestRebuildExpressScratchAndCustody(t *testing.T) {
	const file, lang = "src/routes.js", "javascript"
	target := &graph.Node{ID: "handler", Name: "auth", FilePath: file, Language: lang, Kind: graph.KindFunction}
	ra := &graph.Edge{
		From: "route", To: "unresolved::auth", FilePath: file, Kind: graph.EdgeCalls, Line: 3,
		Meta: map[string]any{"handler": "auth"},
	}
	for _, scenario := range []string{"success", "shared raw claim", "owned scratch"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := t.Context()
			b, receipts := seedSyntheticCustody(t, ParseFacts{
				File:              graph.FileMetaRow{FilePath: file},
				EffectiveLanguage: lang,
				Nodes:             []*graph.Node{target},
				Edges:             []*graph.Edge{ra},
			})
			live := *ra
			live.To = "unresolved::retargeted"
			require.NoError(t, b.ApplyLinkReindexes(ctx, []graph.EdgeReindex{{Edge: &live, OldTo: ra.To}}))
			if scenario == "shared raw claim" {
				stationary := *ra
				stationary.Meta = map[string]any{"incumbent": "preserve"}
				b.AddEdge(&stationary)
			}
			if scenario == "owned scratch" {
				require.NoError(t, b.pending.Set(rebuildExpressPrefix, []byte("owned"), pebble.NoSync))
			}
			stats, err := b.RebuildExpress(ctx, receipts, testExpressRules())
			if scenario == "success" {
				require.NoError(t, err)
				require.Equal(t, RebuildExpressStats{Stubs: 1, Moves: 1}, stats)
				installed := live
				installed.To = target.ID
				requireRebuildLinkCustody(t, b, &installed, rebuildEdgeCustody{raw: pendingEdgeKey(ra)})
			} else {
				require.ErrorIs(t, err, ErrRebuildExpress)
				if scenario == "shared raw claim" {
					require.ErrorIs(t, err, ErrRebuildUnknownProvenance)
				}
				require.Zero(t, stats)
				requireRebuildLinkCustody(t, b, &live, rebuildEdgeCustody{raw: pendingEdgeKey(ra)})
				got, err := b.pendingEdge(pendingEdgeKey(&live))
				require.NoError(t, err)
				require.Equal(t, &live, got)
				if scenario == "shared raw claim" {
					got, err := b.pendingEdge(pendingEdgeKey(ra))
					require.NoError(t, err)
					require.NotNil(t, got)
					require.Equal(t, "preserve", got.Meta["incumbent"])
				}
			}
			busy, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildExpressPrefix)
			require.NoError(t, err)
			require.Equal(t, scenario == "owned scratch", busy)
			if busy {
				owner, err := pendingGet(b.pending, rebuildExpressPrefix, copyValue)
				require.NoError(t, err)
				require.Equal(t, "owned", string(owner))
			}
		})
	}
}

func TestRebuildExpressRoleSizeLimits(t *testing.T) {
	const file, lang = "src/routes.js", "javascript"
	large := strings.Repeat("x", 16<<20)
	for _, role := range []string{"candidate", "method", "caller", "member", "call", "unrelated member", "unrelated call", "unrelated node", "unrelated caller directory"} {
		t.Run(role, func(t *testing.T) {
			ctx := t.Context()
			caller := &graph.Node{ID: "route", Name: "route", FilePath: file, Language: lang, Kind: graph.KindFunction}
			target := &graph.Node{ID: "auth", Name: "auth", FilePath: file, Language: lang, Kind: graph.KindFunction}
			other := &graph.Node{ID: "other", Name: "auth", FilePath: "other/auth.js", Language: lang, Kind: graph.KindFunction}
			method := &graph.Node{ID: "list", Name: "list", FilePath: file, Language: lang, Kind: graph.KindMethod}
			call := &graph.Edge{
				From: caller.ID, To: "unresolved::auth", Kind: graph.EdgeCalls, FilePath: file, Line: 2,
				Meta: map[string]any{"handler": "auth"},
			}
			member := &graph.Edge{From: method.ID, To: "Wanted", Kind: graph.EdgeMemberOf, FilePath: file, Line: 3}
			edges := []*graph.Edge{call, member}
			oversized := map[string]any{"large": large}
			switch role {
			case "candidate":
				target.Meta = oversized
			case "method", "member", "unrelated member":
				call.Meta = map[string]any{"class": "Wanted", "method": "list"}
				switch role {
				case "method":
					method.Meta = oversized
				case "member":
					member.Meta = oversized
				case "unrelated member":
					unknown := *member
					unknown.To, unknown.Meta = "Unrelated", oversized
					edges = []*graph.Edge{call, &unknown, member}
				}
			case "caller", "unrelated caller directory":
				caller.Meta = oversized
			case "call":
				call.Meta["large"] = large
			case "unrelated node":
				other.Name, other.Meta = "unused", oversized
			case "unrelated call":
				edges = append(edges, &graph.Edge{From: caller.ID, To: "settled", FilePath: file, Kind: graph.EdgeCalls, Line: 9, Meta: oversized})
			}
			b, receipts := seedSyntheticCustody(t, ParseFacts{
				File:              graph.FileMetaRow{FilePath: file},
				EffectiveLanguage: lang,
				Nodes:             []*graph.Node{caller, method, target},
				Edges:             edges,
			})
			if role == "caller" || role == "unrelated node" {
				b.AddNode(other)
			}
			stats, err := b.RebuildExpress(ctx, receipts, testExpressRules())
			if strings.HasPrefix(role, "unrelated ") {
				require.NoError(t, err)
				require.Equal(t, RebuildExpressStats{Stubs: 1, Moves: 1}, stats)
			} else {
				require.ErrorIs(t, err, ErrRebuildExpress)
				require.ErrorIs(t, err, ErrRebuildLinkInputSize)
				require.Zero(t, stats)
				old, err := b.pendingEdge(pendingEdgeKey(call))
				require.NoError(t, err)
				require.Equal(t, call, old)
			}
			busy, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildExpressPrefix)
			require.NoError(t, err)
			require.False(t, busy)
		})
	}
}

func TestRebuildExpressReceiptOnlyCandidatePresence(t *testing.T) {
	const file, lang = "src/routes.js", "javascript"
	caller := &graph.Node{ID: "route", Name: "route", FilePath: file, Language: lang, Kind: graph.KindFunction}
	target := &graph.Node{ID: "handler", Name: "auth", FilePath: "middleware/auth.js", Language: lang, Kind: graph.KindFunction}
	live := &graph.Node{ID: "extra", Name: "unrelated", FilePath: "other/extra.js", Language: lang, Kind: graph.KindType,
		Meta: map[string]any{"padding": strings.Repeat("x", 16<<20)}}
	call := &graph.Edge{From: caller.ID, To: "unresolved::auth", FilePath: file, Kind: graph.EdgeCalls, Line: 2,
		Meta: map[string]any{"handler": "auth"}}
	source := producerProofFacts(caller)
	source.Edges = []*graph.Edge{call}
	facts := []ParseFacts{source, producerProofFacts(target)}
	rules := testExpressRules()
	rules.Preferred = func(file, _ string) bool {
		return path.Dir(file) == "middleware"
	}
	for _, tc := range []struct {
		file   string
		refuse bool
	}{
		{"other/extra.js", false},
		{"middleware/extra.js", true},
	} {
		t.Run(tc.file, func(t *testing.T) {
			b, _ := seedSyntheticCustody(t, facts...)
			b.AddNode(live)
			before := rebuildFrameworkCustodyRows(t, b)
			raw := &graph.Node{ID: live.ID, Name: target.Name, FilePath: tc.file, Language: lang, Kind: graph.KindFunction}
			receipts := sqlCallsiteTestReceipts(facts[0], facts[1], producerProofFacts(raw))
			stats, err := b.RebuildExpress(t.Context(), receipts, rules)
			if tc.refuse {
				require.ErrorIs(t, err, ErrRebuildExpress)
				require.ErrorIs(t, err, ErrRebuildUnknownProvenance)
				require.NotErrorIs(t, err, ErrRebuildLinkInputSize)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
				return
			}
			require.NoError(t, err)
			require.Equal(t, RebuildExpressStats{Stubs: 1, Moves: 1}, stats)
			want := *call
			want.To = target.ID
			saved, err := b.pendingEdge(pendingEdgeKey(&want))
			require.NoError(t, err)
			require.Equal(t, &want, saved)
		})
	}
}

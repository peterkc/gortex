package store_pebble

import (
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func testLaravelEventRules() RebuildLaravelEventRules {
	return RebuildLaravelEventRules{
		ListenMap: func(n *graph.Node) string {
			value, _ := n.Meta["map"].(string)
			return value
		},
		ListenerType: func(n *graph.Node) string {
			value, _ := n.Meta["listens"].(string)
			return value
		},
		Entries: func(value string, visit func(string, string)) {
			event, class, ok := strings.Cut(value, "=>")
			if ok {
				visit(event, class)
			}
		},
		SimpleName: func(value string) string { return value },
		Call: func(e *graph.Edge) string {
			value, _ := e.Meta["event"].(string)
			return value
		},
		Rebind: func(e *graph.Edge, to string, matched bool) bool {
			if !matched || e.To == to {
				return false
			}
			e.To = to
			return true
		},
		Fanout: func(e *graph.Edge, to, _ string) *graph.Edge {
			add := *e
			add.To = to
			return &add
		},
	}
}

func TestRebuildLaravelRoleLimitsAndScratch(t *testing.T) {
	const file = "app.php"
	large := strings.Repeat("x", 16<<20)
	for _, role := range []string{"provider", "handle", "member", "raw role live node", "unrelated member", "unrelated node", "scratch occupied"} {
		t.Run(role, func(t *testing.T) {
			ctx := t.Context()
			provider := &graph.Node{ID: "provider", Kind: graph.KindType, FilePath: file, Meta: map[string]any{"map": "Notice=>Listener"}}
			handle := &graph.Node{ID: "listener", Kind: graph.KindMethod, Name: "handle", FilePath: file}
			other := &graph.Node{ID: "other", Kind: graph.KindType, FilePath: file}
			call := &graph.Edge{From: "publisher", To: "unresolved::old", Kind: graph.EdgeCalls, FilePath: file, Line: 4, Meta: map[string]any{"event": "Notice"}}
			member := &graph.Edge{From: handle.ID, To: "Listener", Kind: graph.EdgeMemberOf, FilePath: file, Line: 3}
			edges := []*graph.Edge{member, call}
			switch role {
			case "provider":
				provider.Meta["large"] = large
			case "handle":
				handle.Meta = map[string]any{"large": large}
			case "member":
				member.Meta = map[string]any{"large": large}
			case "raw role live node", "unrelated node":
				other.Meta = map[string]any{"large": large}
			case "unrelated member":
				extra := *member
				extra.To, extra.Meta = "Unrelated", map[string]any{"large": large}
				edges = append([]*graph.Edge{&extra}, edges...)
			}
			facts := ParseFacts{
				File:              graph.FileMetaRow{FilePath: file},
				EffectiveLanguage: "php",
				Nodes:             []*graph.Node{provider, handle, other},
				Edges:             edges,
			}
			b, receipts := seedSyntheticCustody(t, facts)
			if role == "raw role live node" {
				changed := *other
				changed.Meta = map[string]any{"map": "Notice=>Unrelated"}
				facts.Nodes[2] = &changed
			}
			if role == "scratch occupied" {
				require.NoError(t, b.pending.Set(rebuildLaravelEventPrefix, []byte("occupied"), pebble.NoSync))
			}
			stats, err := b.RebuildLaravelEvent(ctx, receipts, testLaravelEventRules())
			if strings.HasPrefix(role, "unrelated ") {
				require.NoError(t, err)
				require.Equal(t, RebuildLaravelEventStats{Stubs: 1, Resolved: 1, Moves: 1}, stats)
			} else {
				require.ErrorIs(t, err, ErrRebuildLaravelEvent)
				if role != "scratch occupied" {
					require.ErrorIs(t, err, ErrRebuildLinkInputSize)
				}
				require.Zero(t, stats)
				got, err := b.pendingEdge(pendingEdgeKey(call))
				require.NoError(t, err)
				require.Equal(t, call, got)
			}
			busy, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildLaravelEventPrefix)
			require.NoError(t, err)
			require.Equal(t, role == "scratch occupied", busy)
			if busy {
				value, err := pendingGet(b.pending, rebuildLaravelEventPrefix, copyValue)
				require.NoError(t, err)
				require.Equal(t, "occupied", string(value))
			}
		})
	}
}

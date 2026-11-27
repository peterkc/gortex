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

func TestRebuildLaravelStationaryCallCustody(t *testing.T) {
	const file = "app.php"
	const target = "unresolved::*.handle"
	listener := &graph.Node{ID: "listener", Kind: graph.KindMethod, FilePath: file, Meta: map[string]any{"listens": "Notice"}}
	call := &graph.Edge{From: "publish", To: target, Kind: graph.EdgeCalls, FilePath: file, Line: 3, Meta: map[string]any{"event": "Missing"}}
	for _, mode := range []string{"exact", "duplicate route", "wrong receipt file", "shared key", "wrong custody identity", "unknown custody"} {
		t.Run(mode, func(t *testing.T) {
			facts := ParseFacts{EffectiveLanguage: "php", File: graph.FileMetaRow{FilePath: file}, Nodes: []*graph.Node{listener}}
			b, _ := seedSyntheticCustody(t, facts)
			raw := *call
			custody := rebuildEdgeCustody{}
			switch mode {
			case "shared key":
				other := *call
				other.To, other.Meta = listener.ID, nil
				require.NoError(t, b.withWrites(func() error {
					return b.addEdgeWithCustody(&other, rebuildEdgeCustody{raw: pendingEdgeKey(&raw)})
				}))
			case "wrong custody identity":
				raw.Line++
				custody.raw = pendingEdgeKey(&raw)
			case "unknown custody":
				custody.proof = rebuildProofUntrackedMove
			}
			require.NoError(t, b.withWrites(func() error { return b.addEdgeWithCustody(call, custody) }))
			before := rebuildFrameworkCustodyRows(t, b)
			receipts := func(yield func(ParseFacts, error) bool) {
				if !yield(facts, nil) {
					return
				}
				if mode == "wrong receipt file" {
					facts.File.FilePath = "wrong.php"
				}
				facts.Edges = []*graph.Edge{&raw}
				if mode == "duplicate route" {
					duplicate := raw
					duplicate.Meta = map[string]any{"event": "Notice"}
					facts.Edges = append(facts.Edges, &duplicate)
				}
				yield(facts, nil)
			}
			stats, err := b.RebuildLaravelEvent(t.Context(), receipts, testLaravelEventRules())
			if mode == "exact" || mode == "duplicate route" {
				require.NoError(t, err)
				require.Equal(t, RebuildLaravelEventStats{Stubs: 1}, stats)
			} else {
				var refusal *RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refusal)
				require.Equal(t, &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(call)}, refusal)
				require.Zero(t, stats)
			}
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

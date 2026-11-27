package store_pebble

import (
	"errors"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func objectRegistryTestRules() RebuildObjectRegistryRules {
	return RebuildObjectRegistryRules{
		Call: func(e *graph.Edge) (string, string) {
			class, _ := e.Meta["registry_value"].(string)
			return class, "run"
		},
		ClassName:   func(id string) string { return id },
		MethodOrder: func(name string) []string { return []string{name} },
		Getter:      storeFactoryTestRules().Getter,
		Rebind:      storeFactoryTestRules().Rebind,
	}
}

func objectRegistryTestFacts() ParseFacts {
	const file = "registry.js"
	return ParseFacts{
		EffectiveLanguage: "javascript",
		File:              graph.FileMetaRow{FilePath: file, NodeCount: 1},
		Nodes: []*graph.Node{
			{ID: "method", Kind: graph.KindMethod, Name: "run", FilePath: file, Language: "javascript"},
		},
		Edges: []*graph.Edge{
			{From: "method", To: "Cmd", Kind: graph.EdgeMemberOf, FilePath: file, Line: 1},
			{From: "caller", To: "unresolved::*.Cmd", Kind: graph.EdgeCalls, FilePath: file, Line: 2,
				Meta: map[string]any{"registry_value": "Cmd"}},
		},
	}
}

func TestRebuildObjectRegistryInvalidInputs(t *testing.T) {
	b := newBuild(t, FullProfile)
	before := rebuildFrameworkCustodyRows(t, b)
	for _, input := range []string{"receipts", "call", "class name", "method order", "getter", "rebind"} {
		t.Run(input, func(t *testing.T) {
			receipts := unreadStoreFactoryReceipts(t)
			rules := objectRegistryTestRules()
			switch input {
			case "receipts":
				receipts = nil
			case "call":
				rules.Call = nil
			case "class name":
				rules.ClassName = nil
			case "method order":
				rules.MethodOrder = nil
			case "getter":
				rules.Getter = nil
			case "rebind":
				rules.Rebind = nil
			}
			stats, err := b.RebuildObjectRegistry(t.Context(), receipts, rules)
			require.ErrorIs(t, err, ErrRebuildObjectRegistry)
			require.ErrorIs(t, err, ErrRebuildLinkInputSize)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildObjectRegistryOccupiedScratch(t *testing.T) {
	b := newBuild(t, FullProfile)
	require.NoError(t, b.pending.Set(rebuildObjectRegistryPrefix, []byte("sentinel"), pebble.NoSync))
	before := rebuildFrameworkCustodyRows(t, b)
	stats, err := b.RebuildObjectRegistry(t.Context(), unreadStoreFactoryReceipts(t), objectRegistryTestRules())
	require.ErrorIs(t, err, ErrRebuildObjectRegistry)
	require.ErrorIs(t, err, errInvalidRecord)
	require.ErrorContains(t, err, "Object Registry scratch range occupied")
	require.Zero(t, stats)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
}

func TestRebuildObjectRegistryReceiptErrors(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		facts := objectRegistryTestFacts()
		b, _ := seedSyntheticCustody(t, facts)
		before := rebuildFrameworkCustodyRows(t, b)
		failure := errors.New("receipt read failed")
		replays := 0
		receipts := func(yield func(ParseFacts, error) bool) {
			replays++
			if yield(facts, nil) && replays == failAt {
				yield(ParseFacts{}, failure)
			}
		}
		stats, err := b.RebuildObjectRegistry(t.Context(), receipts, objectRegistryTestRules())
		require.ErrorIs(t, err, ErrRebuildObjectRegistry)
		require.ErrorIs(t, err, failure)
		require.Zero(t, stats)
		require.Equal(t, failAt, replays)
		require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
	}
}

func TestRebuildObjectRegistryCallRowCap(t *testing.T) {
	for _, producer := range []string{"owned", "foreign"} {
		t.Run(producer, func(t *testing.T) {
			const capBytes = 16 << 20
			facts := objectRegistryTestFacts()
			call := facts.Edges[1]
			call.Context = strings.Repeat("x", capBytes)
			if producer == "foreign" {
				delete(call.Meta, "registry_value")
			}
			b, _ := seedSyntheticCustody(t, facts)
			before := rebuildFrameworkCustodyRows(t, b)
			key := pendingEdgeKey(call)
			require.Greater(t, len(key)+len(before[string(key)]), capBytes)
			stats, err := b.RebuildObjectRegistry(t.Context(), unreadStoreFactoryReceipts(t), objectRegistryTestRules())
			if producer == "foreign" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrRebuildObjectRegistry)
				require.ErrorIs(t, err, ErrRebuildLinkInputSize)
				require.ErrorContains(t, err, "Object Registry edge row")
			}
			require.Zero(t, stats)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildObjectRegistryElectionRows(t *testing.T) {
	for _, malformed := range [][]byte{nil, {1}, {0, 'x'}, {2, 'x'}} {
		facts := objectRegistryTestFacts()
		b, _ := seedSyntheticCustody(t, facts)
		before := rebuildFrameworkCustodyRows(t, b)
		p := rebuildCallMoves{b: b, prefix: rebuildObjectRegistryPrefix}
		key := keyStrings(p.column('i'), "Cmd", "run")
		receipts := func(yield func(ParseFacts, error) bool) {
			require.NoError(t, b.pending.Set(key, malformed, pebble.NoSync))
			yield(facts, nil)
		}
		stats, err := b.RebuildObjectRegistry(t.Context(), receipts, objectRegistryTestRules())
		require.ErrorIs(t, err, ErrRebuildObjectRegistry)
		require.ErrorIs(t, err, errInvalidRecord)
		require.ErrorContains(t, err, "Object Registry election row")
		require.Zero(t, stats)
		require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
	}
}

func TestRebuildObjectRegistryBucketScope(t *testing.T) {
	facts := objectRegistryTestFacts()
	foreign := *facts.Nodes[0]
	foreign.ID = "foreign-method"
	member := *facts.Edges[0]
	member.From, member.To = foreign.ID, "Uncalled"
	facts.Nodes = append(facts.Nodes, &foreign)
	facts.Edges = append(facts.Edges, &member)
	b, receipts := seedSyntheticCustody(t, facts)
	p := rebuildObjectRegistryPass{
		rebuildCallMoves: rebuildCallMoves{
			b: b, prefix: rebuildObjectRegistryPrefix, rowBytes: 16 << 20, label: "Object Registry",
		},
		rules: objectRegistryTestRules(),
	}
	ctx := t.Context()
	require.NoError(t, p.collectEdges(ctx, graph.EdgeCalls))
	require.NoError(t, p.collectEdges(ctx, graph.EdgeMemberOf))
	require.NoError(t, p.orderMemberships(ctx, receipts))
	haveMethods, err := p.collectCandidates(ctx)
	require.NoError(t, err)
	require.True(t, haveMethods)
	for _, n := range facts.Nodes {
		class := "Cmd"
		if n.ID == foreign.ID {
			class = "Uncalled"
		}
		for _, key := range rebuildStoreScopeKeys(p.key('u', class, n.Name), len(p.prefix), 'i', n) {
			id, seen, err := p.unique(key)
			require.NoError(t, err)
			require.Equal(t, class == "Cmd", seen)
			if seen {
				require.Equal(t, n.ID, id)
			}
		}
	}
}

package store_pebble

import (
	"crypto/sha256"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildSpringEventsRejectsSharedArcReceipt(t *testing.T) {
	const file = "Events.java"
	listener := &graph.Node{ID: "listen", Kind: graph.KindMethod, Name: "listen", FilePath: file, Language: "java"}
	original := &graph.Edge{From: "Child", To: "unresolved::Base", Kind: graph.EdgeExtends, FilePath: file, Line: 1}
	call := &graph.Edge{From: "publish", To: "unresolved::old", Kind: graph.EdgeCalls, FilePath: file, Line: 2}
	for _, mode := range []string{"unique receipt", "shared receipt", "Swift/ObjC producer"} {
		b, receipts := seedSyntheticCustody(t, ParseFacts{
			EffectiveLanguage: "java",
			File:              graph.FileMetaRow{FilePath: file},
			Nodes:             []*graph.Node{listener},
		})
		b.AddEdge(call)
		primary, other := *original, *original
		primary.To, other.To = "Base", "Other"
		if mode == "Swift/ObjC producer" {
			primary.Kind, primary.FilePath = graph.EdgeImplements, "Child.swift"
		}
		require.NoError(t, b.withWrites(func() error {
			if mode == "Swift/ObjC producer" {
				return b.addEdgeWithCustody(&primary, rebuildEdgeCustody{proof: rebuildProofSwiftObjC})
			}
			custody := rebuildEdgeCustody{raw: pendingEdgeKey(original)}
			if err := b.addEdgeWithCustody(&primary, custody); err != nil {
				return err
			}
			if mode == "shared receipt" {
				return b.addEdgeWithCustody(&other, custody)
			}
			return nil
		}))
		before := rebuildFrameworkCustodyRows(t, b)
		stats, err := b.RebuildSpringEvents(t.Context(), func(yield func(ParseFacts, error) bool) {
			for facts, err := range receipts {
				facts.Edges = []*graph.Edge{call}
				if mode != "Swift/ObjC producer" {
					facts.Edges = append(facts.Edges, original)
				}
				if !yield(facts, err) {
					return
				}
			}
		}, RebuildSpringEventRules{
			Listener: func(*graph.Node) string { return "Base" },
			Event:    func(*graph.Edge) string { return "Child" },
			Simple:   func(s string) string { return s },
			Rebind: func(e *graph.Edge, _, id string, matched bool) bool {
				if !matched || e.To == id {
					return false
				}
				e.To = id
				return true
			},
			Fanout: func(*graph.Edge, string, string) *graph.Edge { return nil },
		})
		if mode == "shared receipt" {
			var refused *RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			want := graph.EdgeIdentityFor(call)
			want.To = listener.ID
			require.Equal(t, &RebuildUnknownProvenanceError{Count: 1, First: want}, refused)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
			continue
		}
		require.NoError(t, err)
		require.Equal(t, RebuildSpringEventStats{Stubs: 1, Resolved: 1, Moves: 1}, stats)
		want := *call
		want.To = listener.ID
		live, err := b.pendingEdge(pendingEdgeKey(&want))
		require.NoError(t, err)
		require.Equal(t, &want, live)
	}
}

func TestRebuildSpringEventsArclessImplements(t *testing.T) {
	const file = "Events.java"
	arc := &graph.Edge{From: "pkg.Event", To: "other.Event", Kind: graph.EdgeImplements, FilePath: file, Line: 1}
	call := &graph.Edge{From: "publish", To: "unresolved::old", Kind: graph.EdgeCalls, FilePath: file, Line: 2}
	for _, stamped := range []bool{false, true} {
		b, receipts := seedSyntheticCustody(t, ParseFacts{
			EffectiveLanguage: "java",
			File:              graph.FileMetaRow{FilePath: file},
			Edges:             []*graph.Edge{arc, call},
		})
		key := pendingEdgeKey(arc)
		if stamped {
			encoded, err := pendingGet(b.pending, key, copyValue)
			require.NoError(t, err)
			require.NotEmpty(t, encoded)
			digest := sha256.Sum256(encoded)
			require.NoError(t, b.pending.Set(rebuildImplementsStampKey(key), digest[:], pebble.NoSync))
		}
		before := rebuildFrameworkCustodyRows(t, b)
		stats, err := b.RebuildSpringEvents(t.Context(), receipts, RebuildSpringEventRules{
			Listener: func(*graph.Node) string { return "" },
			Event:    func(*graph.Edge) string { return "Event" },
			Simple:   func(string) string { return "Event" },
			Rebind:   func(*graph.Edge, string, string, bool) bool { return false },
			Fanout:   func(*graph.Edge, string, string) *graph.Edge { return nil },
		})
		require.NoError(t, err)
		require.Zero(t, stats)
		require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		found, err := pendingGet(b.pending, nil, func([]byte) (bool, error) { return true, nil })
		require.NoError(t, err)
		require.False(t, found, "a degenerate arc must not write an empty key")
	}
}

package store_pebble

import (
	"bytes"
	"slices"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestThrowerLocationUsesCanonicalEdge(t *testing.T) {
	first := graph.Edge{From: "thrower", To: "one", Kind: graph.EdgeThrows, FilePath: "a.go", Line: 3}
	second := graph.Edge{From: "thrower", To: "two", Kind: graph.EdgeThrows, FilePath: "b.go", Line: 3}
	a, b := edgeIdentity(&first), edgeIdentity(&second)
	if bytes.Compare(a[:], b[:]) > 0 {
		first, second = second, first
	}
	late := graph.Edge{From: "thrower", To: "one", Kind: graph.EdgeThrows, FilePath: "c.go", Line: 10}
	for _, order := range [][]graph.Edge{{late, second, first}, {first, second, late}} {
		store := newBuild(t, FullProfile)
		for _, e := range order {
			store.AddEdge(&e)
		}
		if err := store.Publish(t.Context()); err != nil {
			t.Fatal(err)
		}
		view := snapshot(t, store)
		rows := view.ThrowerErrorSurface("")
		if len(rows) != 1 || rows[0].FilePath != first.FilePath || rows[0].Line != first.Line || rows[0].Throws != 3 || !slices.Equal(rows[0].ErrorTargets, []string{"one", "two"}) {
			t.Fatalf("thrower surface = %+v, want canonical location %s:%d plus all throws", rows, first.FilePath, first.Line)
		}
		if filtered := view.ThrowerErrorSurface("c.go"); len(filtered) != 1 || filtered[0].Throws != 1 || filtered[0].Line != 10 {
			t.Fatalf("scoped thrower surface = %+v, want the single throw in c.go", filtered)
		}
		if err := view.ReadError(); err != nil {
			t.Fatal(err)
		}
	}
}

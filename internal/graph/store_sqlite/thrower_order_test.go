package store_sqlite_test

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The same graph returns a different representative when only insertion order
// changes. This is the FR-20 / D27 exception for Pebble's canonical location.
func TestSQLiteThrowerLocationDependsOnArrivalOrder(t *testing.T) {
	early := graph.Edge{From: "thrower", To: "first", Kind: graph.EdgeThrows, FilePath: "early.go", Line: 3}
	late := graph.Edge{From: "thrower", To: "second", Kind: graph.EdgeThrows, FilePath: "late.go", Line: 10}
	for _, tc := range []struct {
		name          string
		first, second graph.Edge
	}{
		{"early_first", early, late}, {"late_first", late, early},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := store_sqlite.OpenPristineForConformance(t, filepath.Join(t.TempDir(), "graph.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := s.Close(); err != nil {
					t.Error(err)
				}
			}()
			s.AddNode(&graph.Node{ID: "thrower", Kind: graph.KindFunction})
			s.AddEdge(&tc.first)
			s.AddEdge(&tc.second)
			rows := s.ThrowerErrorSurface("")
			if len(rows) != 1 {
				t.Fatalf("got %d throwers, want 1", len(rows))
			}
			row := rows[0]
			if row.FilePath != tc.first.FilePath || row.Line != tc.first.Line || row.Throws != 2 || !slices.Equal(row.ErrorTargets, []string{tc.first.To, tc.second.To}) {
				t.Fatalf("surface = %+v, want the first inserted location %s:%d and both targets", row, tc.first.FilePath, tc.first.Line)
			}
		})
	}
}

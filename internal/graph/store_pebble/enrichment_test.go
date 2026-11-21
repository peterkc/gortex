package store_pebble

import (
	"errors"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestDeferredEnrichmentRefuses(t *testing.T) {
	b := newBuild(t, FullProfile)
	cases := []struct {
		name        string
		set, delete func() error
		read        func(*Snapshot) bool
	}{
		{"Blame",
			func() error { return b.BulkSetBlame("repo", []graph.BlameEnrichment{{}}) },
			func() error { return b.DeleteBlame([]string{"a"}) },
			func(s *Snapshot) bool { return s.BlameRows("repo") == nil },
		},
		{"Churn",
			func() error { return b.BulkSetChurn("repo", []graph.ChurnEnrichment{{}}) },
			func() error { return b.DeleteChurn([]string{"a"}) },
			func(s *Snapshot) bool { return s.ChurnRows("repo") == nil },
		},
		{"Coverage",
			func() error { return b.BulkSetCoverage("repo", []graph.CoverageEnrichment{{}}) },
			func() error { return b.DeleteCoverage([]string{"a"}) },
			func(s *Snapshot) bool { return s.CoverageRows("repo") == nil },
		},
		{"Release",
			func() error { return b.BulkSetReleases("repo", []graph.ReleaseEnrichment{{}}) },
			func() error { return b.DeleteReleases([]string{"a"}) },
			func(s *Snapshot) bool { return s.ReleaseRows("repo") == nil },
		},
	}
	seedBuild(b)
	if err := b.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.set(); !errors.Is(err, errors.ErrUnsupported) {
				t.Fatalf("set = %v", err)
			}
			if err := tc.delete(); !errors.Is(err, errors.ErrUnsupported) {
				t.Fatalf("delete = %v", err)
			}
			s := snapshot(t, b)
			if nilRows := tc.read(s); !nilRows || !errors.Is(s.ReadError(), errors.ErrUnsupported) {
				t.Fatalf("deferred read had rows or lost its typed error: nil rows=%t, error=%v", nilRows, s.ReadError())
			}
		})
	}
	if err := b.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s := snapshot(t, b); s.NodeCount() != 4 || s.EdgeCount() != 6 || s.ReadError() != nil {
		t.Fatalf("enrichment refusal disturbed published graph: %v", s.ReadError())
	}
}

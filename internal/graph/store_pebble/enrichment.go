package store_pebble

import (
	"errors"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
)

// D64 defers the enrichment family. Value-only readers must retain the
// refusal for the request owner instead of reporting a successful empty read.
func enrichmentUnsupported(family string) error {
	return fmt.Errorf("Pebble %s enrichment: %w", family, errors.ErrUnsupported)
}

var (
	_ graph.BlameEnrichmentWriter = (*BuildStore)(nil)
	_ graph.BlameEnrichmentReader = (*Snapshot)(nil)
)

func (b *BuildStore) BulkSetBlame(string, []graph.BlameEnrichment) error {
	return enrichmentUnsupported("blame")
}

func (b *BuildStore) DeleteBlame([]string) error {
	return enrichmentUnsupported("blame")
}

func (s *Snapshot) BlameRows(string) []graph.BlameEnrichment {
	s.retain(enrichmentUnsupported("blame"))
	return nil
}

var (
	_ graph.ChurnEnrichmentWriter = (*BuildStore)(nil)
	_ graph.ChurnEnrichmentReader = (*Snapshot)(nil)
)

func (b *BuildStore) BulkSetChurn(string, []graph.ChurnEnrichment) error {
	return enrichmentUnsupported("churn")
}

func (b *BuildStore) DeleteChurn([]string) error {
	return enrichmentUnsupported("churn")
}

func (s *Snapshot) ChurnRows(string) []graph.ChurnEnrichment {
	s.retain(enrichmentUnsupported("churn"))
	return nil
}

var (
	_ graph.CoverageEnrichmentWriter = (*BuildStore)(nil)
	_ graph.CoverageEnrichmentReader = (*Snapshot)(nil)
)

func (b *BuildStore) BulkSetCoverage(string, []graph.CoverageEnrichment) error {
	return enrichmentUnsupported("coverage")
}

func (b *BuildStore) DeleteCoverage([]string) error {
	return enrichmentUnsupported("coverage")
}

func (s *Snapshot) CoverageRows(string) []graph.CoverageEnrichment {
	s.retain(enrichmentUnsupported("coverage"))
	return nil
}

var (
	_ graph.ReleaseEnrichmentWriter = (*BuildStore)(nil)
	_ graph.ReleaseEnrichmentReader = (*Snapshot)(nil)
)

func (b *BuildStore) BulkSetReleases(string, []graph.ReleaseEnrichment) error {
	return enrichmentUnsupported("release")
}

func (b *BuildStore) DeleteReleases([]string) error {
	return enrichmentUnsupported("release")
}

func (s *Snapshot) ReleaseRows(string) []graph.ReleaseEnrichment {
	s.retain(enrichmentUnsupported("release"))
	return nil
}

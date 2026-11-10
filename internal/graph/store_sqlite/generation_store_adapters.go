package store_sqlite

import (
	"context"
	"github.com/zzet/gortex/internal/graph"
)

// AtGenerationStore returns AtGeneration's borrowed handle as a graph.Store.
func (s *Store) AtGenerationStore(generationID int64) graph.Store {
	handle := s.AtGeneration(generationID)
	if handle == nil {
		return nil
	}
	return handle
}

// IsNilGenerationHandle reports pointer absence, not core liveness.
func (s *Store) IsNilGenerationHandle() bool { return s == nil }

// ViewGenerationRecord reads the catalog paired with this exact native handle.
func (s *Store) ViewGenerationRecord(ctx context.Context, generation int64) (ViewGeneration, bool, error) {
	return s.Catalog().GetViewGeneration(ctx, generation)
}

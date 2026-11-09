package store_sqlite

import "github.com/zzet/gortex/internal/graph"

// AtGenerationStore returns AtGeneration's borrowed handle as a graph.Store.
func (s *Store) AtGenerationStore(generationID int64) graph.Store {
	handle := s.AtGeneration(generationID)
	if handle == nil {
		return nil
	}
	return handle
}

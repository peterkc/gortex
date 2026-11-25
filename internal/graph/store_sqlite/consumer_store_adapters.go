package store_sqlite

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

// ViewScopedAnalysisStore returns the native view-scoped analysis handle.
func (s *Store) ViewScopedAnalysisStore() graph.Store {
	if s == nil {
		return nil
	}
	return s.ViewScopedAnalysis()
}

// TokenizeSymbolFTS forwards the native tokenizer without changing its policy.
func (s *Store) TokenizeSymbolFTS(ctx context.Context, texts []string) ([][]string, error) {
	return SymbolFTSTokenize(ctx, texts)
}

// GetCheckoutRoute reads the catalog paired with this exact native handle.
func (s *Store) GetCheckoutRoute(ctx context.Context, checkoutID string) (CheckoutRoute, bool, error) {
	return s.Catalog().GetCheckoutRoute(ctx, checkoutID)
}

// WithdrawProducer updates the catalog paired with this exact native handle.
func (s *Store) WithdrawProducer(ctx context.Context, generationID int64, producer, reason string) error {
	return s.Catalog().WithdrawProducer(ctx, generationID, producer, reason)
}

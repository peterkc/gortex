package store_sqlite

import "context"

// GetDedicatedGraph reads the control-plane record paired with this store.
func (s *Store) GetDedicatedGraph(ctx context.Context, graphID string) (DedicatedGraph, bool, error) {
	return s.Catalog().GetDedicatedGraph(ctx, graphID)
}

// DedicatedBasePublication reads the publication paired with this store.
func (s *Store) DedicatedBasePublication(ctx context.Context, graphID string) (DedicatedBasePublication, bool, error) {
	return s.Catalog().DedicatedBasePublication(ctx, graphID)
}

// GetCheckout reads the checkout record paired with this store.
func (s *Store) GetCheckout(ctx context.Context, checkoutID string) (Checkout, bool, error) {
	return s.Catalog().GetCheckout(ctx, checkoutID)
}

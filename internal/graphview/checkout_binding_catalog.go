package graphview

import (
	"context"

	"github.com/zzet/gortex/internal/storage"
)

type checkoutBindingCatalog interface {
	IsNilMaterializerCatalog() bool
	CheckoutsByRootPaths(context.Context, []string) ([]storage.Checkout, error)
	ListRepositoryFamilies(context.Context) ([]storage.RepositoryFamily, error)
	ListCheckouts(context.Context, string) ([]storage.Checkout, error)
}

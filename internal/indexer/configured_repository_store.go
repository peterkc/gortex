package indexer

import (
	"context"

	"github.com/zzet/gortex/internal/storage"
)

type configuredRepositoryStore interface {
	GetDedicatedGraph(context.Context, string) (storage.DedicatedGraph, bool, error)
	DedicatedBasePublication(context.Context, string) (storage.DedicatedBasePublication, bool, error)
	GetCheckout(context.Context, string) (storage.Checkout, bool, error)
}

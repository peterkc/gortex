package main

import (
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
)

func slice2LifecycleConfig(cfg indexer.CheckoutLifecycleConfig) indexer.CheckoutLifecycleConfig {
	if provider, ok := cfg.Graph.(interface{ Catalog() *store_sqlite.Catalog }); ok {
		cfg.Catalog = provider.Catalog()
	}
	return cfg
}

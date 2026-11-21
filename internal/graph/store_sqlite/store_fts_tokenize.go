package store_sqlite

import (
	"context"

	"github.com/zzet/gortex/internal/search/fts5token"
)

// SymbolFTSTokenize returns the terms that FTS5 indexes for each symbol document.
func SymbolFTSTokenize(ctx context.Context, texts []string) ([][]string, error) {
	return fts5token.SymbolFTSTokenize(ctx, texts)
}

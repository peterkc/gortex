package storage

// SymbolFTSStats are the corpus statistics bm25 reads: the number of
// documents, the total number of tokens, and a stamp naming the state they
// were read from.
type SymbolFTSStats struct {
	Rows   int64
	Tokens int64
	Stamp  string
	// Lo and Hi are a generation's first and last document rowid
	// (SymbolFTSGenerationStats only; 0 when it has none).
	Lo, Hi int64
}

// SymbolFTSRow is one document of a generation: its node, repository, token
// text and token count (FTS5's own per-document size).
type SymbolFTSRow struct {
	RowID      int64
	NodeID     string
	RepoPrefix string
	Tokens     string
	TokenCount int
}

package base

import (
	"context"
	"errors"
	"fmt"
	"github.com/blevesearch/bleve/v2"
	_ "github.com/blevesearch/bleve/v2/analysis/analyzer/custom"
	_ "github.com/blevesearch/bleve/v2/analysis/token/lowercase"
	_ "github.com/blevesearch/bleve/v2/analysis/tokenizer/regexp"
	"github.com/blevesearch/bleve/v2/search/query"
	index "github.com/blevesearch/bleve_index_api"
	"gxstudy/adapter"
)

// BuildText creates the shared study Bleve mapping. The producer calls add
// synchronously and returns its errors; BuildText owns and closes the index.
func BuildText(ctx context.Context, path string, rows func(func(string, string) error) error) (err error) {
	m := bleve.NewIndexMapping()
	m.ScoringModel = "bm25"
	if err = m.AddCustomTokenizer("gxwords", map[string]interface{}{"type": "regexp", "regexp": `[\p{L}\p{N}]+`}); err != nil {
		return err
	}
	if err = m.AddCustomAnalyzer("gxtext", map[string]interface{}{"type": "custom", "tokenizer": "gxwords", "token_filters": []string{"to_lower"}}); err != nil {
		return err
	}
	m.DefaultAnalyzer = "gxtext"
	m.DefaultField = "text"
	dm := bleve.NewDocumentStaticMapping()
	fm := bleve.NewTextFieldMapping()
	fm.Analyzer = "gxtext"
	fm.Store = false
	fm.IncludeInAll = false
	fm.IncludeTermVectors = false
	dm.AddFieldMappingsAt("text", fm)
	m.DefaultMapping = dm
	ix, err := bleve.NewUsing(path, m, "scorch", bleve.Config.DefaultKVStore, map[string]interface{}{"unsafe_batch": false})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, ix.Close()) }()
	batch := ix.NewBatch()
	err = rows(func(id, text string) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		if e := batch.Index(id, map[string]string{"text": text}); e != nil {
			return e
		}
		if batch.Size() >= 1000 {
			if e := ix.Batch(batch); e != nil {
				return e
			}
			batch = ix.NewBatch()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if batch.Size() > 0 {
		return ix.Batch(batch)
	}
	return nil
}

// SearchText applies the shared native BM25 policy. Zero k returns no hits;
// negative k returns all matches. The caller retains ownership of the index.
func SearchText(ctx context.Context, ix bleve.Index, text string, k int) ([]adapter.Hit, error) {
	if k == 0 {
		return nil, nil
	}
	terms := ix.Mapping().AnalyzerNamed("gxtext").Analyze([]byte(text))
	seen := map[string]bool{}
	var queries []query.Query
	for _, t := range terms {
		term := string(t.Term)
		if seen[term] {
			continue
		}
		seen[term] = true
		q := bleve.NewTermQuery(term)
		q.SetField("text")
		queries = append(queries, q)
	}
	if len(queries) == 0 {
		return nil, nil
	}
	if k < 0 {
		n, e := ix.DocCount()
		if e != nil {
			return nil, e
		}
		k = int(n)
	}
	req := bleve.NewSearchRequestOptions(bleve.NewDisjunctionQuery(queries...), k, 0, false)
	req.SortBy([]string{"-_score", "_id"})
	res, e := ix.SearchInContext(ctx, req)
	if e != nil {
		return nil, e
	}
	if res.Status.Failed > 0 {
		return nil, fmt.Errorf("Bleve search partial failure: %v", res.Status.Errors)
	}
	hits := make([]adapter.Hit, 0, len(res.Hits))
	for _, h := range res.Hits {
		hits = append(hits, adapter.Hit{ID: h.ID, Score: h.Score})
	}
	return hits, nil
}
func (s *Segment) SearchSymbols(ctx context.Context, q string, k int) ([]adapter.Hit, error) {
	return SearchText(ctx, s.symbols, q, k)
}
func (s *Segment) SearchContent(ctx context.Context, q string, k int) ([]adapter.Hit, error) {
	return SearchText(ctx, s.content, q, k)
}

// TextIndex returns a borrowed read-only index for a Bleve alias. It is valid
// only while this Segment remains open. Close on the borrowed index is a no-op;
// only Segment.Close owns the real handle. Mutation and Advanced are refused.
func (s *Segment) TextIndex(name string) (bleve.Index, error) {
	switch name {
	case "symbols":
		return readOnlyText{s.symbols}, nil
	case "content":
		return readOnlyText{s.content}, nil
	default:
		return nil, fmt.Errorf("unsupported text index %q: %w", name, adapter.ErrUnsupported)
	}
}

type textReader = bleve.Index
type readOnlyText struct{ textReader }

func (readOnlyText) Index(id string, value interface{}) error { return adapter.ErrUnsupported }
func (readOnlyText) Delete(id string) error                   { return adapter.ErrUnsupported }
func (readOnlyText) Batch(batch *bleve.Batch) error           { return adapter.ErrUnsupported }
func (readOnlyText) SetInternal(key, value []byte) error      { return adapter.ErrUnsupported }
func (readOnlyText) DeleteInternal(key []byte) error          { return adapter.ErrUnsupported }
func (readOnlyText) Advanced() (index.Index, error)           { return nil, adapter.ErrUnsupported }
func (readOnlyText) Close() error                             { return nil }
func (readOnlyText) SetName(name string)                      {}

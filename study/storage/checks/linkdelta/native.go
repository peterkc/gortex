package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"path/filepath"
	"slices"

	"github.com/blevesearch/bleve/v2"
	_ "github.com/blevesearch/bleve/v2/analysis/analyzer/custom"
	_ "github.com/blevesearch/bleve/v2/analysis/token/lowercase"
	_ "github.com/blevesearch/bleve/v2/analysis/tokenizer/regexp"
	"github.com/blevesearch/bleve/v2/search/query"
)

func buildText(ctx context.Context, path string, rows func(func(string, string) error) error) (err error) {
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

func buildDeltaText(ctx context.Context, dir string, rows []row) error {
	for _, kind := range []string{"symbol", "content"} {
		name := "symbols"
		if kind == "content" {
			name = "content"
		}
		err := buildText(ctx, filepath.Join(dir, name+".bleve"), func(add func(string, string) error) error {
			for _, r := range rows {
				if r.Family != kind {
					continue
				}
				if kind == "symbol" {
					var v dataset.Symbol
					if e := json.Unmarshal(r.Value, &v); e != nil {
						return e
					}
					if e := add(v.NodeID, v.Tokens); e != nil {
						return e
					}
				} else {
					var v dataset.Content
					if e := json.Unmarshal(r.Value, &v); e != nil {
						return e
					}
					if e := add(v.ChunkID, v.Body); e != nil {
						return e
					}
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// A member excludes only its superseded documents. An alias-wide exclusion
// would incorrectly hide replacement IDs in the newer member too.
type maskedIndex struct {
	bleve.Index
	excluded []string
}

func (m maskedIndex) SearchInContext(ctx context.Context, req *bleve.SearchRequest) (*bleve.SearchResult, error) {
	if len(m.excluded) == 0 {
		return m.Index.SearchInContext(ctx, req)
	}
	copy := *req
	filter := bleve.NewBooleanQuery()
	filter.AddMust(req.Query)
	filter.AddMustNot(bleve.NewDocIDQuery(m.excluded))
	copy.Query = filter
	return m.Index.SearchInContext(ctx, &copy)
}
func (m maskedIndex) Search(req *bleve.SearchRequest) (*bleve.SearchResult, error) {
	return m.SearchInContext(context.Background(), req)
}
func (s *segment) configureText(ctx context.Context) error {
	base := s.external.(baseResources)
	for _, kind := range []string{"symbol", "content"} {
		name := "symbols"
		if kind == "content" {
			name = "content"
		}
		index, err := base.TextIndex(name)
		if err != nil {
			return err
		}
		var excluded []string
		if err = s.scanExternal(ctx, kind, "", "", func(r row) error {
			if s.masked(r.Owner, r.Source, -1) {
				excluded = append(excluded, r.Key)
			}
			return nil
		}); err != nil {
			return err
		}
		slices.Sort(excluded)
		excluded = slices.Compact(excluded)
		members := []bleve.Index{maskedIndex{Index: index, excluded: excluded}}
		for i, l := range s.layers {
			ix := l.symbols
			if kind == "content" {
				ix = l.content
			}
			var masks []string
			for _, r := range l.rows {
				if r.Family == kind && s.masked(r.Owner, r.Source, i) {
					masks = append(masks, r.Key)
				}
			}
			slices.Sort(masks)
			masks = slices.Compact(masks)
			members = append(members, maskedIndex{Index: ix, excluded: masks})
		}
		alias := bleve.NewIndexAlias(members...)
		if err := alias.SetIndexMapping(index.Mapping()); err != nil {
			return err
		}
		if kind == "symbol" {
			s.symbols = alias
		} else {
			s.content = alias
		}
	}
	return nil
}
func nativeSearch(ctx context.Context, ix bleve.Index, text string, k int) ([]adapter.Hit, error) {
	if k == 0 {
		return nil, nil
	}
	terms := ix.Mapping().AnalyzerNamed("gxtext").Analyze([]byte(text))
	seen := map[string]bool{}
	var qs []query.Query
	for _, t := range terms {
		term := string(t.Term)
		if seen[term] {
			continue
		}
		seen[term] = true
		q := bleve.NewTermQuery(term)
		q.SetField("text")
		qs = append(qs, q)
	}
	if len(qs) == 0 {
		return nil, nil
	}
	if k < 0 {
		n, e := ix.DocCount()
		if e != nil {
			return nil, e
		}
		k = int(n)
	}
	req := bleve.NewSearchRequestOptions(bleve.NewDisjunctionQuery(qs...), k, 0, false)
	req.SortBy([]string{"-_score", "_id"})
	res, err := ix.SearchInContext(ctx, req)
	if err != nil {
		return nil, err
	}
	if res.Status.Failed > 0 {
		return nil, fmt.Errorf("Bleve alias partial search: %v", res.Status.Errors)
	}
	hits := make([]adapter.Hit, 0, len(res.Hits))
	for _, h := range res.Hits {
		hits = append(hits, adapter.Hit{ID: h.ID, Score: h.Score})
	}
	return hits, nil
}

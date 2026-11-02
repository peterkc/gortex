package main

import (
	"context"
	"encoding/json"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"path/filepath"
	"slices"
	cbase "study/checks/base"

	"github.com/blevesearch/bleve/v2"
)

func buildDeltaText(ctx context.Context, dir string, rows []row) error {
	for _, kind := range []string{"symbol", "content"} {
		name := "symbols"
		if kind == "content" {
			name = "content"
		}
		err := cbase.BuildText(ctx, filepath.Join(dir, name+".bleve"), func(add func(string, string) error) error {
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
type textIndex = bleve.Index

type maskedIndex struct {
	textIndex
	excluded []string
}

var _ bleve.Index = maskedIndex{}

func (m maskedIndex) SearchInContext(ctx context.Context, req *bleve.SearchRequest) (*bleve.SearchResult, error) {
	if len(m.excluded) == 0 {
		return m.textIndex.SearchInContext(ctx, req)
	}
	filtered := *req
	filter := bleve.NewBooleanQuery()
	filter.AddMust(req.Query)
	filter.AddMustNot(bleve.NewDocIDQuery(m.excluded))
	filtered.Query = filter
	return m.textIndex.SearchInContext(ctx, &filtered)
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
		masks := s.nodeMask
		if kind == "content" {
			masks = s.chunkMask
		}
		for id := range masks {
			excluded = append(excluded, id)
		}
		slices.Sort(excluded)
		excluded = slices.Compact(excluded)
		members := []bleve.Index{maskedIndex{textIndex: index, excluded: excluded}}
		for i, l := range s.layers {
			ix := l.symbols
			if kind == "content" {
				ix = l.content
			}
			var masks []string
			for _, r := range l.text[kind] {
				if s.masked(r.File, r.Node, i) || (kind == "content" && s.chunkMask[r.ID] > i+1) {
					masks = append(masks, r.ID)
				}
			}
			slices.Sort(masks)
			masks = slices.Compact(masks)
			members = append(members, maskedIndex{textIndex: ix, excluded: masks})
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
	return cbase.SearchText(ctx, ix, text, k)
}

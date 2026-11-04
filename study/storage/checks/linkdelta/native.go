package main

import (
	"context"
	"encoding/json"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"math"
	"path/filepath"
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

// A view borrows the base index and owns only the current combined D.
func (s *segment) configureText(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	base := s.external.(baseResources)
	for i, name := range []string{"symbols", "content"} {
		ix, err := base.TextIndex(name)
		if err != nil {
			return err
		}
		indexes := [2]bleve.Index{ix, s.openedText[i]}
		if i == 0 {
			s.symbols = indexes
		} else {
			s.content = indexes
		}
	}
	return nil
}
func (s *segment) searchText(ctx context.Context, kind, text string, k int) ([]adapter.Hit, error) {
	if k == 0 {
		return nil, nil
	}
	indexes := s.symbols
	family := "node"
	if kind == "content" {
		indexes = s.content
		family = "content"
	}
	baseK := k
	if k > 0 {
		baseK = overfetch(k, s.hiddenTextCount(kind))
	}
	base, err := cbase.SearchText(ctx, indexes[0], text, baseK)
	if err != nil {
		return nil, err
	}
	hits := make([]adapter.Hit, 0, len(base))
	for _, h := range base {
		if !s.baseRowHidden(family, h.ID) {
			hits = append(hits, h)
		}
	}
	delta, err := cbase.SearchText(ctx, indexes[1], text, k)
	if err != nil {
		return nil, err
	}
	// Native per-index BM25 order remains diagnostic against a clean rebuild.
	return adapter.Top(append(hits, delta...), k), nil
}

func overfetch(k, hidden int) int {
	if k > 0 && k > math.MaxInt-hidden {
		return -1
	}
	if k > 0 {
		return k + hidden
	}
	return k
}

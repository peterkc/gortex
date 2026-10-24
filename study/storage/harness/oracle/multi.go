package oracle

import (
	"context"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/dataset"
)

// GlobalMulti scores BM25 over all repositories with one corpus-wide IDF and
// average length. Fanout remains available as a separately measured emulation.
type GlobalMulti struct {
	adapter.Fanout
	symbols textIndex
}

func NewMulti(ds []dataset.Dataset, segments map[string]adapter.Segment) *GlobalMulti {
	var ids, bodies []string
	for _, d := range ds {
		for _, s := range d.Symbols {
			ids = append(ids, s.NodeID)
			bodies = append(bodies, s.Tokens)
		}
	}
	return &GlobalMulti{Fanout: adapter.Fanout{Segments: segments}, symbols: newText(ids, bodies)}
}
func (m *GlobalMulti) SearchSymbols(ctx context.Context, q string, k int) ([]adapter.Hit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return m.symbols.search(q, k), nil
}
func (Backend) OpenMulti(ctx context.Context, segments map[string]adapter.Segment) (adapter.MultiRepo, error) {
	var ds []dataset.Dataset
	for _, s := range segments {
		v, ok := s.(*Segment)
		if !ok {
			return nil, fmt.Errorf("oracle requires oracle segments")
		}
		ds = append(ds, v.data)
	}
	return NewMulti(ds, segments), ctx.Err()
}

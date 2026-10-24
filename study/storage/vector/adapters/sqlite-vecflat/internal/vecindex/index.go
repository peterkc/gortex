// Package vecindex holds the vector paths shared by the two study adapters and benchmark.
package vecindex

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"slices"

	"github.com/coder/hnsw"
	"gxstudy/adapter"
	"gxstudy/dataset"
)

// Config fixes all parameters before construction. Coder/hnsw uses Ef for both
// construction and search; Candidates is the Search k before exact reranking.
type Config struct {
	M, Ef, Candidates int
	Seed              int64
}

var Default = Config{M: 32, Ef: 256, Candidates: 10, Seed: 20261022}

type Index struct {
	Flat   []dataset.Vector
	norms  []float64
	Graph  *hnsw.Graph[string]
	Config Config
}

func Validate(v []float32) error {
	if len(v) != dataset.Dimensions {
		return fmt.Errorf("vector dimensions: %d", len(v))
	}
	var norm float64
	for _, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return fmt.Errorf("nonfinite vector")
		}
		norm += float64(x) * float64(x)
	}
	if norm == 0 {
		return fmt.Errorf("zero vector")
	}
	return nil
}
func Cosine(a, b []float32) float64 {
	var dot, an, bn float64
	for i, x := range a {
		y := b[i]
		dot += float64(x) * float64(y)
		an += float64(x) * float64(x)
		bn += float64(y) * float64(y)
	}
	return dot / math.Sqrt(an*bn)
}

func Build(ctx context.Context, path, mode string, vectors []dataset.Vector, cfg Config) error {
	seen := make(map[string]bool, len(vectors))
	for _, v := range vectors {
		if v.NodeID == "" || len(v.NodeID) > 1<<20 || seen[v.NodeID] {
			return fmt.Errorf("invalid or duplicate vector ID")
		}
		seen[v.NodeID] = true
		if err := Validate(v.Values); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)
	switch mode {
	case "flat":
		for _, v := range vectors {
			if err = ctx.Err(); err != nil {
				return err
			}
			if err = binary.Write(w, binary.LittleEndian, uint32(len(v.NodeID))); err != nil {
				return err
			}
			if _, err = w.WriteString(v.NodeID); err != nil {
				return err
			}
			if err = binary.Write(w, binary.LittleEndian, v.Values); err != nil {
				return err
			}
		}
	case "hnsw":
		g := hnsw.NewGraph[string]()
		g.M = cfg.M
		g.EfSearch = cfg.Ef
		seed := cfg.Seed
		if seed == 0 {
			seed = 20261022
		}
		g.Rng = rand.New(rand.NewSource(seed))
		for _, v := range vectors {
			if err = ctx.Err(); err != nil {
				return err
			}
			g.Add(hnsw.MakeNode(v.NodeID, v.Values))
		}
		if err = g.Export(w); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown vector mode %q", mode)
	}
	if err = w.Flush(); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	return errors.Join(f.Close(), os.Chmod(path, 0444))
}

func Open(path, mode string, cfg Config) (*Index, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	idx := &Index{Config: cfg}
	switch mode {
	case "hnsw":
		idx.Graph = hnsw.NewGraph[string]()
		if err = idx.Graph.Import(r); err != nil {
			return nil, err
		}
		// Imported construction parameters are immutable during serving.
		idx.Graph.EfSearch = cfg.Ef
	case "flat":
		for {
			var n uint32
			err = binary.Read(r, binary.LittleEndian, &n)
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			if n == 0 || n > 1<<20 {
				return nil, fmt.Errorf("invalid ID length %d", n)
			}
			id := make([]byte, n)
			if _, err = io.ReadFull(r, id); err != nil {
				return nil, err
			}
			v := dataset.Vector{NodeID: string(id), Values: make([]float32, dataset.Dimensions)}
			if err = binary.Read(r, binary.LittleEndian, v.Values); err != nil {
				return nil, err
			}
			if err = Validate(v.Values); err != nil {
				return nil, err
			}
			idx.Flat = append(idx.Flat, v)
			idx.norms = append(idx.norms, normSquared(v.Values))
		}
	default:
		return nil, fmt.Errorf("unknown vector mode %q", mode)
	}
	return idx, nil
}

// Search keeps only k candidates during a flat scan. HNSW scores returned
// candidates with the same float64 cosine before truncating. Neither path caches answers.
func (idx *Index) Search(ctx context.Context, q []float32, k int) ([]adapter.Hit, error) {
	if err := Validate(q); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if k == 0 {
		return nil, nil
	}
	if idx.Graph != nil {
		if idx.Graph.Len() == 0 {
			return nil, nil
		}
		if k < 0 {
			k = idx.Graph.Len()
		}
		candidates := max(k, idx.Config.Candidates)
		nodes := idx.Graph.Search(q, candidates)
		hits := make([]adapter.Hit, 0, len(nodes))
		for _, n := range nodes {
			hits = append(hits, adapter.Hit{ID: n.Key, Score: Cosine(q, n.Value)})
		}
		return adapter.Top(hits, k), nil
	}
	var hits []adapter.Hit
	qnorm := normSquared(q)
	for i, v := range idx.Flat {
		if i%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		var dot float64
		for j, x := range q {
			dot += float64(x) * float64(v.Values[j])
		}
		h := adapter.Hit{ID: v.NodeID, Score: dot / math.Sqrt(qnorm*idx.norms[i])}
		if k < 0 || len(hits) < k {
			hits = append(hits, h)
			slices.SortFunc(hits, compare)
			continue
		}
		if compare(h, hits[len(hits)-1]) < 0 {
			hits[len(hits)-1] = h
			slices.SortFunc(hits, compare)
		}
	}
	return hits, nil
}
func compare(a, b adapter.Hit) int {
	if a.Score > b.Score {
		return -1
	}
	if a.Score < b.Score {
		return 1
	}
	if a.ID < b.ID {
		return -1
	}
	if a.ID > b.ID {
		return 1
	}
	return 0
}

func normSquared(v []float32) float64 {
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	return n
}

func (idx *Index) Len() int {
	if idx.Graph != nil {
		return idx.Graph.Len()
	}
	return len(idx.Flat)
}
func (idx *Index) Close() error { return nil }

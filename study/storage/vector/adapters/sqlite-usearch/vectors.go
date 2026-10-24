package main

import (
	"context"
	"errors"
	"fmt"
	usearch "github.com/unum-cloud/usearch/golang"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"os"
)

type NativeVectors struct {
	Index *usearch.Index
	IDs   []string
}

func nativeConfig() usearch.IndexConfig {
	c := usearch.DefaultConfig(384)
	c.Quantization = usearch.F32
	c.Connectivity = 16
	c.ExpansionAdd = 128
	c.ExpansionSearch = 128
	return c
}
func buildNative(ctx context.Context, path string, vectors []dataset.Vector) error {
	idx, err := usearch.NewIndex(nativeConfig())
	if err != nil {
		return err
	}
	defer idx.Destroy()
	if err = idx.Reserve(uint(len(vectors))); err != nil {
		return err
	}
	if err = idx.ChangeThreadsAdd(1); err != nil {
		return err
	}
	for i, v := range vectors {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = idx.Add(uint64(i), v.Values); err != nil {
			return err
		}
	}
	if err = idx.Save(path); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	err = errors.Join(f.Sync(), f.Close())
	if err != nil {
		return err
	}
	return os.Chmod(path, 0444)
}
func openNative(path string) (*NativeVectors, error) {
	idx, err := usearch.NewIndex(nativeConfig())
	if err != nil {
		return nil, err
	}
	if vectorOpenMode == "view" {
		err = idx.View(path)
	} else {
		err = idx.Load(path)
	}
	if err != nil {
		idx.Destroy()
		return nil, err
	}
	if err = idx.ChangeThreadsSearch(8); err != nil {
		idx.Destroy()
		return nil, err
	}
	return &NativeVectors{Index: idx}, nil
}
func (v *NativeVectors) Close() error { return v.Index.Destroy() }
func (v *NativeVectors) Search(ctx context.Context, q []float32, k int) ([]adapter.Hit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateVector(q); err != nil {
		return nil, err
	}
	if k == 0 {
		return nil, nil
	}
	if k < 0 {
		k = len(v.IDs)
	}
	keys, dist, err := v.Index.Search(q, uint(k))
	if err != nil {
		return nil, err
	}
	hits := make([]adapter.Hit, 0, len(keys))
	for i, key := range keys {
		if key >= uint64(len(v.IDs)) {
			return nil, fmt.Errorf("native key outside ID mapping")
		}
		hits = append(hits, adapter.Hit{ID: v.IDs[key], Score: 1 - float64(dist[i])})
	}
	return adapter.Top(hits, k), nil
}

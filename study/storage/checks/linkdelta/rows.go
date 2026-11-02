package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"os"
	"path/filepath"
	"slices"
)

// row is a shared physical envelope; Value retains every source-record field.
type row struct {
	Family string `parquet:"family"`
	Key    string `parquet:"key"`
	Owner  string `parquet:"owner"`
	Source string `parquet:"source"`
	Target string `parquet:"target"`
	Name   string `parquet:"name"`
	Value  []byte `parquet:"value"`
}

func dataRows(ctx context.Context, d dataset.Dataset, add func(row) error) error {
	put := func(r row, v any) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		r.Value = b
		return add(r)
	}
	for n, err := range d.NodeRows() {
		if err != nil {
			return err
		}
		if err = put(row{Family: "node", Key: n.ID, Owner: n.FilePath, Source: n.ID, Name: n.Name}, n); err != nil {
			return err
		}
	}
	seq := 0
	for e, err := range d.EdgeRows() {
		if err != nil {
			return err
		}
		if err = put(row{Family: "edge", Key: fmt.Sprint(seq), Owner: e.FilePath, Source: e.From, Target: e.To}, e); err != nil {
			return err
		}
		seq++
	}
	for f, err := range d.FileRows() {
		if err != nil {
			return err
		}
		if err = put(row{Family: "file", Key: f.Path, Owner: f.Path}, f); err != nil {
			return err
		}
	}
	for s, err := range d.SymbolRows() {
		if err != nil {
			return err
		}
		if err = put(row{Family: "symbol", Key: s.NodeID, Source: s.NodeID}, s); err != nil {
			return err
		}
	}
	for c, err := range d.ContentRows() {
		if err != nil {
			return err
		}
		if err = put(row{Family: "content", Key: c.ChunkID, Owner: c.FilePath, Source: c.NodeID}, c); err != nil {
			return err
		}
	}
	for v, err := range d.VectorRows() {
		if err != nil {
			return err
		}
		if err = put(row{Family: "vector", Key: v.NodeID, Source: v.NodeID}, v); err != nil {
			return err
		}
	}
	return nil
}
func changeRows(ctx context.Context, s *segment, c adapter.FileChange) ([]row, error) {
	if c.Path == "" || c.Replacement.Repo != s.ref.Repo {
		return nil, fmt.Errorf("file change identity mismatch")
	}
	old, err := s.FileNodes(ctx, c.Path)
	if err != nil {
		return nil, err
	}
	rows := []row{{Family: "mask_file", Key: c.Path}}
	oldIDs := map[string]bool{}
	for _, n := range old {
		oldIDs[n.ID] = true
		rows = append(rows, row{Family: "mask_node", Key: n.ID})
	}
	chunks := map[string]bool{}
	for _, id := range s.owners.ByFile[c.Path] {
		chunks[id] = true
	}
	for _, n := range old {
		for _, id := range s.owners.ByNode[n.ID] {
			chunks[id] = true
		}
	}
	for _, l := range s.layers {
		for _, v := range l.text["content"] {
			if v.File == c.Path || oldIDs[v.Node] {
				chunks[v.ID] = true
			}
		}
	}
	ids := make([]string, 0, len(chunks))
	for id := range chunks {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		rows = append(rows, row{Family: "mask_chunk", Key: id})
	}
	seen := map[string]bool{}
	err = dataRows(ctx, c.Replacement, func(r row) error {
		if r.Family == "node" {
			if r.Key == "" || seen[r.Key] {
				return fmt.Errorf("empty/duplicate replacement node ID")
			}
			seen[r.Key] = true
		}
		if (r.Family == "node" || r.Family == "file" || r.Family == "content") && r.Owner != c.Path {
			return fmt.Errorf("replacement escaped owner %q", c.Path)
		}
		rows = append(rows, r)
		return nil
	})
	return rows, err
}
func syncDir(path string) (err error) {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
func durableJSON(path string, v any) (err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	if err = json.NewEncoder(f).Encode(v); err != nil {
		return err
	}
	return f.Sync()
}
func checkPath(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return err
	}
	if real != abs {
		return fmt.Errorf("payload alias/symlink refused")
	}
	return nil
}

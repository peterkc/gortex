package main

import (
	"context"
	"encoding/json"
	"errors"
	"gxstudy/dataset"
	"os"
	"path/filepath"
)

// Row numbers belong to one immutable row space. Keys resolve an indexed result
// to that space; duplicate edge-owner keys always have the same visibility.
type rowOwner struct {
	Family, Key, File, Node, Name string
}
type rowSpace struct {
	rows  []rowOwner
	byKey map[string]int
}
type rowBitmap []uint64

func (b rowBitmap) has(row int) bool { return b[row/64]&(uint64(1)<<uint(row%64)) != 0 }
func (b rowBitmap) set(row int)      { b[row/64] |= uint64(1) << uint(row%64) }
func edgeKey(e dataset.Edge) string  { return lookup(e.FilePath, e.From) }
func ownerOf(r row) rowOwner {
	key := r.Key
	if r.Family == "edge" {
		key = lookup(r.Owner, r.Source)
	}
	return rowOwner{Family: r.Family, Key: key, File: r.Owner, Node: r.Source, Name: r.Name}
}
func rowSpaces(rows []rowOwner) map[string]rowSpace {
	spaces := map[string]rowSpace{}
	for _, r := range rows {
		if r.Family == "symbol" {
			continue
		}
		s := spaces[r.Family]
		if s.byKey == nil {
			s.byKey = map[string]int{}
		}
		s.byKey[r.Key] = len(s.rows)
		s.rows = append(s.rows, r)
		spaces[r.Family] = s
	}
	return spaces
}
func writeOwners(ctx context.Context, path string, d dataset.Dataset) (err error) {
	f, err := os.OpenFile(filepath.Join(path, "rows.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	enc := json.NewEncoder(f)
	put := func(r rowOwner) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		return enc.Encode(r)
	}
	for n, e := range d.NodeRows() {
		if e != nil {
			return e
		}
		if e = put(rowOwner{Family: "node", Key: n.ID, File: n.FilePath, Node: n.ID, Name: n.Name}); e != nil {
			return e
		}
	}
	for e, err := range d.EdgeRows() {
		if err != nil {
			return err
		}
		if err = put(rowOwner{Family: "edge", Key: edgeKey(e), File: e.FilePath, Node: e.From}); err != nil {
			return err
		}
	}
	for s, e := range d.SymbolRows() {
		if e != nil {
			return e
		}
		if e = put(rowOwner{Family: "symbol", Key: s.NodeID, Node: s.NodeID}); e != nil {
			return e
		}
	}
	chunkParents := map[string]chunkOwner{}
	for c, e := range d.ContentRows() {
		if e != nil {
			return e
		}
		chunkParents[c.ChunkID] = chunkOwner{ID: c.ChunkID, Node: c.NodeID, File: c.FilePath}
		if e = put(rowOwner{Family: "content", Key: c.ChunkID, File: c.FilePath, Node: c.NodeID}); e != nil {
			return e
		}
	}
	for v, e := range d.VectorRows() {
		if e != nil {
			return e
		}
		owner := rowOwner{Family: "vector", Key: v.NodeID, Node: v.NodeID}
		if c, ok := chunkParents[v.NodeID]; ok {
			owner.Node = c.Node
			owner.File = c.File
		}
		if e = put(owner); e != nil {
			return e
		}
	}
	return f.Sync()
}
func (s *segment) indexHidden() {
	s.baseHidden = s.hiddenRows(s.owners.Spaces, -1)
	s.hiddenNames = map[string]int{}
	for i, r := range s.owners.Spaces["node"].rows {
		if s.baseHidden["node"].has(i) {
			s.hiddenNames[r.Name]++
		}
	}
	s.hiddenText = map[string]int{}
	for kind, rows := range s.owners.Text {
		family := kind
		if kind == "symbol" {
			family = "node"
		}
		for _, r := range rows {
			if s.baseRowHidden(family, r.ID) {
				s.hiddenText[kind]++
			}
		}
	}
	for i := range s.layers {
		s.layers[i].hidden = s.hiddenRows(s.layers[i].spaces, i)
	}
}
func (s *segment) hiddenRows(spaces map[string]rowSpace, after int) map[string]rowBitmap {
	out := map[string]rowBitmap{}
	for family, space := range spaces {
		bits := make(rowBitmap, (len(space.rows)+63)/64)
		for i, r := range space.rows {
			hidden := s.masked(r.File, r.Node, after)
			if family == "vector" && s.chunkMask[r.Key] > after+1 {
				hidden = true
			}
			// Symbols use the node bitmap. Content chunks have their own explicit
			// tombstones; file/node replacement expands them in changeRows.
			if family == "content" {
				hidden = s.chunkMask[r.Key] > after+1
			}
			if hidden {
				bits.set(i)
			}
		}
		out[family] = bits
	}
	return out
}
func rowHidden(spaces map[string]rowSpace, masks map[string]rowBitmap, family, key string) bool {
	i, ok := spaces[family].byKey[key]
	return ok && masks[family].has(i)
}
func (s *segment) baseRowHidden(family, key string) bool {
	return rowHidden(s.owners.Spaces, s.baseHidden, family, key)
}
func (l layer) rowHidden(family, key string) bool  { return rowHidden(l.spaces, l.hidden, family, key) }
func (s *segment) hiddenTextCount(kind string) int { return s.hiddenText[kind] }

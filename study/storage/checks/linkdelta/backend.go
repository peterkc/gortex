package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/blevesearch/bleve/v2"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"os"
	"path/filepath"
	"slices"
)

type backend struct {
	format      string
	baseBackend adapter.Backend
}
type seal struct {
	Base    *adapter.SegmentRef
	Files   map[string]string
	Schema  string
	Repo    string
	Format  string
	Parent  *adapter.SegmentRef
	Payload string
	SHA256  string
}

func (b backend) Name() string { return "sealed-" + b.format + "-delta" }
func (b backend) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{
		Operations: map[string]string{"id": "indexed disk base plus file replacement masks", "name": "indexed base and delta merge", "file": "indexed base and delta merge", "out": "indexed source adjacency and owner masks", "in": "indexed target adjacency and owner masks", "symbols": "streamed exact BM25", "content": "streamed exact BM25", "vector": "streamed exact cosine", "delta": "sealed " + b.format + " replacement rows and tombstones"},
		Libraries:  []string{"arrow-go v18.7.0", "parquet-go v0.30.1"}, Deployment: "throwaway indexed JSON base with sealed file deltas", Notes: []string{"Same indexed JSON base for both forms; not a base B/C comparison. Delta rows and base offset metadata stay in memory. Full payload stays on disk in the base. Durable payload+seal fsync, then parent-directory fsync. Incoming dangling edges retained per harness contract."}}
}
func (b backend) Build(ctx context.Context, d dataset.Dataset, path string) (adapter.SegmentRef, adapter.BuildStats, error) {
	if b.format != "arrow" && b.format != "parquet" {
		return adapter.SegmentRef{}, adapter.BuildStats{}, fmt.Errorf("invalid format")
	}
	if d.Source == nil {
		seen := map[string]bool{}
		for _, n := range d.Nodes {
			if n.ID == "" || seen[n.ID] {
				return adapter.SegmentRef{}, adapter.BuildStats{}, fmt.Errorf("empty/duplicate node ID")
			}
			seen[n.ID] = true
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, err
	}
	if err = os.Mkdir(abs, 0700); err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, err
	}
	if b.baseBackend != nil {
		ref, _, e := b.baseBackend.Build(ctx, d, filepath.Join(abs, "c-base"))
		if e != nil {
			return adapter.SegmentRef{}, adapter.BuildStats{}, e
		}
		if e = durableJSON(filepath.Join(abs, "base-ref.json"), ref); e != nil {
			return adapter.SegmentRef{}, adapter.BuildStats{}, e
		}
		return finishSeal(abs, seal{Schema: "linkdelta-seal/v1", Repo: d.Repo, Format: "c-base", Base: &ref, Payload: "base-ref.json"}, "full")
	}
	if err = writeBase(ctx, filepath.Join(abs, "base.jsonl"), d); err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, err
	}
	return finishSeal(abs, seal{Schema: "linkdelta-seal/v1", Repo: d.Repo, Format: "base", Payload: "base.jsonl"}, "full")
}
func (b backend) BuildDelta(ctx context.Context, old adapter.SegmentRef, c adapter.FileChange, path string) (adapter.SegmentRef, adapter.BuildStats, error) {
	opened, err := b.Open(ctx, old.Path, old)
	if err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, err
	}
	rows, err := changeRows(ctx, opened.(*segment), c)
	err = errors.Join(err, opened.Close())
	if err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, err
	}
	if err = os.Mkdir(abs, 0700); err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, err
	}
	payload := "delta." + b.format
	if err = writeDelta(filepath.Join(abs, payload), b.format, rows); err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, err
	}
	if err = buildDeltaText(ctx, abs, rows); err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, err
	}
	return finishSeal(abs, seal{Schema: "linkdelta-seal/v1", Repo: old.Repo, Format: b.format, Parent: &old, Payload: payload}, "delta")
}
func finishSeal(path string, s seal, mode string) (adapter.SegmentRef, adapter.BuildStats, error) {
	var stats adapter.BuildStats
	stats.Mode = mode
	pin, err := dataset.Hash(filepath.Join(path, s.Payload))
	if err != nil {
		return adapter.SegmentRef{}, stats, err
	}
	s.SHA256 = pin
	s.Files = map[string]string{}
	var dirs []string
	err = filepath.WalkDir(path, func(p string, de os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if de.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		if !de.Type().IsRegular() {
			return fmt.Errorf("nonregular sealed output")
		}
		name, e := filepath.Rel(path, p)
		if e != nil {
			return e
		}
		h, e := dataset.Hash(p)
		if e != nil {
			return e
		}
		s.Files[name] = h
		st, e := de.Info()
		if e != nil {
			return e
		}
		stats.Bytes += st.Size()
		f, e := os.Open(p)
		if e != nil {
			return e
		}
		return errors.Join(f.Sync(), f.Close())
	})
	if err != nil {
		return adapter.SegmentRef{}, stats, err
	}
	sealPath := filepath.Join(path, "seal.json")
	if err = durableJSON(sealPath, s); err != nil {
		return adapter.SegmentRef{}, stats, err
	}
	slices.Reverse(dirs)
	dirs = append(dirs, filepath.Dir(path))
	for _, d := range dirs {
		if err = syncDir(d); err != nil {
			return adapter.SegmentRef{}, stats, err
		}
	}
	pin, err = dataset.Hash(sealPath)
	if err != nil {
		return adapter.SegmentRef{}, stats, err
	}
	st, err := os.Stat(sealPath)
	if err != nil {
		return adapter.SegmentRef{}, stats, err
	}
	stats.Bytes += st.Size()
	return adapter.SegmentRef{Repo: s.Repo, Version: pin, Digest: pin, Path: path}, stats, nil
}
func readSeal(ref adapter.SegmentRef) (seal, error) {
	var s seal
	if ref.Path == "" || !filepath.IsAbs(ref.Path) || ref.Version == "" || ref.Version != ref.Digest {
		return s, fmt.Errorf("invalid segment identity")
	}
	if err := checkPath(ref.Path); err != nil {
		return s, err
	}
	path, err := dataset.SafeFile(ref.Path, "seal.json")
	if err != nil {
		return s, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(raw)) != ref.Digest {
		return s, fmt.Errorf("seal digest mismatch")
	}
	if err = json.Unmarshal(raw, &s); err != nil {
		return s, err
	}
	if s.Schema != "linkdelta-seal/v1" || s.Repo != ref.Repo {
		return s, fmt.Errorf("seal identity mismatch")
	}
	expected := "base.jsonl"
	if s.Format == "c-base" {
		expected = "base-ref.json"
		if s.Base == nil || s.Parent != nil || s.Base.Repo != ref.Repo || s.Base.Path != filepath.Join(ref.Path, "c-base") {
			return s, fmt.Errorf("C base identity mismatch")
		}
	} else if s.Format != "base" {
		expected = "delta." + s.Format
		if s.Parent == nil {
			return s, fmt.Errorf("delta without parent")
		}
	} else if s.Parent != nil {
		return s, fmt.Errorf("base has parent")
	}
	if s.Format != "base" && s.Format != "c-base" && s.Format != "arrow" && s.Format != "parquet" {
		return s, fmt.Errorf("unsupported seal format")
	}
	if s.Payload != expected {
		return s, fmt.Errorf("unexpected payload path")
	}
	path, err = dataset.SafeFile(ref.Path, s.Payload)
	if err != nil {
		return s, err
	}
	pin, err := dataset.Hash(path)
	if err != nil {
		return s, err
	}
	if pin != s.SHA256 {
		return s, fmt.Errorf("payload digest mismatch")
	}
	for name, expected := range s.Files {
		p, e := dataset.SafeFile(ref.Path, name)
		if e != nil {
			return s, e
		}
		got, e := dataset.Hash(p)
		if e != nil {
			return s, e
		}
		if got != expected {
			return s, fmt.Errorf("seal artifact digest mismatch: %s", name)
		}
	}
	return s, nil
}
func (b backend) Open(ctx context.Context, path string, ref adapter.SegmentRef) (out adapter.Segment, err error) {
	if path != ref.Path {
		return nil, fmt.Errorf("open path mismatch")
	}
	var reverse []layer
	var textIndexes []bleve.Index
	success := false
	defer func() {
		if !success {
			for _, ix := range textIndexes {
				err = errors.Join(err, ix.Close())
			}
		}
	}()
	seen := map[string]bool{}
	current := ref
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if seen[current.Path] {
			return nil, fmt.Errorf("parent cycle")
		}
		seen[current.Path] = true
		s, err := readSeal(current)
		if err != nil {
			return nil, err
		}
		if s.Repo != ref.Repo {
			return nil, fmt.Errorf("parent repository mismatch")
		}
		if s.Format == "base" || s.Format == "c-base" {
			result := &segment{ref: ref, keys: make(map[string][]int), openedText: textIndexes}
			if s.Format == "c-base" {
				if b.baseBackend == nil {
					return nil, fmt.Errorf("blocked: C base backend is not configured")
				}
				external, e := b.baseBackend.Open(ctx, s.Base.Path, *s.Base)
				if e != nil {
					return nil, e
				}
				result.external = external
				if _, ok := external.(baseResources); !ok {
					_ = external.Close()
					return nil, fmt.Errorf("blocked: C base ScanRecords/TextIndex resources are unavailable")
				}
			} else {
				p := filepath.Join(current.Path, s.Payload)
				index, e := baseIndex(p)
				if e != nil {
					return nil, e
				}
				f, e := os.Open(p)
				if e != nil {
					return nil, e
				}
				result.base = f
				result.index = index
				for i, v := range index {
					result.keys[lookup("family", v.Family)] = append(result.keys[lookup("family", v.Family)], i)
					for _, pair := range [][2]string{{"key", v.Key}, {"source", v.Source}, {"target", v.Target}, {"owner", v.Owner}, {"name", v.Name}} {
						k := lookup(v.Family, pair[0], pair[1])
						result.keys[k] = append(result.keys[k], i)
					}
				}
			}
			for i := len(reverse) - 1; i >= 0; i-- {
				result.layers = append(result.layers, reverse[i])
			}
			if result.external != nil && len(result.layers) > 0 {
				if e := result.configureText(ctx); e != nil {
					_ = result.external.Close()
					return nil, e
				}
			}
			success = true
			return result, nil
		}
		rows, err := readDelta(filepath.Join(current.Path, s.Payload), s.Format)
		if err != nil {
			return nil, err
		}
		l := layer{rows: rows, files: map[string]bool{}, nodes: map[string]bool{}}
		for _, r := range rows {
			if r.Family == "mask_file" {
				l.files[r.Key] = true
			}
			if r.Family == "mask_node" {
				l.nodes[r.Key] = true
			}
		}
		l.symbols, err = bleve.OpenUsing(filepath.Join(current.Path, "symbols.bleve"), map[string]interface{}{"read_only": true})
		if err != nil {
			return nil, err
		}
		textIndexes = append(textIndexes, l.symbols)
		l.content, err = bleve.OpenUsing(filepath.Join(current.Path, "content.bleve"), map[string]interface{}{"read_only": true})
		if err != nil {
			return nil, err
		}
		textIndexes = append(textIndexes, l.content)
		reverse = append(reverse, l)
		current = *s.Parent
	}
}

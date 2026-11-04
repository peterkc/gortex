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
	cbase "study/checks/base"
	"sync"
)

type backend struct {
	format    string
	cache     *adoptionCache
	buildText func(context.Context, string, []row) error
}
type adoptionCache struct {
	mu         sync.Mutex
	verified   map[adapter.SegmentRef]bool
	owners     map[adapter.SegmentRef]ownership
	HashPasses int
	TextCPU    map[string]int64
}

func newBackend(format string) backend {
	return backend{format: format, buildText: buildDeltaText, cache: &adoptionCache{TextCPU: map[string]int64{}, verified: map[adapter.SegmentRef]bool{}, owners: map[adapter.SegmentRef]ownership{}}}
}

type seal struct {
	Base    *adapter.SegmentRef
	Files   map[string]string
	Sizes   map[string]int64
	Schema  string
	Repo    string
	Format  string
	Parent  *adapter.SegmentRef
	Payload string
	SHA256  string
}

func (b backend) Name() string { return "sealed-" + b.format + "-delta" }
func (b backend) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{Operations: map[string]string{"id": "typed C lookup plus indexed delta IDs", "name": "typed indexed merge", "file": "typed indexed merge", "out": "typed indexed source adjacency and owner masks", "in": "typed indexed target adjacency and owner masks", "symbols": "base plus one immutable delta text index", "content": "base plus one immutable delta text index", "vector": "base and delta exact cosine with row bitmaps before top-k", "delta": "sealed " + b.format + " replacement rows and file/node/chunk tombstones"}, Libraries: []string{"arrow-go v18.7.0", "parquet-go v0.30.1", "native Bleve (shared C policy)"}, Deployment: "throwaway C Parquet base plus sealed deltas", Notes: []string{"The same indexed typed merge serves both forms; read cost is format-independent. Compare write/seal/open/bytes for codec choice.", "Durable payload+seal fsync and directory fsync. Full hashing once at seal/adoption; later opens verify seal digest, safe paths and sizes under immutable ownership.", "Incoming dangling edges retained."}}
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
	ref, _, e := (cbase.Backend{SideIndex: false}).Build(ctx, d, filepath.Join(abs, "c-base"))
	if e != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, e
	}
	if e = durableJSON(filepath.Join(abs, "base-ref.json"), ref); e != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, e
	}
	// Persist row ownership once; payloads remain in C. These serving keys and
	// their resident indexes count in baseline bytes and process memory.
	if e = writeOwners(ctx, abs, d); e != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, e
	}
	return b.finishSeal(abs, seal{Schema: "linkdelta-seal/v1", Repo: d.Repo, Format: "c-base", Base: &ref, Payload: "base-ref.json"}, "full")
}
func (b backend) BuildDelta(ctx context.Context, old adapter.SegmentRef, c adapter.FileChange, path string) (adapter.SegmentRef, adapter.BuildStats, error) {
	opened, err := b.Open(ctx, old.Path, old)
	if err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, err
	}
	prior := opened.(*segment)
	rows, err := changeRows(ctx, prior, c)
	if err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, errors.Join(err, opened.Close())
	}
	p0, err := cpuNS()
	if err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, errors.Join(err, opened.Close())
	}
	added, err := indexLayer(rows)
	if err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, errors.Join(err, opened.Close())
	}
	view := *prior
	view.layers = append(slices.Clone(prior.layers), added)
	view.indexMasks()
	var deltaText []row
	for i, l := range view.layers {
		for _, r := range l.textRows {
			if !view.masked(r.Owner, r.Source, i) && (r.Family != "content" || view.chunkMask[r.Key] <= i+1) {
				deltaText = append(deltaText, r)
			}
		}
	}
	p1, err := cpuNS()
	if err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, errors.Join(err, opened.Close())
	}
	if err = opened.Close(); err != nil {
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
	c0, err := cpuNS()
	if err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, err
	}
	if err = b.buildText(ctx, abs, deltaText); err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, err
	}
	c1, err := cpuNS()
	if err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, err
	}
	ref, stats, err := b.finishSeal(abs, seal{Schema: "linkdelta-seal/v1", Repo: old.Repo, Format: b.format, Parent: &old, Payload: payload}, "delta")
	if err == nil {
		b.cache.mu.Lock()
		b.cache.TextCPU[ref.Digest] = (p1 - p0) + (c1 - c0)
		b.cache.mu.Unlock()
	}
	return ref, stats, err
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
	s.Sizes = map[string]int64{}
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
		s.Sizes[name] = st.Size()
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
func (b backend) finishSeal(path string, s seal, mode string) (adapter.SegmentRef, adapter.BuildStats, error) {
	ref, stats, err := finishSeal(path, s, mode)
	if err == nil {
		b.cache.mu.Lock()
		b.cache.verified[ref] = true
		b.cache.mu.Unlock()
	}
	return ref, stats, err
}
func (b backend) readSeal(ref adapter.SegmentRef) (seal, error) {
	s, err := readSeal(ref)
	if err != nil {
		return s, err
	}
	b.cache.mu.Lock()
	defer b.cache.mu.Unlock()
	if b.cache.verified[ref] {
		return s, nil
	}
	for name, want := range s.Files {
		p, e := dataset.SafeFile(ref.Path, name)
		if e != nil {
			return s, e
		}
		got, e := dataset.Hash(p)
		if e != nil {
			return s, e
		}
		if got != want {
			return s, fmt.Errorf("seal artifact digest mismatch: %s", name)
		}
	}
	b.cache.HashPasses++
	b.cache.verified[ref] = true
	return s, nil
}
func (b backend) ownership(ref adapter.SegmentRef) (ownership, error) {
	b.cache.mu.Lock()
	defer b.cache.mu.Unlock()
	if o, ok := b.cache.owners[ref]; ok {
		return o, nil
	}
	o := ownership{ByFile: map[string][]string{}, ByNode: map[string][]string{}}
	var rows []rowOwner
	err := eachJSON(filepath.Join(ref.Path, "rows.jsonl"), func(r rowOwner) error { rows = append(rows, r); return nil })
	if err != nil {
		return o, err
	}
	for _, r := range rows {
		if r.Family == "content" {
			o.ByFile[r.File] = append(o.ByFile[r.File], r.Key)
			o.ByNode[r.Node] = append(o.ByNode[r.Node], r.Key)
		}
	}
	o.Spaces = rowSpaces(rows)
	o.Text = map[string][]chunkOwner{}
	for _, r := range rows {
		if r.Family == "symbol" || r.Family == "content" {
			o.Text[r.Family] = append(o.Text[r.Family], chunkOwner{r.Key, r.Node, r.File})
		}
	}
	b.cache.owners[ref] = o
	return o, nil
}
func readSeal(ref adapter.SegmentRef) (seal, error) {
	var s seal
	if ref.Path == "" || ref.Path != filepath.Clean(ref.Path) || !filepath.IsAbs(ref.Path) || ref.Version == "" || ref.Version != ref.Digest {
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
	expected := "delta." + s.Format
	if s.Format == "c-base" {
		expected = "base-ref.json"
		if s.Base == nil || s.Parent != nil || s.Base.Repo != ref.Repo || s.Base.Path != filepath.Join(ref.Path, "c-base") {
			return s, fmt.Errorf("C base identity mismatch")
		}
	} else if (s.Format != "arrow" && s.Format != "parquet") || s.Parent == nil {
		return s, fmt.Errorf("unsupported seal format or missing parent")
	}
	if s.Payload != expected || s.Files[s.Payload] != s.SHA256 || len(s.Files) == 0 || len(s.Files) != len(s.Sizes) {
		return s, fmt.Errorf("invalid sealed inventory")
	}
	if s.Format == "c-base" {
		if _, ok := s.Files["rows.jsonl"]; !ok {
			return s, fmt.Errorf("missing row-space ownership metadata")
		}
	}
	for name, pin := range s.Files {
		if !digest(pin) {
			return s, fmt.Errorf("invalid artifact hash")
		}
		p, e := dataset.SafeFile(ref.Path, name)
		if e != nil {
			return s, e
		}
		st, e := os.Stat(p)
		if e != nil {
			return s, e
		}
		size, ok := s.Sizes[name]
		if !ok || st.Size() != size {
			return s, fmt.Errorf("seal artifact size mismatch: %s", name)
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
		s, err := b.readSeal(current)
		if err != nil {
			return nil, err
		}
		if s.Repo != ref.Repo {
			return nil, fmt.Errorf("parent repository mismatch")
		}
		if s.Format == "c-base" {
			external, e := (cbase.Backend{SideIndex: false}).Open(ctx, s.Base.Path, *s.Base)
			if e != nil {
				return nil, e
			}
			if _, ok := external.(baseResources); !ok {
				return nil, errors.Join(fmt.Errorf("blocked: C typed/TextIndex resources unavailable"), external.Close())
			}
			owners, e := b.ownership(current)
			if e != nil {
				return nil, errors.Join(e, external.Close())
			}
			result := &segment{ref: ref, external: external, owners: owners, openedText: textIndexes}
			for i := len(reverse) - 1; i >= 0; i-- {
				result.layers = append(result.layers, reverse[i])
			}
			result.indexMasks()
			if len(result.layers) > 0 {
				if e = result.configureText(ctx); e != nil {
					return nil, errors.Join(e, external.Close())
				}
			}
			success = true
			return result, nil
		}

		rows, err := readDelta(filepath.Join(current.Path, s.Payload), s.Format)
		if err != nil {
			return nil, err
		}
		l, err := indexLayer(rows)
		if err != nil {
			return nil, err
		}
		if current == ref {
			for _, name := range []string{"symbols", "content"} {
				ix, e := bleve.OpenUsing(filepath.Join(current.Path, name+".bleve"), map[string]interface{}{"read_only": true})
				if e != nil {
					return nil, e
				}
				textIndexes = append(textIndexes, ix)
			}
		}
		reverse = append(reverse, l)
		current = *s.Parent
	}
}

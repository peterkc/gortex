package dataset

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

// DiskSource describes validated files without retaining their records. The
// optional overlay replaces one file and is small enough for the delta fixture.
type DiskSource struct {
	Dir         string
	OmitFile    string
	RemovedIDs  []string
	Replacement *Dataset
}

func SafeFile(root, name string) (string, error) {
	if name == "" || filepath.IsAbs(name) || filepath.Clean(name) != name || name == "." || strings.HasPrefix(name, ".."+string(filepath.Separator)) || name == ".." {
		return "", fmt.Errorf("unsafe input path %q", name)
	}
	path := filepath.Join(root, name)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved != abs {
		return "", fmt.Errorf("input alias or symlink: %s", name)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("input is not a regular file: %s", name)
	}
	return path, nil
}

func sourceRows[T any](d Dataset, name string, memory []T) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		if d.Source == nil {
			for _, row := range memory {
				if !yield(row, nil) {
					return
				}
			}
			return
		}
		var zero T
		path, err := SafeFile(d.Source.Dir, name)
		if err != nil {
			yield(zero, err)
			return
		}
		f, err := os.Open(path)
		if err != nil {
			yield(zero, err)
			return
		}
		defer f.Close()
		r := bufio.NewReaderSize(f, 64<<10)
		for {
			line, err := r.ReadBytes('\n')
			if err == io.EOF && len(line) == 0 {
				break
			}
			if err != nil {
				yield(zero, fmt.Errorf("truncated %s: %w", name, err))
				return
			}
			if !utf8.Valid(line) {
				yield(zero, fmt.Errorf("invalid UTF-8 in %s", name))
				return
			}
			var row T
			if err = json.Unmarshal(line, &row); err != nil {
				yield(zero, fmt.Errorf("decode %s: %w", name, err))
				return
			}
			if keepRecord(d.Source, row) && !yield(row, nil) {
				return
			}
		}
		if d.Source.Replacement != nil {
			for row, err := range replacementRows[T](*d.Source.Replacement, name) {
				if !yield(row, err) {
					return
				}
			}
		}
	}
}
func replacementRows[T any](d Dataset, name string) iter.Seq2[T, error] {
	var rows any
	switch name {
	case "nodes.jsonl":
		rows = d.Nodes
	case "edges.jsonl":
		rows = d.Edges
	case "files.jsonl":
		rows = d.Files
	case "symbols.jsonl":
		rows = d.Symbols
	case "content.jsonl":
		rows = d.Content
	}
	return sourceRows(d, name, rows.([]T))
}
func keepRecord(s *DiskSource, row any) bool {
	if s.OmitFile == "" {
		return true
	}
	switch v := row.(type) {
	case Node:
		return v.FilePath != s.OmitFile
	case Edge:
		return v.FilePath != s.OmitFile && !slices.Contains(s.RemovedIDs, v.From)
	case File:
		return v.Path != s.OmitFile
	case Symbol:
		return !slices.Contains(s.RemovedIDs, v.NodeID)
	case Content:
		return v.FilePath != s.OmitFile && !slices.Contains(s.RemovedIDs, v.NodeID)
	case Vector:
		return !slices.Contains(s.RemovedIDs, v.NodeID)
	}
	return false
}
func (d Dataset) NodeRows() iter.Seq2[Node, error] { return sourceRows(d, "nodes.jsonl", d.Nodes) }
func (d Dataset) EdgeRows() iter.Seq2[Edge, error] { return sourceRows(d, "edges.jsonl", d.Edges) }
func (d Dataset) FileRows() iter.Seq2[File, error] { return sourceRows(d, "files.jsonl", d.Files) }
func (d Dataset) SymbolRows() iter.Seq2[Symbol, error] {
	return sourceRows(d, "symbols.jsonl", d.Symbols)
}
func (d Dataset) ContentRows() iter.Seq2[Content, error] {
	return sourceRows(d, "content.jsonl", d.Content)
}
func (d Dataset) VectorRows() iter.Seq2[Vector, error] {
	return func(yield func(Vector, error) bool) {
		if d.Source == nil {
			for _, v := range d.Vectors {
				if !yield(v, nil) {
					return
				}
			}
			return
		}
		path, err := SafeFile(d.Source.Dir, "vectors.bin")
		if err != nil {
			yield(Vector{}, err)
			return
		}
		f, err := os.Open(path)
		if err != nil {
			yield(Vector{}, err)
			return
		}
		defer f.Close()
		r := bufio.NewReaderSize(f, 64<<10)
		for {
			var n uint32
			err := binary.Read(r, binary.LittleEndian, &n)
			if err == io.EOF {
				break
			}
			if err != nil {
				yield(Vector{}, err)
				return
			}
			if n == 0 || n > 1<<20 {
				yield(Vector{}, fmt.Errorf("invalid vector id length %d", n))
				return
			}
			id := make([]byte, n)
			if _, err = io.ReadFull(r, id); err != nil {
				yield(Vector{}, err)
				return
			}
			if !utf8.Valid(id) {
				yield(Vector{}, fmt.Errorf("invalid UTF-8 vector id"))
				return
			}
			v := Vector{NodeID: string(id), Values: make([]float32, Dimensions)}
			if err = binary.Read(r, binary.LittleEndian, v.Values); err != nil {
				yield(Vector{}, err)
				return
			}
			for _, x := range v.Values {
				if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
					yield(Vector{}, fmt.Errorf("nonfinite vector"))
					return
				}
			}
			if keepRecord(d.Source, v) && !yield(v, nil) {
				return
			}
		}
		if d.Source.Replacement != nil {
			for v, err := range d.Source.Replacement.VectorRows() {
				if !yield(v, err) {
					return
				}
			}
		}
	}
}
func CountRows(d Dataset) (Counts, error) {
	c := Counts{SkippedSpans: d.Meta.Counts.SkippedSpans}
	for _, err := range d.NodeRows() {
		if err != nil {
			return c, err
		}
		c.Nodes++
	}
	for v, err := range d.EdgeRows() {
		if err != nil {
			return c, err
		}
		c.Edges++
		if v.CrossRepo {
			c.CrossEdges++
		}
	}
	for _, err := range d.FileRows() {
		if err != nil {
			return c, err
		}
		c.Files++
	}
	for _, err := range d.SymbolRows() {
		if err != nil {
			return c, err
		}
		c.Symbols++
	}
	for _, err := range d.ContentRows() {
		if err != nil {
			return c, err
		}
		c.Content++
	}
	for _, err := range d.VectorRows() {
		if err != nil {
			return c, err
		}
		c.Vectors++
	}
	return c, nil
}

// OpenSource validates before returning a descriptor. sort owns a fixed 4 MiB
// scratch buffer; IDs spill to owned temporary files, not a dataset-sized map.
func OpenSource(dir string) (d Dataset, err error) {
	path, err := SafeFile(dir, "manifest.json")
	if err != nil {
		return d, err
	}
	if err = ReadJSON(path, &d.Meta); err != nil {
		return d, err
	}
	if d.Meta.Version != GeneratorVersion || d.Meta.Repo == "" {
		return d, fmt.Errorf("invalid dataset manifest")
	}
	for _, name := range RecordFiles {
		path, err := SafeFile(dir, name)
		if err != nil {
			return d, err
		}
		got, err := Hash(path)
		if err != nil {
			return d, err
		}
		if got != d.Meta.SHA256[name] {
			return d, fmt.Errorf("checksum mismatch: %s", name)
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return d, err
	}
	d.Repo = d.Meta.Repo
	d.Source = &DiskSource{Dir: abs}
	counts, err := CountRows(d)
	if err != nil {
		return d, err
	}
	if counts != d.Meta.Counts {
		return d, fmt.Errorf("record counts do not match manifest")
	}
	for _, family := range []string{"nodes", "symbols", "content", "vectors"} {
		if err = uniqueIDs(sourceIDs(d, family)); err != nil {
			return d, fmt.Errorf("%s: %w", family, err)
		}
	}
	return d, nil
}
func sourceIDs(d Dataset, family string) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		switch family {
		case "nodes":
			for v, err := range d.NodeRows() {
				if !yield(v.ID, err) {
					return
				}
			}
		case "symbols":
			for v, err := range d.SymbolRows() {
				if !yield(v.NodeID, err) {
					return
				}
			}
		case "content":
			for v, err := range d.ContentRows() {
				if !yield(v.ChunkID, err) {
					return
				}
			}
		case "vectors":
			for v, err := range d.VectorRows() {
				if !yield(v.NodeID, err) {
					return
				}
			}
		}
	}
}
func ValidateTierIDs(ds []Dataset) error {
	return uniqueIDs(func(yield func(string, error) bool) {
		for _, d := range ds {
			for id, err := range sourceIDs(d, "nodes") {
				if !yield(id, err) {
					return
				}
			}
		}
	})
}
func uniqueIDs(ids iter.Seq2[string, error]) (err error) {
	scratch, err := os.MkdirTemp("", "gxstudy-ids-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(scratch)) }()
	file, err := os.Create(filepath.Join(scratch, "ids"))
	if err != nil {
		return err
	}
	enc := json.NewEncoder(file)
	for id, e := range ids {
		if e != nil {
			return errors.Join(e, file.Close())
		}
		if id == "" {
			return errors.Join(fmt.Errorf("empty id"), file.Close())
		}
		if e = enc.Encode(id); e != nil {
			return errors.Join(e, file.Close())
		}
	}
	if err = file.Close(); err != nil {
		return err
	}
	sorted := filepath.Join(scratch, "sorted")
	cmd := exec.Command("sort", "-S", "4M", "-T", scratch, "-o", sorted, file.Name())
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	if output, e := cmd.CombinedOutput(); e != nil {
		return fmt.Errorf("sort IDs: %w: %s", e, output)
	}
	input, err := os.Open(sorted)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, input.Close()) }()
	r := bufio.NewReader(input)
	previous := ""
	for {
		line, e := r.ReadString('\n')
		if e == io.EOF {
			return nil
		}
		if e != nil {
			return e
		}
		if line == previous {
			return fmt.Errorf("duplicate id %s", strings.TrimSpace(line))
		}
		previous = line
	}
}
func writeRowStream[T any](path string, rows iter.Seq2[T, error]) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	w := bufio.NewWriterSize(f, 64<<10)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for row, e := range rows {
		if e != nil {
			return e
		}
		if e = enc.Encode(row); e != nil {
			return e
		}
	}
	return w.Flush()
}
func writeVectorStream(path string, rows iter.Seq2[Vector, error]) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	w := bufio.NewWriterSize(f, 64<<10)
	for v, e := range rows {
		if e != nil {
			return e
		}
		if v.NodeID == "" || len(v.NodeID) > 1<<20 || len(v.Values) != Dimensions {
			return fmt.Errorf("invalid vector %s", v.NodeID)
		}
		if e = binary.Write(w, binary.LittleEndian, uint32(len(v.NodeID))); e != nil {
			return e
		}
		if _, e = w.WriteString(v.NodeID); e != nil {
			return e
		}
		if e = binary.Write(w, binary.LittleEndian, v.Values); e != nil {
			return e
		}
	}
	return w.Flush()
}

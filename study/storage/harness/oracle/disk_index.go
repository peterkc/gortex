package oracle

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"unicode/utf8"
)

type edgeSpan struct{ offset, length int64 }

type sourceIdentity struct {
	dir, name string
	info      os.FileInfo
}

func sourceFile(dir, name string) (sourceIdentity, error) {
	path, err := dataset.SafeFile(dir, name)
	if err != nil {
		return sourceIdentity{}, err
	}
	info, err := os.Stat(path)
	return sourceIdentity{dir: dir, name: name, info: info}, err
}
func (s sourceIdentity) validate() error {
	current, err := sourceFile(s.dir, s.name)
	if err != nil {
		return err
	}
	if !os.SameFile(s.info, current.info) || s.info.Size() != current.info.Size() || !s.info.ModTime().Equal(current.info.ModTime()) {
		return fmt.Errorf("checker source changed: %s", s.name)
	}
	return nil
}

type edgeIndex struct {
	file               *os.File
	identity           sourceIdentity
	incoming, outgoing map[string][]edgeSpan
	// Counts decoded source rows, including lookup reads. Tests can distinguish
	// one construction pass plus matching rows from a full scan per frontier.
	decodedRows int
}

func newEdgeIndex(ctx context.Context, d dataset.Dataset) (index *edgeIndex, err error) {
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	identity, err := sourceFile(d.Source.Dir, "edges.jsonl")
	if err != nil {
		return nil, err
	}
	path, err := dataset.SafeFile(d.Source.Dir, "edges.jsonl")
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, file.Close())
		}
	}()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(identity.info, opened) {
		return nil, fmt.Errorf("edge source replaced during open")
	}
	index = &edgeIndex{file: file, identity: identity, incoming: map[string][]edgeSpan{}, outgoing: map[string][]edgeSpan{}}
	checksum := sha256.New()
	reader := bufio.NewReaderSize(file, 64<<10)
	var offset int64
	for {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		line, readErr := reader.ReadBytes('\n')
		if readErr == io.EOF && len(line) == 0 {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("truncated edges.jsonl: %w", readErr)
		}
		if !utf8.Valid(line) {
			return nil, fmt.Errorf("invalid UTF-8 in edges.jsonl")
		}
		var edge dataset.Edge
		if err = json.Unmarshal(line, &edge); err != nil {
			return nil, fmt.Errorf("decode edges.jsonl: %w", err)
		}
		checksum.Write(line)
		span := edgeSpan{offset: offset, length: int64(len(line))}
		index.incoming[edge.To] = append(index.incoming[edge.To], span)
		index.outgoing[edge.From] = append(index.outgoing[edge.From], span)
		offset += span.length
		index.decodedRows++
	}
	if offset != identity.info.Size() || index.decodedRows != d.Meta.Counts.Edges || fmt.Sprintf("%x", checksum.Sum(nil)) != d.Meta.SHA256["edges.jsonl"] {
		return nil, fmt.Errorf("edge source size, count or checksum mismatch")
	}
	if err = identity.validate(); err != nil {
		return nil, err
	}
	return index, nil
}

func (index *edgeIndex) edges(ctx context.Context, id, kind string, incoming bool) ([]dataset.Edge, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := index.identity.validate(); err != nil {
		return nil, err
	}
	spans := index.outgoing[id]
	if incoming {
		spans = index.incoming[id]
	}
	var out []dataset.Edge
	for _, span := range spans {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if span.offset < 0 || span.length <= 0 || span.offset > index.identity.info.Size()-span.length {
			return nil, fmt.Errorf("invalid edge span")
		}
		line := make([]byte, span.length)
		if _, err := index.file.ReadAt(line, span.offset); err != nil {
			return nil, err
		}
		if line[len(line)-1] != '\n' || !utf8.Valid(line) {
			return nil, fmt.Errorf("invalid indexed edge row")
		}
		var edge dataset.Edge
		if err := json.Unmarshal(line, &edge); err != nil {
			return nil, err
		}
		index.decodedRows++
		key := edge.From
		if incoming {
			key = edge.To
		}
		if key != id {
			return nil, fmt.Errorf("edge index endpoint mismatch")
		}
		if kind == "" || kind == "any" || kind == edge.Kind {
			out = append(out, edge)
		}
	}
	if err := index.identity.validate(); err != nil {
		return nil, err
	}
	return out, nil
}

// CheckerView owns reference-only edge offsets, never candidate answers or
// engine state. Callers serialize Reference and Close. Overlays and memory
// datasets deliberately retain the independent scan-backed path.
type CheckerView struct {
	data     []dataset.Dataset
	segments map[string]adapter.Segment
	global   *DiskMulti
	sources  []sourceIdentity
	indexes  []*edgeIndex
	closed   bool
}

func NewCheckerView(ctx context.Context, ds []dataset.Dataset) (view *CheckerView, err error) {
	// The descriptor (including manifest, overlay and memory records) is owned by
	// this view, not by a caller that can mutate a map or slice after admission.
	encoded, err := json.Marshal(ds)
	if err != nil {
		return nil, err
	}
	view = &CheckerView{}
	if err = json.Unmarshal(encoded, &view.data); err != nil {
		return nil, err
	}
	owned := view
	defer func() {
		if err != nil {
			err = errors.Join(err, owned.Close())
		}
	}()
	view.segments, view.global = DiskView(view.data)
	for _, d := range view.data {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if d.Source == nil {
			continue
		}
		immutable := d.Source.OmitFile == "" && d.Source.Replacement == nil && len(d.Source.RemovedIDs) == 0
		var meta dataset.Manifest
		manifestPath, e := dataset.SafeFile(d.Source.Dir, "manifest.json")
		if e != nil {
			return nil, e
		}
		manifestIdentity, e := sourceFile(d.Source.Dir, "manifest.json")
		if e != nil {
			return nil, e
		}
		if e = dataset.ReadJSON(manifestPath, &meta); e != nil {
			return nil, e
		}
		// ChangeSource updates the logical counts while the manifest still
		// describes the unchanged base files. This view scans the overlay.
		if !immutable {
			meta.Counts = d.Meta.Counts
		}
		if !reflect.DeepEqual(meta, d.Meta) {
			return nil, fmt.Errorf("checker manifest differs from descriptor")
		}
		view.sources = append(view.sources, manifestIdentity)
		for _, name := range dataset.RecordFiles {
			if err = ctx.Err(); err != nil {
				return nil, err
			}
			identity, e := sourceFile(d.Source.Dir, name)
			if e != nil {
				return nil, e
			}
			view.sources = append(view.sources, identity)
			if name != "edges.jsonl" || !immutable {
				checksum, e := dataset.Hash(filepath.Join(d.Source.Dir, name))
				if e != nil {
					return nil, e
				}
				if checksum != d.Meta.SHA256[name] {
					return nil, fmt.Errorf("checker checksum mismatch: %s", name)
				}
			}
		}
		if !immutable {
			continue
		}
		index, e := newEdgeIndex(ctx, d)
		if e != nil {
			return nil, e
		}
		view.indexes = append(view.indexes, index)
		view.segments[d.Repo].(*DiskSegment).index = index
	}
	if err = view.Validate(ctx); err != nil {
		return nil, err
	}
	return view, nil
}

// Validate refuses a changed path, file identity, size or modification time.
// Construction hashes the same newline-delimited bytes decoded by the scanner.
func (v *CheckerView) Validate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if v.closed {
		return fmt.Errorf("checker view is closed")
	}
	for _, source := range v.sources {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := source.validate(); err != nil {
			return err
		}
	}
	return nil
}
func (v *CheckerView) Close() error {
	if v.closed {
		return nil
	}
	v.closed = true
	var failures []error
	for _, index := range v.indexes {
		failures = append(failures, index.file.Close())
	}
	return errors.Join(failures...)
}

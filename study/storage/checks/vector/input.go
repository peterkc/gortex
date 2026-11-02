package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gxstudy/dataset"
)

const dimensions = 768
const fixtureRecipe = "32 seeded unit clusters; adjacent document pairs duplicate; normalize(0.8*center+0.2*unit-noise); held-out query noise offset=1000000000; F64 normalization then F32"

type identity struct {
	ID     string `json:"id"`
	Parent string `json:"parent_id,omitempty"`
	Repo   string `json:"repo"`
}
type fixtureManifest struct {
	Synthetic       bool              `json:"synthetic"`
	Recipe          string            `json:"recipe"`
	Seed            int64             `json:"seed"`
	Documents       int               `json:"documents"`
	Queries         int               `json:"queries"`
	Dimensions      int               `json:"dimensions"`
	SourceRevision  string            `json:"source_revision"`
	GeneratorSHA256 string            `json:"generator_sha256"`
	SHA256          map[string]string `json:"sha256"`
}
type input struct {
	manifest           fixtureManifest
	documents, queries []identity
	matrix             *matrix
	vectors            [][]float32
}

func inputPath(dir, name string) (string, error) {
	path, err := filepath.Abs(filepath.Join(dir, name))
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if path != resolved {
		return "", fmt.Errorf("symlink/alias input refused: %s", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("nonregular input: %s", path)
	}
	return path, nil
}

func readManifest(dir, expected string) (fixtureManifest, error) {
	var m fixtureManifest
	path, err := inputPath(dir, "manifest.json")
	if err != nil {
		return m, err
	}
	digest, err := dataset.Hash(path)
	if err != nil {
		return m, err
	}
	if !validSHA(expected) || digest != expected {
		return m, fmt.Errorf("fixture manifest hash mismatch")
	}
	if err := readJSON(path, &m); err != nil {
		return m, err
	}
	if !m.Synthetic || m.Recipe != fixtureRecipe || m.Dimensions != dimensions || m.Documents < 1 || m.Documents > 150000 || m.Queries < 1 || m.Queries > min(4096, m.Documents) || !validRevision(m.SourceRevision) || !validSHA(m.GeneratorSHA256) {
		return m, fmt.Errorf("unsupported fixture identity/count/dimension/recipe")
	}
	if len(m.SHA256) != 3 {
		return m, fmt.Errorf("fixture requires exactly documents.jsonl, queries.jsonl and vectors.npy hashes")
	}
	for _, name := range []string{"documents.jsonl", "queries.jsonl", "vectors.npy"} {
		path, err := inputPath(dir, name)
		if err != nil {
			return m, err
		}
		pin, err := dataset.Hash(path)
		if err != nil {
			return m, err
		}
		if !validSHA(m.SHA256[name]) || pin != m.SHA256[name] {
			return m, fmt.Errorf("fixture input hash mismatch: %s", name)
		}
	}
	return m, nil
}

func readIdentities(path string, count int) ([]identity, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 16<<20)
	seen := map[string]bool{}
	rows := make([]identity, 0, count)
	for scanner.Scan() {
		var row identity
		// Existing accuracy rows contain body/text/language fields; only identities are consumed.
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return nil, err
		}
		if len(rows) >= count || row.ID == "" || row.Repo == "" || seen[row.ID] {
			return nil, fmt.Errorf("invalid/duplicate/extra identity at row %d", len(rows))
		}
		seen[row.ID] = true
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(rows) != count {
		return nil, fmt.Errorf("identity count mismatch: got %d want %d", len(rows), count)
	}
	return rows, nil
}

// NPY v1/v2, little-endian F32, C order. Object/pickle and Fortran layouts refuse.
var npyHeader = regexp.MustCompile(`^\{'descr': '<f4', 'fortran_order': False, 'shape': \(([0-9]+), ([0-9]+)\), \} *\n$`)

func readNPY(ctx context.Context, path string, documents, queries int, ids []identity) (*matrix, [][]float32, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	prefix := make([]byte, 8)
	if _, err := io.ReadFull(f, prefix); err != nil {
		return nil, nil, err
	}
	if string(prefix[:6]) != "\x93NUMPY" || prefix[7] != 0 || (prefix[6] != 1 && prefix[6] != 2) {
		return nil, nil, fmt.Errorf("unsupported NPY magic/version")
	}
	var size uint32
	if prefix[6] == 1 {
		var n uint16
		if err := binary.Read(f, binary.LittleEndian, &n); err != nil {
			return nil, nil, err
		}
		size = uint32(n)
	} else {
		if err := binary.Read(f, binary.LittleEndian, &size); err != nil {
			return nil, nil, err
		}
	}
	if size < 1 || size > 4096 {
		return nil, nil, fmt.Errorf("NPY header outside 1..4096 bytes")
	}
	header := make([]byte, size)
	if _, err := io.ReadFull(f, header); err != nil {
		return nil, nil, err
	}
	match := npyHeader.FindStringSubmatch(string(header))
	if match == nil {
		return nil, nil, fmt.Errorf("NPY must be little-endian F32, C order, rank 2")
	}
	n, err := strconv.Atoi(match[1])
	if err != nil {
		return nil, nil, err
	}
	d, err := strconv.Atoi(match[2])
	if err != nil {
		return nil, nil, err
	}
	if n != documents+queries || d != dimensions {
		return nil, nil, fmt.Errorf("NPY shape mismatch")
	}
	offset, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if info.Size() != offset+int64(n)*dimensions*4 {
		return nil, nil, fmt.Errorf("NPY truncated or has trailing bytes")
	}
	m := &matrix{dim: dimensions, ids: make([]string, documents), raw: make([]float32, documents*dimensions), norms: make([]float64, documents)}
	q := make([][]float32, queries)
	buffer := make([]byte, dimensions*4)
	reader := bufio.NewReaderSize(f, 1<<20)
	for i := range n {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if _, err := io.ReadFull(reader, buffer); err != nil {
			return nil, nil, err
		}
		var row []float32
		if i < documents {
			row = m.raw[i*dimensions : (i+1)*dimensions]
		} else {
			row = make([]float32, dimensions)
			q[i-documents] = row
		}
		for j := range row {
			row[j] = math.Float32frombits(binary.LittleEndian.Uint32(buffer[j*4:]))
		}
		norm, err := vectorNorm(row, dimensions)
		if err != nil {
			return nil, nil, err
		}
		if math.Abs(norm-1) > 1e-5 {
			return nil, nil, fmt.Errorf("row %d is not L2-normalized", i)
		}
		if i < documents {
			m.ids[i] = ids[i].ID
			m.norms[i] = norm
		}
	}
	return m, q, nil
}

func loadInput(ctx context.Context, dir, pin string) (input, error) {
	var in input
	var err error
	in.manifest, err = readManifest(dir, pin)
	if err != nil {
		return in, err
	}
	in.documents, err = readIdentities(filepath.Join(dir, "documents.jsonl"), in.manifest.Documents)
	if err != nil {
		return in, err
	}
	in.queries, err = readIdentities(filepath.Join(dir, "queries.jsonl"), in.manifest.Queries)
	if err != nil {
		return in, err
	}
	parents := map[string]string{}
	for _, row := range in.documents {
		if row.Parent == "" {
			return in, fmt.Errorf("empty document parent ID")
		}
		if _, ok := parents[row.Parent]; ok {
			return in, fmt.Errorf("duplicate document parent ID %q", row.Parent)
		}
		parents[row.Parent] = row.Repo
	}
	for _, row := range in.queries {
		if parents[row.ID] != row.Repo {
			return in, fmt.Errorf("query target is absent from its repository")
		}
	}
	in.matrix, in.vectors, err = readNPY(ctx, filepath.Join(dir, "vectors.npy"), in.manifest.Documents, in.manifest.Queries, in.documents)
	return in, err
}

func unitNoise(seed int64, position int) []float32 {
	rng := rand.New(rand.NewSource(seed + int64(position)*104729))
	row := make([]float32, dimensions)
	var norm float64
	for i := range row {
		row[i] = rng.Float32()*2 - 1
		norm += float64(row[i]) * float64(row[i])
	}
	for i := range row {
		row[i] = float32(float64(row[i]) / math.Sqrt(norm))
	}
	return row
}
func clustered(seed int64, position int, center []float32) []float32 {
	row := unitNoise(seed, position)
	var norm float64
	for i := range row {
		row[i] = float32(.8*float64(center[i]) + .2*float64(row[i]))
		norm += float64(row[i]) * float64(row[i])
	}
	for i := range row {
		row[i] = float32(float64(row[i]) / math.Sqrt(norm))
	}
	return row
}

func writeFixture(ctx context.Context, dir, revision, generator string, seed int64, documents, queries int) (err error) {
	if documents < 1 || documents > 150000 || queries < 1 || queries > min(documents, 4096) || !validRevision(revision) || !validSHA(generator) {
		return fmt.Errorf("invalid generator arguments")
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	manifest := fixtureManifest{true, fixtureRecipe, seed, documents, queries, dimensions, revision, generator, map[string]string{}}
	for _, role := range []string{"documents", "queries"} {
		count := documents
		if role == "queries" {
			count = queries
		}
		rows := make([]identity, count)
		for i := range count {
			id := fmt.Sprintf("synthetic-%09d", i)
			rows[i] = identity{ID: id, Repo: "synthetic"}
			if role == "documents" {
				rows[i].Parent = id
			}
		}
		f, err := os.OpenFile(filepath.Join(dir, role+".jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		w := bufio.NewWriter(f)
		encoder := json.NewEncoder(w)
		for _, row := range rows {
			if err := encoder.Encode(row); err != nil {
				f.Close()
				return err
			}
		}
		err = errors.Join(w.Flush(), f.Sync(), f.Close())
		if err != nil {
			return err
		}
	}
	f, err := os.OpenFile(filepath.Join(dir, "vectors.npy"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	w := bufio.NewWriterSize(f, 1<<20)
	header := fmt.Sprintf("{'descr': '<f4', 'fortran_order': False, 'shape': (%d, %d), }", documents+queries, dimensions)
	header += strings.Repeat(" ", (64-(10+len(header)+1)%64)%64) + "\n"
	if _, err := w.WriteString("\x93NUMPY\x01\x00"); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint16(len(header))); err != nil {
		return err
	}
	if _, err := w.WriteString(header); err != nil {
		return err
	}
	centers := make([][]float32, 32)
	for i := range centers {
		centers[i] = unitNoise(seed, 2000000000+i)
	}
	for i := range documents + queries {
		if err := ctx.Err(); err != nil {
			return err
		}
		position, cluster := i/2, (i/2)%32
		if i >= documents {
			qi := i - documents
			position = 1000000000 + qi
			cluster = qi % 32
		}
		if err := binary.Write(w, binary.LittleEndian, clustered(seed, position, centers[cluster])); err != nil {
			return err
		}
	}
	if err := errors.Join(w.Flush(), f.Sync()); err != nil {
		return err
	}
	for _, name := range []string{"documents.jsonl", "queries.jsonl", "vectors.npy"} {
		pin, err := dataset.Hash(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		manifest.SHA256[name] = pin
	}
	return writeJSON(filepath.Join(dir, "manifest.json"), manifest)
}

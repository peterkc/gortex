package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/sys/unix"
	"hybridfollowup/internal/quantindex"
)

const dimensions = 1024

type record struct {
	ID     string `json:"id"`
	Parent string `json:"parent_id"`
	Repo   string `json:"repo"`
	Key    uint64 `json:"key"`
}
type fingerprint struct {
	SHA        string `json:"sha256"`
	Bytes      int64  `json:"bytes"`
	Rows       int    `json:"rows"`
	Dimensions int    `json:"dimensions"`
}
type metadata struct {
	Documents, Queries []record
	Parents            []string
	KeyToPosition      map[uint64]int
	ReceiptSHA         string
	Directory          string
}

func fileSHA(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func readJSON(path string, out any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
func loadMetadata(directory string) (metadata, error) {
	d := metadata{Directory: directory, KeyToPosition: map[uint64]int{}}
	if err := readJSON(filepath.Join(directory, "records.json"), &d.Documents); err != nil {
		return d, err
	}
	if err := readJSON(filepath.Join(directory, "query-records.json"), &d.Queries); err != nil {
		return d, err
	}
	if len(d.Documents) < 10 || len(d.Queries) == 0 {
		return d, fmt.Errorf("dataset too small")
	}
	seen := map[string]bool{}
	for i, row := range d.Documents {
		if row.ID == "" || row.Parent == "" || seen[row.ID] {
			return d, fmt.Errorf("invalid document ID/parent")
		}
		if _, ok := d.KeyToPosition[row.Key]; ok {
			return d, fmt.Errorf("duplicate native key")
		}
		seen[row.ID] = true
		d.KeyToPosition[row.Key] = i
		d.Parents = append(d.Parents, row.Parent)
	}
	parentSet := map[string]bool{}
	repo := d.Documents[0].Repo
	for _, row := range d.Documents {
		if row.Repo != repo {
			return d, fmt.Errorf("mixed-repository export")
		}
		parentSet[row.Parent] = true
	}
	for _, row := range d.Queries {
		if row.Repo != repo || !parentSet[row.ID] {
			return d, fmt.Errorf("query target missing from repository corpus")
		}
	}
	var err error
	d.ReceiptSHA, err = fileSHA(filepath.Join(directory, "input-receipt.json"))
	return d, err
}
func verifyInputs(d metadata) error {
	var fingerprints map[string]fingerprint
	if err := readJSON(filepath.Join(d.Directory, "input-receipt.json"), &fingerprints); err != nil {
		return err
	}
	for _, name := range []string{"document.f32", "query.f32", "records.json", "query-records.json"} {
		fp, ok := fingerprints[name]
		if !ok {
			return fmt.Errorf("missing input fingerprint %s", name)
		}
		sha, err := fileSHA(filepath.Join(d.Directory, name))
		if err != nil {
			return err
		}
		if sha != fp.SHA {
			return fmt.Errorf("input hash drift %s", name)
		}
	}
	return nil
}

type matrix struct {
	raw  []byte
	Rows int
}

func openMatrix(path string, rows int) (*matrix, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if rows < 1 || st.Size() != int64(rows)*dimensions*4 {
		return nil, fmt.Errorf("matrix size mismatch")
	}
	raw, err := unix.Mmap(int(f.Fd()), 0, int(st.Size()), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return nil, err
	}
	return &matrix{raw: raw, Rows: rows}, nil
}
func (m *matrix) Close() error { return unix.Munmap(m.raw) }
func (m *matrix) Vector(position int) []float32 {
	raw := m.raw[position*dimensions*4 : (position+1)*dimensions*4]
	v := make([]float32, dimensions)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return v
}

type query struct {
	Values []float32
	Norm   float64
}

func newQuery(values []float32) (query, error) {
	q := query{Values: values}
	for _, x := range values {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return q, fmt.Errorf("nonfinite vector")
		}
		q.Norm += float64(x) * float64(x)
	}
	if q.Norm == 0 {
		return q, fmt.Errorf("zero vector")
	}
	return q, nil
}
func (m *matrix) Norms() ([]float64, error) {
	out := make([]float64, m.Rows)
	for i := range out {
		q, err := newQuery(m.Vector(i))
		if err != nil {
			return nil, err
		}
		out[i] = q.Norm
	}
	return out, nil
}
func (m *matrix) Cosine(q query, position int) float64 {
	raw := m.raw[position*dimensions*4 : (position+1)*dimensions*4]
	var dot, norm float64
	for i, x := range q.Values {
		v := float64(math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:])))
		dot += float64(x) * v
		norm += v * v
	}
	return dot / math.Sqrt(q.Norm*norm)
}
func (m *matrix) ExactCosine(q query, position int, norms []float64) float64 {
	raw := m.raw[position*dimensions*4 : (position+1)*dimensions*4]
	var dot float64
	for i, x := range q.Values {
		v := math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
		dot += float64(x) * float64(v)
	}
	return dot / math.Sqrt(q.Norm*norms[position])
}

type hit struct {
	Position int     `json:"position"`
	Score    float64 `json:"score"`
}

func compareHit(a, b hit, documents []record) int {
	if a.Score > b.Score {
		return -1
	}
	if a.Score < b.Score {
		return 1
	}
	if documents[a.Position].ID < documents[b.Position].ID {
		return -1
	}
	if documents[a.Position].ID > documents[b.Position].ID {
		return 1
	}
	return 0
}
func keepTop10(best []hit, h hit, documents []record) []hit {
	compare := func(a, b hit) int { return compareHit(a, b, documents) }
	if len(best) < 10 {
		best = append(best, h)
		slices.SortFunc(best, compare)
	} else if compare(h, best[9]) < 0 {
		best[9] = h
		slices.SortFunc(best, compare)
	}
	return best
}

type truthQuery struct {
	QueryPosition int      `json:"query_position"`
	Raw           []hit    `json:"raw_top10"`
	Parents       []string `json:"parent_top10"`
}
type truthFile struct {
	ReceiptSHA string       `json:"input_receipt_sha256"`
	Queries    []truthQuery `json:"queries"`
	Metric     string       `json:"metric"`
}

func exactTruth(d metadata, documents, queries *matrix) (truthFile, error) {
	result := truthFile{ReceiptSHA: d.ReceiptSHA, Metric: "F64 cosine accumulation over original stored F32"}
	norms, err := documents.Norms()
	if err != nil {
		return result, err
	}
	parentNumber := map[string]int{}
	var parentNames []string
	var firstPositions []int
	parentForDoc := make([]int, len(d.Documents))
	for i, row := range d.Documents {
		number, ok := parentNumber[row.Parent]
		if !ok {
			number = len(parentNames)
			parentNumber[row.Parent] = number
			parentNames = append(parentNames, row.Parent)
			firstPositions = append(firstPositions, i)
		}
		parentForDoc[i] = number
	}
	for qi := range d.Queries {
		q, err := newQuery(queries.Vector(qi))
		if err != nil {
			return result, err
		}
		parentScores := make([]hit, len(parentNames))
		for i := range parentScores {
			parentScores[i] = hit{Position: firstPositions[i], Score: math.Inf(-1)}
		}
		var best []hit
		for position := range d.Documents {
			score := documents.ExactCosine(q, position, norms)
			best = keepTop10(best, hit{position, score}, d.Documents)
			p := parentForDoc[position]
			if score > parentScores[p].Score {
				parentScores[p] = hit{position, score}
			}
		}
		// Stable corpus order resolves equal parent scores, matching the text study.
		compareParent := func(a, b hit) int {
			if a.Score > b.Score {
				return -1
			}
			if a.Score < b.Score {
				return 1
			}
			return a.Position - b.Position
		}
		var parentBest []hit
		for _, h := range parentScores {
			if len(parentBest) < 10 {
				parentBest = append(parentBest, h)
				slices.SortFunc(parentBest, compareParent)
			} else if compareParent(h, parentBest[9]) < 0 {
				parentBest[9] = h
				slices.SortFunc(parentBest, compareParent)
			}
		}
		parents := make([]string, len(parentBest))
		for i, h := range parentBest {
			parents[i] = d.Parents[h.Position]
		}
		result.Queries = append(result.Queries, truthQuery{qi, best, parents})
	}
	return result, nil
}

// Generated latency input uses the timing command's generator and held-out positions.
func generateInputs(directory string, n int) (err error) {
	if n < 10 || n > 1000000 {
		return fmt.Errorf("generated row count outside 10..1000000")
	}
	if err = os.Mkdir(directory, 0755); err != nil {
		return err
	}
	receipt := map[string]fingerprint{}
	for _, name := range []string{"document", "query"} {
		count, offset, recordsName := n, 0, "records.json"
		if name == "query" {
			count, offset, recordsName = 256, 1000000000, "query-records.json"
		}
		var f *os.File
		path := filepath.Join(directory, name+".f32")
		f, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0444)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, f.Close()) }()
		rows := make([]record, count)
		for i := range count {
			id := fmt.Sprintf("synthetic-%012d", i%n)
			rows[i] = record{ID: id, Parent: id, Repo: "generated", Key: uint64(i)}
			v := quantindex.SyntheticVector(20261023, i+offset, dimensions)
			if err = binary.Write(f, binary.LittleEndian, v); err != nil {
				return err
			}
		}
		if err = f.Sync(); err != nil {
			return err
		}
		if err = writeNewJSON(filepath.Join(directory, recordsName), rows); err != nil {
			return err
		}
		for _, file := range []string{name + ".f32", recordsName} {
			sha, hashErr := fileSHA(filepath.Join(directory, file))
			if hashErr != nil {
				return hashErr
			}
			info, statErr := os.Stat(filepath.Join(directory, file))
			if statErr != nil {
				return statErr
			}
			receipt[file] = fingerprint{sha, info.Size(), count, dimensions}
		}
	}
	return writeNewJSON(filepath.Join(directory, "input-receipt.json"), receipt)
}

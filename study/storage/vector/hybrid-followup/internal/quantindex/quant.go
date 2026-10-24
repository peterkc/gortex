// Package quantindex implements exact scans in a quantized metric, followed by
// float32 cosine reranking. It is approximate relative to float32 ground truth.
package quantindex

import (
	"bufio"
	"container/heap"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"

	"hybridfollowup/internal/vector"
	"math"
	"math/bits"
	"os"
)

type Index struct {
	IDs              []string
	data, quant, raw []byte
	norms            []int64
	Mode             string
	Candidates       int
	dimensions       int
}

func quantize(v []float32) []byte {
	var largest float32
	for _, x := range v {
		if x < 0 {
			x = -x
		}
		if x > largest {
			largest = x
		}
	}
	b := make([]byte, len(v))
	for i, x := range v {
		b[i] = byte(int8(math.Round(float64(x / largest * 127))))
	}
	return b
}
func binaryVector(v []float32) []byte {
	b := make([]byte, len(v)/8)
	for i, x := range v {
		if x >= 0 {
			b[i/8] |= 1 << uint(i%8)
		}
	}
	return b
}
func Build(ctx context.Context, path, mode string, vectors []vector.Vector) error {
	return BuildDimensions(ctx, path, mode, vectors, vector.Dimensions)
}
func BuildDimensions(ctx context.Context, path, mode string, vectors []vector.Vector, dimensions int) error {
	if dimensions < 1 || dimensions > 65536 || dimensions%64 != 0 {
		return fmt.Errorf("invalid quantized dimensions")
	}

	if mode != "int8" && mode != "binary" {
		return fmt.Errorf("unknown quantization %s", mode)
	}
	ids := make([]string, len(vectors))
	seen := map[string]bool{}
	for i, v := range vectors {
		if v.NodeID == "" || seen[v.NodeID] {
			return fmt.Errorf("invalid vector ID")
		}
		seen[v.NodeID] = true
		ids[i] = v.NodeID
		if err := validateDimensions(v.Values, dimensions); err != nil {
			return err
		}
	}
	meta, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)
	magic := "QINT8v02"
	if mode == "binary" {
		magic = "QBIN1v02"
	}
	if _, err = w.WriteString(magic); err != nil {
		return err
	}
	for _, n := range []uint64{uint64(len(vectors)), uint64(len(meta)), uint64(dimensions)} {
		if err = binary.Write(w, binary.LittleEndian, n); err != nil {
			return err
		}
	}
	if _, err = w.Write(meta); err != nil {
		return err
	}
	for _, v := range vectors {
		if err = ctx.Err(); err != nil {
			return err
		}
		b := quantize(v.Values)
		if mode == "binary" {
			b = binaryVector(v.Values)
		}
		if _, err = w.Write(b); err != nil {
			return err
		}
	}
	for _, v := range vectors {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = binary.Write(w, binary.LittleEndian, v.Values); err != nil {
			return err
		}
	}
	if err = w.Flush(); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	return errors.Join(f.Close(), os.Chmod(path, 0444))
}
func Open(path, mode string, candidates int) (*Index, error) {
	if mode != "int8" && mode != "binary" {
		return nil, fmt.Errorf("unknown quantization mode")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() < 24 {
		return nil, fmt.Errorf("short quantized header")
	}
	data, err := unix.Mmap(int(f.Fd()), 0, int(st.Size()), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Index, error) { unix.Munmap(data); return nil, err }
	magic := "QINT8"
	if mode == "binary" {
		magic = "QBIN1"
	}
	version := string(data[:8])
	header := 24
	dimensions := 384
	if version == magic+"v02" {
		if len(data) < 32 {
			return fail(fmt.Errorf("short quantized header"))
		}
		d := binary.LittleEndian.Uint64(data[24:32])
		if d < 1 || d > 65536 || d%64 != 0 {
			return fail(fmt.Errorf("invalid quantized dimensions"))
		}
		dimensions = int(d)
		header = 32
	} else if version != magic+"v01" {
		return fail(fmt.Errorf("quantized mode mismatch"))
	}
	stride := dimensions
	if mode == "binary" {
		stride = dimensions / 8
	}
	rawStride := dimensions * 4
	n := binary.LittleEndian.Uint64(data[8:16])
	meta := binary.LittleEndian.Uint64(data[16:24])
	if n > uint64(len(data))/uint64(stride+rawStride) || meta > uint64(len(data)-header) {
		return fail(fmt.Errorf("invalid quantized lengths"))
	}
	qstart := header + int(meta)
	body := int(n) * (stride + rawStride)
	if body != len(data)-qstart {
		return fail(fmt.Errorf("quantized file length mismatch"))
	}
	qend := qstart + int(n)*stride
	idx := &Index{data: data, quant: data[qstart:qend], raw: data[qend:], Mode: mode, Candidates: candidates, dimensions: dimensions}
	if err = json.Unmarshal(data[header:qstart], &idx.IDs); err != nil {
		return fail(err)
	}
	if len(idx.IDs) != int(n) {
		return fail(fmt.Errorf("ID count mismatch"))
	}
	if mode == "int8" {
		idx.norms = make([]int64, n)
		for i := range idx.norms {
			for _, x := range idx.quant[i*idx.dimensions : (i+1)*idx.dimensions] {
				v := int64(int8(x))
				idx.norms[i] += v * v
			}
		}
	}
	return idx, nil
}
func (idx *Index) Close() error {
	if idx.data == nil {
		return nil
	}
	err := unix.Munmap(idx.data)
	idx.data = nil
	return err
}
func (idx *Index) Search(ctx context.Context, q []float32, k int) ([]vector.Hit, error) {
	if err := validateDimensions(q, idx.dimensions); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if k == 0 {
		return nil, nil
	}
	if k < 0 {
		k = len(idx.IDs)
	}
	limit := max(k, idx.Candidates)
	hits := &candidateHeap{}
	heap.Init(hits)
	quant := quantize(q)
	var qnorm int64
	for _, x := range quant {
		v := int64(int8(x))
		qnorm += v * v
	}
	if idx.Mode == "binary" {
		quant = binaryVector(q)
	}
	for i := range idx.IDs {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		var score float64
		if idx.Mode == "int8" {
			var dot int32
			v := idx.quant[i*idx.dimensions : (i+1)*idx.dimensions]
			for j, x := range quant {
				dot += int32(int8(x)) * int32(int8(v[j]))
			}
			score = float64(dot) / math.Sqrt(float64(qnorm*idx.norms[i]))
		} else {
			var distance int
			v := idx.quant[i*(idx.dimensions/8) : (i+1)*(idx.dimensions/8)]
			for j := 0; j < len(quant); j += 8 {
				distance += bits.OnesCount64(binary.LittleEndian.Uint64(quant[j:]) ^ binary.LittleEndian.Uint64(v[j:]))
			}
			score = -float64(distance)
		}
		h := candidate{position: i, score: score, id: idx.IDs[i]}
		if hits.Len() < limit {
			heap.Push(hits, h)
			continue
		}
		if compare(h, (*hits)[0]) < 0 {
			(*hits)[0] = h
			heap.Fix(hits, 0)
		}
	}
	out := make([]vector.Hit, 0, hits.Len())
	for _, h := range *hits {
		raw := idx.raw[h.position*idx.dimensions*4 : (h.position+1)*idx.dimensions*4]
		var dot, qn, vn float64
		for j, x := range q {
			v := float64(math.Float32frombits(binary.LittleEndian.Uint32(raw[j*4:])))
			dot += float64(x) * v
			qn += float64(x) * float64(x)
			vn += v * v
		}
		out = append(out, vector.Hit{ID: idx.IDs[h.position], Score: dot / math.Sqrt(qn*vn)})
	}
	return vector.Top(out, k), nil
}

type candidate struct {
	position int
	score    float64
	id       string
}

func compare(a, b candidate) int {
	if a.score > b.score {
		return -1
	}
	if a.score < b.score {
		return 1
	}
	if a.id < b.id {
		return -1
	}
	if a.id > b.id {
		return 1
	}
	return 0
}

func (idx *Index) Len() int { return len(idx.IDs) }

type candidateHeap []candidate

func (h candidateHeap) Len() int           { return len(h) }
func (h candidateHeap) Less(i, j int) bool { return compare(h[i], h[j]) > 0 }
func (h candidateHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *candidateHeap) Push(v any)        { *h = append(*h, v.(candidate)) }
func (h *candidateHeap) Pop() any          { a := *h; v := a[len(a)-1]; *h = a[:len(a)-1]; return v }

func validateDimensions(v []float32, dimensions int) error {
	if len(v) != dimensions {
		return fmt.Errorf("vector dimensions: %d", len(v))
	}
	var n float64
	for _, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return fmt.Errorf("nonfinite vector")
		}
		n += float64(x) * float64(x)
	}
	if n == 0 {
		return fmt.Errorf("zero vector")
	}
	return nil
}

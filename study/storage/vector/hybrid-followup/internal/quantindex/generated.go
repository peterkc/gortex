package quantindex

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
)

// SyntheticVector has no dependence on a process-global random source.
func SyntheticVector(seed int64, position, dimensions int) []float32 {
	rng := rand.New(rand.NewSource(seed + int64(position)*104729))
	v := make([]float32, dimensions)
	var sum float64
	for i := range v {
		v[i] = rng.Float32()*2 - 1
		sum += float64(v[i]) * float64(v[i])
	}
	scale := float32(1 / math.Sqrt(sum))
	for i := range v {
		v[i] *= scale
	}
	return v
}

// BuildGenerated streams two reproducible passes instead of retaining a 4GB
// source matrix. It writes the same v02 layout as BuildDimensions.
func BuildGenerated(ctx context.Context, path string, n, dimensions int, seed int64) error {
	if n < 1 || n > 1000000 || (dimensions != 384 && dimensions != 1024) {
		return fmt.Errorf("unsupported synthetic size/dimensions")
	}
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("synthetic-%012d", i)
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
	if _, err = w.WriteString("QINT8v02"); err != nil {
		return err
	}
	for _, value := range []uint64{uint64(n), uint64(len(meta)), uint64(dimensions)} {
		if err = binary.Write(w, binary.LittleEndian, value); err != nil {
			return err
		}
	}
	if _, err = w.Write(meta); err != nil {
		return err
	}
	for i := range n {
		if i%1024 == 0 {
			if err = ctx.Err(); err != nil {
				return err
			}
		}
		if _, err = w.Write(quantize(SyntheticVector(seed, i, dimensions))); err != nil {
			return err
		}
	}
	raw := make([]byte, dimensions*4)
	for i := range n {
		if i%1024 == 0 {
			if err = ctx.Err(); err != nil {
				return err
			}
		}
		v := SyntheticVector(seed, i, dimensions)
		for j, x := range v {
			binary.LittleEndian.PutUint32(raw[j*4:], math.Float32bits(x))
		}
		if _, err = w.Write(raw); err != nil {
			return err
		}
	}
	if err = w.Flush(); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, 0444)
}

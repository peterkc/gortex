package runner

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"gxstudy/workload"
	"io"
	"iter"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type sampleDirectoryKey struct{}
type storedSample struct {
	Start, End, PinStart, PinEnd time.Time
	PinNS                        int64
	NS                           int64
	Check                        workload.Check
	Pending                      *pendingCheck
	NewVersion                   bool
	Error                        string
}
type sampleStream struct {
	memory []readerSample
	files  []string
}

func (s sampleStream) rows() iter.Seq2[readerSample, error] {
	return func(yield func(readerSample, error) bool) {
		for _, sample := range s.memory {
			if !yield(sample, sample.err) {
				return
			}
		}
		for _, path := range s.files {
			f, err := os.Open(path)
			if err != nil {
				yield(readerSample{}, err)
				return
			}
			dec := json.NewDecoder(bufio.NewReaderSize(f, 64<<10))
			for {
				var row storedSample
				err := dec.Decode(&row)
				if err == io.EOF {
					break
				}
				if err != nil {
					f.Close()
					yield(readerSample{}, err)
					return
				}
				sample := readerSample{start: row.Start, end: row.End, pinStart: row.PinStart, pinEnd: row.PinEnd, pinNS: row.PinNS, ns: row.NS, check: row.Check, pending: row.Pending, newVersion: row.NewVersion}
				if row.Error != "" {
					sample.err = errors.New(row.Error)
				}
				if !yield(sample, sample.err) {
					f.Close()
					return
				}
			}
			if err = f.Close(); err != nil {
				yield(readerSample{}, err)
				return
			}
		}
	}
}
func finishReaderStream(c *readerControl) (sampleStream, error) {
	close(c.stop)
	c.wg.Wait()
	var result sampleStream
	for i := 0; i < 8; i++ {
		result.memory = append(result.memory, (<-c.out)...)
		if path := <-c.files; path != "" {
			result.files = append(result.files, path)
		}
	}
	for _, sample := range result.memory {
		if sample.err != nil {
			return result, sample.err
		}
	}
	return result, nil
}

type diskDistribution struct {
	file  *os.File
	count int
}

func newDiskDistribution(dir, name string) (*diskDistribution, error) {
	f, err := os.CreateTemp(dir, name+"-*.ns")
	if err != nil {
		return nil, err
	}
	return &diskDistribution{file: f}, nil
}
func (d *diskDistribution) add(ns int64) error {
	if _, err := fmt.Fprintln(d.file, ns); err != nil {
		return err
	}
	d.count++
	return nil
}
func (d *diskDistribution) finish() (Distribution, error) {
	path := d.file.Name()
	if err := d.file.Close(); err != nil {
		d.file = nil
		return Distribution{}, err
	}
	d.file = nil
	if d.count == 0 {
		return Distribution{}, nil
	}
	sorted := path + ".sorted"
	cmd := exec.Command("sort", "-n", "-S", "4M", "-T", filepath.Dir(path), "-o", sorted, path)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	if output, err := cmd.CombinedOutput(); err != nil {
		return Distribution{}, fmt.Errorf("sort samples: %w: %s", err, output)
	}
	f, err := os.Open(sorted)
	if err != nil {
		return Distribution{}, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	out := Distribution{Count: d.count}
	sum := 0.
	points := []int{max(1, int(math.Ceil(float64(d.count)*.5))), max(1, int(math.Ceil(float64(d.count)*.95))), max(1, int(math.Ceil(float64(d.count)*.99)))}
	for i := 1; i <= d.count; i++ {
		line, err := r.ReadString('\n')
		if err != nil {
			return Distribution{}, err
		}
		ns, err := strconv.ParseInt(strings.TrimSpace(line), 10, 64)
		if err != nil {
			return Distribution{}, err
		}
		sum += float64(ns)
		if i == points[0] {
			out.P50 = float64(ns)
		}
		if i == points[1] {
			out.P95 = float64(ns)
		}
		if i == points[2] {
			out.P99 = float64(ns)
		}
	}
	out.Mean = sum / float64(d.count)
	return out, nil
}

func (d *diskDistribution) close() error {
	if d.file == nil {
		return nil
	}
	err := d.file.Close()
	d.file = nil
	return err
}

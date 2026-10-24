package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	usearch "github.com/unum-cloud/usearch/golang"
	"hybridfollowup/internal/quantindex"
)

type engine struct {
	Options    options
	Data       metadata
	Native     *usearch.Index
	Flat       *quantindex.Index
	Original   *matrix
	IDs        map[string]int
	NativePath string
	Norms      []float64
}

func openEngine(d metadata, o options) (*engine, error) {
	e := &engine{Options: o, Data: d, IDs: map[string]int{}}
	for i, row := range d.Documents {
		e.IDs[row.ID] = i
	}
	fail := func(err error) (*engine, error) { e.Close(); return nil, err }
	if o.Method == "exact" {
		var err error
		e.Original, err = openMatrix(filepath.Join(d.Directory, "document.f32"), len(d.Documents))
		if err != nil {
			return fail(err)
		}
		e.Norms, err = e.Original.Norms()
		if err != nil {
			return fail(err)
		}
		return e, nil
	}
	if o.Method == "int8" {
		var err error
		e.Flat, err = quantindex.Open(filepath.Join(d.Directory, "int8.bin"), "int8", 128)
		if err != nil {
			return fail(err)
		}
		if e.Flat.Len() != len(d.Documents) {
			return fail(fmt.Errorf("int8 count mismatch"))
		}
		return e, nil
	}
	if usearch.Version() != "2.26.5" {
		return fail(fmt.Errorf("native USearch version mismatch"))
	}
	path := o.Index
	if path == "" {
		path = filepath.Join(nativeDirectory(d, o), "index.bin")
	}
	e.NativePath = path
	var build struct {
		SHA      string `json:"file_sha256"`
		InputSHA string `json:"input_receipt_sha256"`
	}
	if err := readJSON(filepath.Join(filepath.Dir(path), "build.json"), &build); err != nil {
		return fail(err)
	}
	sha, err := fileSHA(path)
	if err != nil {
		return fail(err)
	}
	if sha != build.SHA || build.InputSHA != d.ReceiptSHA {
		return fail(fmt.Errorf("native file or input receipt mismatch"))
	}
	meta, err := usearch.Metadata(path)
	if err != nil {
		return fail(err)
	}
	expected := usearch.F32
	if o.Method == "i8" {
		expected = usearch.I8
	}
	if meta.Quantization != expected || meta.Metric != usearch.Cosine || meta.Dimensions != dimensions {
		return fail(fmt.Errorf("saved native metric/scalar mismatch"))
	}
	cfg := usearch.DefaultConfig(dimensions)
	e.Native, err = usearch.NewIndex(cfg)
	if err != nil {
		return fail(err)
	}
	if err = e.Native.View(path); err != nil {
		return fail(err)
	}
	count, err := e.Native.Len()
	if err != nil {
		return fail(err)
	}
	if int(count) != len(d.Documents) {
		return fail(fmt.Errorf("native corpus count mismatch"))
	}
	if err = e.Native.ChangeExpansionSearch(uint(o.Ef)); err != nil {
		return fail(err)
	}
	if err = e.Native.ChangeThreadsSearch(1); err != nil {
		return fail(err)
	}
	if o.Method == "i8" || (o.Method == "f32" && o.Phase == "query") {
		e.Original, err = openMatrix(filepath.Join(d.Directory, "document.f32"), len(d.Documents))
		if err != nil {
			return fail(err)
		}
	}
	return e, nil
}
func (e *engine) Close() {
	if e.Native != nil {
		e.Native.Destroy()
	}
	if e.Flat != nil {
		e.Flat.Close()
	}
	if e.Original != nil {
		e.Original.Close()
	}
}
func (e *engine) Search(q query) ([]hit, error) {
	if e.Options.Method == "exact" {
		var best []hit
		for position := range e.Data.Documents {
			best = keepTop10(best, hit{position, e.Original.ExactCosine(q, position, e.Norms)}, e.Data.Documents)
		}
		return best, nil
	}
	if e.Flat != nil {
		hits, err := e.Flat.SearchWorkers(context.Background(), q.Values, 128, e.Options.Workers)
		if err != nil {
			return nil, err
		}
		out := make([]hit, 0, len(hits))
		for _, h := range hits {
			position, ok := e.IDs[h.ID]
			if !ok {
				return nil, fmt.Errorf("unknown scan ID")
			}
			out = append(out, hit{position, h.Score})
		}
		return out, nil
	}
	k := 10
	if e.Options.Method == "i8" {
		k = e.Options.K
	}
	keys, distances, err := e.Native.Search(q.Values, uint(k))
	if err != nil {
		return nil, err
	}
	out := make([]hit, 0, len(keys))
	for i, key := range keys {
		position, ok := e.Data.KeyToPosition[key]
		if !ok {
			return nil, fmt.Errorf("unknown native key")
		}
		score := 1 - float64(distances[i])
		if e.Options.Method == "i8" {
			score = e.Original.Cosine(q, position)
		}
		out = append(out, hit{position, score})
	}
	if e.Options.Method == "i8" {
		slices.SortFunc(out, func(a, b hit) int { return compareHit(a, b, e.Data.Documents) })
	}
	return out, nil
}
func (e *engine) OriginalScores(q query, hits []hit) ([]hit, error) {
	if e.Options.Method != "f32" {
		return slices.Clone(hits), nil
	}
	out := make([]hit, len(hits))
	for i, h := range hits {
		out[i] = hit{h.Position, e.Original.Cosine(q, h.Position)}
	}
	return out, nil
}
func (e *engine) TaskPositions(q query, hits []hit) ([]int, error) {
	if e.Options.Method != "f32" {
		return positions(hits), nil
	}
	keys, _, err := e.Native.Search(q.Values, 200)
	if err != nil {
		return nil, err
	}
	out := make([]int, 0, len(keys))
	for _, key := range keys {
		position, ok := e.Data.KeyToPosition[key]
		if !ok {
			return nil, fmt.Errorf("unknown task key")
		}
		out = append(out, position)
	}
	return out, nil
}
func rssBytes() (int64, error) {
	output, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0, err
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
	return value * 1024, err
}

func (e *engine) FileComponents() (map[string]int64, error) {
	out := map[string]int64{}
	paths := map[string]string{"id_metadata": filepath.Join(e.Data.Directory, "records.json")}
	switch e.Options.Method {
	case "exact":
		paths["original_f32"] = filepath.Join(e.Data.Directory, "document.f32")
	case "int8":
		paths["int8_with_original_f32"] = filepath.Join(e.Data.Directory, "int8.bin")
	case "f32":
		paths["native_f32"] = e.NativePath
	case "i8":
		paths["native_i8"] = e.NativePath
		paths["required_original_f32"] = filepath.Join(e.Data.Directory, "document.f32")
	}
	for name, path := range paths {
		st, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		out[name] = st.Size()
	}
	return out, nil
}

package runner

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"gxstudy/oracle"
	"gxstudy/workload"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func makeDiskChange(d dataset.Dataset) (change adapter.FileChange, err error) {
	scratch, err := os.MkdirTemp("", "gxstudy-delta-")
	if err != nil {
		return change, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(scratch)) }()
	f, err := os.Create(filepath.Join(scratch, "paths"))
	if err != nil {
		return change, err
	}
	for n, e := range d.NodeRows() {
		if e != nil {
			f.Close()
			return change, e
		}
		if n.Language != "go" || n.FilePath == "" || n.StartLine <= 0 || (n.Kind != "function" && n.Kind != "method") {
			continue
		}
		encoded, e := json.Marshal(n.FilePath)
		if e != nil {
			f.Close()
			return change, e
		}
		hash := sha256.Sum256([]byte(n.FilePath))
		if _, e = fmt.Fprintf(f, "%x %s\n", hash, encoded); e != nil {
			f.Close()
			return change, e
		}
	}
	if err = f.Close(); err != nil {
		return change, err
	}
	sorted := filepath.Join(scratch, "sorted")
	cmd := exec.Command("sort", "-S", "4M", "-T", scratch, "-o", sorted, f.Name())
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	if output, e := cmd.CombinedOutput(); e != nil {
		return change, fmt.Errorf("sort delta files: %w: %s", e, output)
	}
	input, err := os.Open(sorted)
	if err != nil {
		return change, err
	}
	defer input.Close()
	reader := bufio.NewReader(input)
	previous, path := "", ""
	for {
		line, e := reader.ReadString('\n')
		if e == io.EOF {
			break
		}
		if e != nil {
			return change, e
		}
		if line == previous {
			if e = json.Unmarshal([]byte(strings.TrimSpace(line[65:])), &path); e != nil {
				return change, e
			}
			break
		}
		previous = line
	}
	if path == "" {
		return change, fmt.Errorf("delta requires a Go source file with two functions")
	}
	subset := dataset.Dataset{Repo: d.Repo, Meta: d.Meta}
	owned := map[string]bool{}
	for n, e := range d.NodeRows() {
		if e != nil {
			return change, e
		}
		if n.FilePath == path {
			subset.Nodes = append(subset.Nodes, n)
			owned[n.ID] = true
		}
	}
	for v, e := range d.EdgeRows() {
		if e != nil {
			return change, e
		}
		if v.FilePath == path || owned[v.From] {
			subset.Edges = append(subset.Edges, v)
		}
	}
	for v, e := range d.FileRows() {
		if e != nil {
			return change, e
		}
		if v.Path == path {
			subset.Files = append(subset.Files, v)
		}
	}
	for v, e := range d.SymbolRows() {
		if e != nil {
			return change, e
		}
		if owned[v.NodeID] {
			subset.Symbols = append(subset.Symbols, v)
		}
	}
	for v, e := range d.ContentRows() {
		if e != nil {
			return change, e
		}
		if v.FilePath == path {
			subset.Content = append(subset.Content, v)
		}
	}
	for v, e := range d.VectorRows() {
		if e != nil {
			return change, e
		}
		if owned[v.NodeID] {
			subset.Vectors = append(subset.Vectors, v)
		}
	}
	return MakeChange(subset)
}

type checkerContextKey struct{}

func checkedAnswer(ctx context.Context, q workload.Query, w workload.Expected, got workload.Answer, inputs []adapter.SearchInput) (workload.Check, error) {
	if len(w.Sources) == 0 {
		return compareRankedAnswer(q, w, got, inputs), nil
	}
	ids := make([]string, 0, len(got.Hits))
	for _, h := range got.Hits {
		ids = append(ids, h.ID)
	}
	for _, input := range inputs {
		for _, h := range input.Hits {
			ids = append(ids, h.ID)
		}
	}
	if client, ok := ctx.Value(checkerContextKey{}).(*processClient); ok {
		reply, err := client.call(ctx, ProcessRequest{Op: "check", Data: w.Sources, Query: q, Answer: got, Inputs: inputs})
		return reply.Check, err
	}
	expected, err := oracle.DiskReference(ctx, w.Sources, q, ids)
	if err != nil {
		return workload.Check{}, err
	}
	return compareRankedAnswer(q, expected, got, inputs), nil
}

// pendingCheck contains one bounded observation, never the source corpus. Real
// Expected values hold repeatable disk descriptors, not loaded records.
type pendingCheck struct {
	// Preserve runtime-only fields without changing the public query format.
	Sources       []dataset.Dataset
	NativeScoring bool
	MatchRepos    map[string]string
	Query         workload.Query
	Expected      workload.Expected
	Answer        workload.Answer
	Inputs        []adapter.SearchInput
	Warmup        bool
	Ready         *workload.Check
}

func (p pendingCheck) check(ctx context.Context) (workload.Check, error) {
	if p.Ready != nil {
		return *p.Ready, nil
	}
	query, expected := p.Query, p.Expected
	query.NativeScoring = p.NativeScoring
	query.MatchRepos = p.MatchRepos
	expected.Sources = p.Sources
	return checkedAnswer(ctx, query, expected, p.Answer, p.Inputs)
}
func checkReaderSample(ctx context.Context, sample readerSample) (readerSample, error) {
	if sample.pending == nil {
		return sample, sample.err
	}
	check, err := sample.pending.check(ctx)
	sample.check = check
	sample.pending = nil
	if err == nil && check.Mismatch {
		err = fmt.Errorf("reader check %s: %s", check.QueryID, check.Detail)
	}
	sample.err = errors.Join(sample.err, err)
	return sample, sample.err
}

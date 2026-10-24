package main

import (
	"context"
	"flag"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"gxstudy/oracle"
	"gxstudy/workload"
	"math"
	"os"
	"path/filepath"
	"strings"
)

func run() error {
	source := flag.String("source", "", "single exported corpus")
	sources := flag.String("sources", "", "comma-separated exported real repositories; extra slots are labelled derived")
	out := flag.String("out", "", "tier directory")
	count := flag.Int("repos", 1, "repository count")
	seed := flag.Int64("seed", 20261022, "deterministic seed")
	flag.Parse()
	if *count < 1 || *count > 100 || *out == "" {
		return fmt.Errorf("-repos must be 1..100 and -out is required")
	}
	dirs := []string{*source}
	real := false
	if *sources != "" {
		dirs = strings.Split(*sources, ",")
		real = true
	}
	var bases []dataset.Dataset
	for _, dir := range dirs {
		d, e := dataset.Load(dir)
		if e != nil {
			return e
		}
		bases = append(bases, d)
	}
	if real && len(bases) > *count {
		return fmt.Errorf("more input repositories than slots")
	}
	ds := make([]dataset.Dataset, *count)
	ids := make([]string, *count)
	seen := map[string]bool{}
	for i := range ds {
		fraction := .02 + .98/math.Pow(float64(i+1), 1.15)
		base := bases[i%len(bases)]
		repo := fmt.Sprintf("r%03d", i)
		if real && i < len(bases) {
			repo = base.Repo
			fraction = 1
		}
		if seen[repo] || strings.ContainsAny(repo, "/\\") || repo == ".." || repo == "" {
			return fmt.Errorf("invalid or duplicate repository id %q", repo)
		}
		seen[repo] = true
		d := dataset.Derive(base, repo, fraction, *seed)
		if real && i < len(bases) {
			d.Meta.Origin = "real"
			d.Meta.ParentRepo = ""
		}
		ds[i] = d
		ids[i] = repo
	}
	dataset.LinkCrossRepo(ds)
	segments := map[string]adapter.Segment{}
	for i, d := range ds {
		if _, e := dataset.Write(filepath.Join(*out, ids[i]), d); e != nil {
			return e
		}
		segments[d.Repo] = oracle.New(d)
	}
	qs := workload.Generate(ds, *seed)
	expected := make([]workload.Expected, 0, len(qs))
	multi := oracle.NewMulti(ds, segments)
	for _, q := range qs {
		a, e := workload.Evaluate(context.Background(), segments, multi, q)
		if e != nil {
			return e
		}
		expected = append(expected, workload.Expected{QueryID: q.ID, Answer: a})
	}
	if e := dataset.WriteJSON(filepath.Join(*out, "queries.json"), qs); e != nil {
		return e
	}
	if e := dataset.WriteJSON(filepath.Join(*out, "expected.json"), expected); e != nil {
		return e
	}
	meta := bases[0].Meta
	meta.Version = dataset.GeneratorVersion
	meta.Seed = *seed
	meta.Origin = "tier"
	meta.ParentRepo = ""
	meta.DerivedFraction = 0
	if len(ds) > 1 {
		meta.CrossRepoPolicy = dataset.CrossRepoPolicy
	}
	if len(bases) > 1 {
		meta.SourceRevision = "per-repository manifests"
		meta.SourceSHA256 = "per-repository manifests"
	}
	if e := dataset.TierManifest(*out, ids, meta); e != nil {
		return e
	}
	var m dataset.Manifest
	if e := dataset.ReadJSON(filepath.Join(*out, "manifest.json"), &m); e != nil {
		return e
	}
	fmt.Printf("%s: repos=%d queries=%d counts=%+v\n", *out, len(ids), len(qs), m.Counts)
	return nil
}
func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}

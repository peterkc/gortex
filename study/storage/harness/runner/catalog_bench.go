package runner

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"
)

type catalogReadRequest struct {
	Candidate, Directory string
	Expected             adapter.View
	Repos                []string
}
type catalogReadResult struct {
	Candidate string `json:"candidate"`
	Factory   string `json:"factory"`
	PID       int    `json:"pid"`
	NS        int64  `json:"ns"`
}

func catalogResolve(ctx context.Context, cat adapter.Catalog, repos []string, want adapter.View) error {
	view, release, err := cat.Pin(ctx, repos)
	if err != nil {
		return err
	}
	defer release()
	if !maps.Equal(view, want) {
		return fmt.Errorf("catalog resolved a different view")
	}
	return nil
}

func catalogReadChild(candidate string, factory adapter.CatalogFactory, path string) (err error) {
	var request catalogReadRequest
	if err = dataset.ReadJSON(path, &request); err != nil {
		return err
	}
	if request.Candidate != candidate || factory == nil {
		return fmt.Errorf("wrong recovery candidate or missing factory")
	}
	start := time.Now()
	cat, err := factory(request.Directory)
	if err != nil {
		return err
	}
	if cat == nil {
		return fmt.Errorf("factory returned no recovery catalog")
	}
	defer func() { err = errors.Join(err, cat.Close()) }()
	if err = cat.Recover(context.Background()); err != nil {
		return err
	}
	if err = catalogResolve(context.Background(), cat, request.Repos, request.Expected); err != nil {
		return err
	}
	result := catalogReadResult{candidate, fmt.Sprintf("%T", cat), os.Getpid(), time.Since(start).Nanoseconds()}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func catalogFixture(dir string, entries int, seed int64) (adapter.View, adapter.View, []string, map[string]string, error) {
	views := []adapter.View{{}, {}}
	var repos []string
	hashes := map[string]string{}
	for i := range entries {
		repo := fmt.Sprintf("repo-%03d", i)
		repos = append(repos, repo)
		for version, view := range views {
			leaf := filepath.Join(dir, fmt.Sprintf("%s-v%d", repo, version))
			if err := os.MkdirAll(leaf, 0755); err != nil {
				return nil, nil, nil, nil, err
			}
			manifest := filepath.Join(leaf, "manifest.json")
			if err := dataset.WriteJSON(manifest, map[string]any{"seed": seed, "repo": repo, "version": version}); err != nil {
				return nil, nil, nil, nil, err
			}
			sha, err := dataset.Hash(manifest)
			if err != nil {
				return nil, nil, nil, nil, err
			}
			if err = os.WriteFile(filepath.Join(leaf, "SEALED"), []byte(sha+"\n"), 0444); err != nil {
				return nil, nil, nil, nil, err
			}
			view[repo] = adapter.SegmentRef{Repo: repo, Version: sha, Digest: sha, Path: leaf}
			for _, name := range []string{"manifest.json", "SEALED"} {
				path := filepath.Join(leaf, name)
				hash, err := dataset.Hash(path)
				if err != nil {
					return nil, nil, nil, nil, err
				}
				relative := filepath.Join(fmt.Sprintf("%s-v%d", repo, version), name)
				hashes[relative] = hash
			}
		}
	}
	return views[0], views[1], repos, hashes, nil
}

func catalogFootprint(dir string) (map[string]int64, error) {
	var bytes, files int64
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		info, err := entry.Info()
		if err == nil {
			bytes += info.Size()
			files++
		}
		return err
	})
	return map[string]int64{"bytes": bytes, "files": files}, err
}

func catalogTrial(candidate string, factory adapter.CatalogFactory, old, next adapter.View, repos []string, dir string) (samples map[string]int64, recovery catalogReadResult, footprint map[string]int64, err error) {
	ctx := context.Background()
	catDir := filepath.Join(dir, "catalog")
	cat, err := factory(catDir)
	if err != nil {
		return nil, recovery, nil, err
	}
	if cat == nil {
		return nil, recovery, nil, fmt.Errorf("factory returned no catalog")
	}
	defer func() {
		if cat != nil {
			err = errors.Join(err, cat.Close())
		}
	}()
	if err = cat.PublishSet(ctx, old, nil); err != nil {
		return nil, recovery, nil, err
	}
	samples = map[string]int64{}
	prior := old[repos[0]]
	start := time.Now()
	if err = cat.Publish(ctx, repos[0], next[repos[0]], &prior); err != nil {
		return nil, recovery, nil, err
	}
	samples["publish"] = time.Since(start).Nanoseconds()
	mixed := maps.Clone(old)
	mixed[repos[0]] = next[repos[0]]
	start = time.Now()
	if err = cat.PublishSet(ctx, next, mixed); err != nil {
		return nil, recovery, nil, err
	}
	samples["publish_set"] = time.Since(start).Nanoseconds()
	start = time.Now()
	if err = catalogResolve(ctx, cat, repos, next); err != nil {
		return nil, recovery, nil, err
	}
	samples["pin_resolve"] = time.Since(start).Nanoseconds()
	closing := cat
	cat = nil
	if err = closing.Close(); err != nil {
		return nil, recovery, nil, err
	}
	request := filepath.Join(dir, "recover-request.json")
	if err = dataset.WriteJSON(request, catalogReadRequest{candidate, catDir, next, repos}); err != nil {
		return nil, recovery, nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, recovery, nil, err
	}
	cmd := exec.CommandContext(ctx, exe, "__catalog_bench_read", request)
	cmd.Stderr = os.Stderr
	output, err := cmd.Output()
	if err != nil {
		return nil, recovery, nil, err
	}
	if err = json.Unmarshal(output, &recovery); err != nil {
		return nil, recovery, nil, err
	}
	if recovery.Candidate != candidate || recovery.PID <= 0 || recovery.PID == os.Getpid() || recovery.NS <= 0 {
		return nil, recovery, nil, fmt.Errorf("recovery did not invoke the selected child candidate")
	}
	samples["reopen_recover_pin_resolve"] = recovery.NS
	footprint, err = catalogFootprint(catDir)
	return samples, recovery, footprint, err
}

// RunCatalogBenchmark uses the binary's injected factory, never its serving backend.
func RunCatalogBenchmark(b adapter.Backend, factory adapter.CatalogFactory, args []string) error {
	flags := flag.NewFlagSet("catalog-bench", flag.ContinueOnError)
	candidate := flags.String("candidate", "", "exact catalog candidate identity")
	out := flags.String("out", "", "fresh output directory")
	entries := flags.Int("entries", 1, "1, 10 or 100 repository entries")
	repeats := flags.Int("repeats", 30, "1..30; study acceptance requires 30")
	seed := flags.Int64("seed", 20261025, "placeholder manifest seed")
	if err := flags.Parse(args); err != nil {
		return err
	}
	allowed := []string{"catalog-sqlite", "catalog-refs", "catalog-bbolt", "catalog-pebble", "catalog-manifest"}
	if !slices.Contains(allowed, *candidate) || b.Name() != *candidate || factory == nil {
		return fmt.Errorf("wrong candidate or missing catalog factory")
	}
	if *out == "" || !slices.Contains([]int{1, 10, 100}, *entries) || *repeats < 1 || *repeats > 30 {
		return fmt.Errorf("fresh out, entries 1/10/100 and repeats 1..30 required")
	}
	absolute, err := filepath.Abs(*out)
	if err != nil {
		return err
	}
	if err = os.Mkdir(absolute, 0755); err != nil {
		return err
	}
	old, next, repos, hashes, err := catalogFixture(filepath.Join(absolute, "fixtures"), *entries, *seed)
	if err != nil {
		return err
	}
	metrics := map[string][]int64{}
	var recoveries []catalogReadResult
	var footprints []map[string]int64
	for repeat := range *repeats {
		dir := filepath.Join(absolute, fmt.Sprintf("trial-%02d", repeat))
		if err = os.Mkdir(dir, 0755); err != nil {
			return err
		}
		values, recovery, footprint, err := catalogTrial(*candidate, factory, old, next, repos, dir)
		if err != nil {
			return err
		}
		for name, ns := range values {
			metrics[name] = append(metrics[name], ns)
		}
		recoveries = append(recoveries, recovery)
		footprints = append(footprints, footprint)
	}
	distributions := map[string]Distribution{}
	for name, samples := range metrics {
		distributions[name] = Summarize(samples)
	}
	return dataset.WriteJSON(filepath.Join(absolute, "result.json"), map[string]any{
		"success": true, "candidate": *candidate, "entries": *entries, "repeats": *repeats, "seed": *seed,
		"fixture_sha256": hashes, "latency": distributions, "recoveries": recoveries, "footprints": footprints,
		"measurement_point": "per trial, after recovery child closed; catalog directory only; fixtures excluded",
		"reopen_timing":     "fresh process factory+Recover+Pin+view resolution+release; process startup and Close excluded",
	})
}

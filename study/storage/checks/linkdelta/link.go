package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"gxstudy/dataset"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
)

type symbolKey struct {
	Repo, Scope, Qualified, Kind string
	Start, End, Ordinal          int
}
type declaration struct {
	ID  string
	Key symbolKey
}
type fact struct {
	Package    string
	Target     symbolKey
	Edge       dataset.Edge
	Unresolved bool
}
type linkResult struct {
	Facts, Resolved, Missing, Ambiguous int
	UnresolvedKept                      int `json:"unresolved_kept"`
	Status, Branch                      string
	WitnessShare                        float64
}
type collisionStats struct{ NameScopeGroups, ExtendedGroups, NameScopeFacts, ExtendedFacts int }

func targetKey(repo string, n dataset.Node) symbolKey {
	q := n.QualName
	if q == "" {
		q = n.Name
	}
	return symbolKey{Repo: repo, Scope: n.FilePath, Qualified: q, Kind: n.Kind, Start: n.StartLine, End: n.EndLine}
}
func hashJSON(v any) string { b, _ := json.Marshal(v); return fmt.Sprintf("%x", sha256.Sum256(b)) }
func prepareLink(ctx context.Context, ds []dataset.Dataset, dir string) error {
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	// This ID-to-projected-declaration join belongs to the separately capped
	// preparation checker, never to the measured link worker. Ordinals are per
	// file preparation identities, not hashes/copies of target IDs in facts.
	targets := map[string]declaration{}
	ordinals := map[string]int{}
	original, extended := map[string]int{}, map[string]int{}
	err := writeJSONLines(filepath.Join(dir, "declarations.jsonl"), func(enc *json.Encoder) error {
		for _, d := range ds {
			for n, e := range d.NodeRows() {
				if e != nil {
					return e
				}
				if e = ctx.Err(); e != nil {
					return e
				}
				if _, ok := targets[n.ID]; n.ID == "" || ok {
					return fmt.Errorf("duplicate declaration ID")
				}
				key := targetKey(d.Repo, n)
				original[lookup(key.Repo, key.Scope, key.Qualified)]++
				extended[hashJSON(key)]++
				file := lookup(d.Repo, n.FilePath)
				ordinals[file]++
				key.Ordinal = ordinals[file]
				v := declaration{n.ID, key}
				targets[n.ID] = v
				if e = enc.Encode(v); e != nil {
					return e
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	stats := collisionStats{}
	for _, n := range original {
		if n > 1 {
			stats.NameScopeGroups++
		}
	}
	for _, n := range extended {
		if n > 1 {
			stats.ExtendedGroups++
		}
	}
	projected, err := projectFacts(ctx, ds, dir, targets, original, extended)
	if err != nil {
		return err
	}
	stats.NameScopeFacts = projected.NameScopeFacts
	stats.ExtendedFacts = projected.ExtendedFacts
	if err = dataset.WriteJSON(filepath.Join(dir, "collisions.json"), stats); err != nil {
		return err
	}
	for _, kind := range []string{"facts", "declarations"} {
		src := filepath.Join(dir, "package-"+kind+".unsorted")
		sorted := filepath.Join(dir, "package-"+kind+".sorted")
		args := []string{"-S", "4M", "-T", dir, "-o", sorted}
		if kind == "declarations" {
			args = append(args, "-u")
		}
		args = append(args, src)
		cmd := exec.CommandContext(ctx, "sort", args...)
		cmd.Env = append(os.Environ(), "LC_ALL=C")
		if raw, e := cmd.CombinedOutput(); e != nil {
			return fmt.Errorf("partition sort: %w: %s", e, raw)
		}
		if err = partitionLines(sorted, dir, kind); err != nil {
			return err
		}
		if err = os.Remove(src); err != nil {
			return err
		}
		if err = os.Remove(sorted); err != nil {
			return err
		}
	}
	return nil
}
func projectFacts(ctx context.Context, ds []dataset.Dataset, dir string, targets map[string]declaration, original, extended map[string]int) (stats collisionStats, err error) {
	expected, err := os.Create(filepath.Join(dir, "expected.jsonl"))
	if err != nil {
		return stats, err
	}
	ew := bufio.NewWriterSize(expected, 64<<10)
	defer func() { err = errors.Join(err, ew.Flush(), expected.Sync(), expected.Close()) }()
	want := json.NewEncoder(ew)
	output, err := os.Create(filepath.Join(dir, "facts.jsonl"))
	if err != nil {
		return stats, err
	}
	fw := bufio.NewWriterSize(output, 64<<10)
	defer func() { err = errors.Join(err, fw.Flush(), output.Sync(), output.Close()) }()
	enc := json.NewEncoder(fw)
	facts, err := os.Create(filepath.Join(dir, "package-facts.unsorted"))
	if err != nil {
		return stats, err
	}
	defer func() { err = errors.Join(err, facts.Sync(), facts.Close()) }()
	decls, err := os.Create(filepath.Join(dir, "package-declarations.unsorted"))
	if err != nil {
		return stats, err
	}
	defer func() { err = errors.Join(err, decls.Sync(), decls.Close()) }()
	for _, d := range ds {
		for edge, e := range d.EdgeRows() {
			if e != nil {
				return stats, e
			}
			if e = ctx.Err(); e != nil {
				return stats, e
			}
			if e = want.Encode(edge); e != nil {
				return stats, e
			}
			v, ok := targets[edge.To]
			f := fact{Package: d.Repo + ":" + path.Dir(edge.FilePath), Target: v.Key, Edge: edge, Unresolved: !ok}
			if ok {
				f.Edge.To = ""
				key := v.Key
				key.Ordinal = 0
				if original[lookup(key.Repo, key.Scope, key.Qualified)] > 1 {
					stats.NameScopeFacts++
				}
				if extended[hashJSON(key)] > 1 {
					stats.ExtendedFacts++
				}
			}
			if e = enc.Encode(f); e != nil {
				return stats, e
			}
			pkg := hashJSON(f.Package)
			raw, e := json.Marshal(f)
			if e != nil {
				return stats, e
			}
			if _, e = fmt.Fprintf(facts, "%s\t%s\n", pkg, raw); e != nil {
				return stats, e
			}
			if ok {
				raw, e = json.Marshal(v)
				if e != nil {
					return stats, e
				}
				if _, e = fmt.Fprintf(decls, "%s\t%s\n", pkg, raw); e != nil {
					return stats, e
				}
			}
		}
	}
	return stats, nil
}
func writeJSONLines(path string, write func(*json.Encoder) error) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	w := bufio.NewWriterSize(f, 64<<10)
	if err = write(json.NewEncoder(w)); err != nil {
		return err
	}
	return errors.Join(w.Flush(), f.Sync())
}
func partitionLines(sorted, dir, kind string) (err error) {
	f, err := os.Open(sorted)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	r := bufio.NewReaderSize(f, 64<<10)
	var out *os.File
	var pkg string
	defer func() {
		if out != nil {
			err = errors.Join(err, out.Close())
		}
	}()
	for {
		line, e := r.ReadString('\n')
		if e == io.EOF && line == "" {
			return nil
		}
		if e != nil {
			return e
		}
		if len(line) < 66 || line[64] != '\t' || !digest(line[:64]) {
			return fmt.Errorf("invalid package spool")
		}
		if line[:64] != pkg {
			if out != nil {
				if e = out.Close(); e != nil {
					return e
				}
				out = nil
			}
			pkg = line[:64]
			p := filepath.Join(dir, "packages", pkg)
			if e = os.MkdirAll(p, 0700); e != nil {
				return e
			}
			out, e = os.OpenFile(filepath.Join(p, kind+".jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if e != nil {
				return e
			}
		}
		if _, e = io.WriteString(out, line[65:]); e != nil {
			return e
		}
	}
}
func eachJSON[T any](path string, visit func(T) error) (err error) {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	dec := json.NewDecoder(bufio.NewReaderSize(f, 64<<10))
	for {
		var v T
		e := dec.Decode(&v)
		if e == io.EOF {
			return nil
		}
		if e != nil {
			return e
		}
		if e = visit(v); e != nil {
			return e
		}
	}
}
func resolveFacts(ctx context.Context, dir, out, mode string) (result linkResult, err error) {
	if mode != "global" && mode != "package" {
		return result, fmt.Errorf("invalid link mode")
	}
	if err = os.Mkdir(out, 0700); err != nil {
		return result, err
	}
	packages := []string{dir}
	if mode == "package" {
		entries, e := os.ReadDir(filepath.Join(dir, "packages"))
		if e != nil {
			return result, e
		}
		packages = nil
		for _, v := range entries {
			if !v.IsDir() || !digest(v.Name()) {
				return result, fmt.Errorf("invalid package partition")
			}
			packages = append(packages, filepath.Join(dir, "packages", v.Name()))
		}
	}
	edges, err := os.Create(filepath.Join(out, "edges.jsonl"))
	if err != nil {
		return result, err
	}
	ew := bufio.NewWriterSize(edges, 64<<10)
	defer func() { err = errors.Join(err, ew.Flush(), edges.Sync(), edges.Close()) }()
	enc := json.NewEncoder(ew)
	witnesses, err := os.Create(filepath.Join(out, "witnesses.jsonl"))
	if err != nil {
		return result, err
	}
	ww := bufio.NewWriterSize(witnesses, 64<<10)
	defer func() { err = errors.Join(err, ww.Flush(), witnesses.Sync(), witnesses.Close()) }()
	we := json.NewEncoder(ww)
	for _, pkg := range packages {
		table := map[symbolKey][]string{}
		e := eachJSON(filepath.Join(pkg, "declarations.jsonl"), func(d declaration) error { table[d.Key] = append(table[d.Key], d.ID); return ctx.Err() })
		if e != nil && !(mode == "package" && os.IsNotExist(e)) {
			return result, e
		}
		if e = eachJSON(filepath.Join(pkg, "facts.jsonl"), func(f fact) error {
			if e := ctx.Err(); e != nil {
				return e
			}
			result.Facts++
			if f.Unresolved {
				if f.Edge.To == "" {
					return fmt.Errorf("dangling fact lost raw target")
				}
				result.UnresolvedKept++
				return enc.Encode(f.Edge)
			}
			if f.Edge.To != "" {
				return fmt.Errorf("declared fact retains target ID")
			}
			ids := table[f.Target]
			if len(ids) != 1 {
				reason := "missing"
				if len(ids) > 1 {
					reason = "ambiguous"
					result.Ambiguous++
				} else {
					result.Missing++
				}
				return we.Encode(struct {
					Reason     string
					Fact       fact
					Candidates []string
				}{reason, f, ids})
			}
			f.Edge.To = ids[0]
			result.Resolved++
			return enc.Encode(f.Edge)
		}); e != nil {
			return result, e
		}
	}
	result.Status = "pass"
	if result.Resolved+result.UnresolvedKept != result.Facts {
		result.Status = "partial"
	}
	result.Branch = mode
	if result.Facts > 0 {
		result.WitnessShare = float64(result.Missing+result.Ambiguous) / float64(result.Facts)
	}
	return result, dataset.WriteJSON(filepath.Join(out, "result.json"), result)
}

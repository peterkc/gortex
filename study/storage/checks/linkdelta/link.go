package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"gxstudy/dataset"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
)

type symbolKey struct{ Repo, Scope, Qualified string }
type declaration struct {
	ID  string
	Key symbolKey
}
type fact struct {
	Package string
	Target  symbolKey
	Edge    dataset.Edge
}
type linkResult struct {
	Facts, Resolved, Missing, Ambiguous int
	Status, Branch                      string
	WitnessShare                        float64
}

func targetKey(repo string, n dataset.Node) symbolKey {
	q := n.QualName
	if q == "" {
		q = n.Name
	}
	return symbolKey{repo, n.FilePath, q}
}
func prepareLink(ctx context.Context, ds []dataset.Dataset, dir string) (err error) {
	if err = os.Mkdir(dir, 0700); err != nil {
		return err
	}
	// Preparation is a separately capped checker process; it is never link peak.
	targets := map[string]declaration{}
	decl, err := os.Create(filepath.Join(dir, "declarations.jsonl"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, decl.Close()) }()
	de := json.NewEncoder(decl)
	for _, d := range ds {
		for n, e := range d.NodeRows() {
			if e != nil {
				return e
			}
			if e = ctx.Err(); e != nil {
				return e
			}
			v := declaration{n.ID, targetKey(d.Repo, n)}
			if _, exists := targets[n.ID]; exists {
				return fmt.Errorf("duplicate declaration ID")
			}
			targets[n.ID] = v
			if e = de.Encode(v); e != nil {
				return e
			}
		}
	}
	facts, err := os.Create(filepath.Join(dir, "facts.jsonl"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, facts.Close()) }()
	enc := json.NewEncoder(facts)
	expected, err := os.Create(filepath.Join(dir, "expected.jsonl"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, expected.Close()) }()
	want := json.NewEncoder(expected)
	for _, d := range ds {
		for edge, e := range d.EdgeRows() {
			if e != nil {
				return e
			}
			if e = want.Encode(edge); e != nil {
				return e
			}
			decl, ok := targets[edge.To]
			key := symbolKey{Repo: d.Repo}
			if ok {
				key = decl.Key
			}
			edge.To = ""
			f := fact{Package: d.Repo + ":" + path.Dir(edge.FilePath), Target: key, Edge: edge}
			if e = enc.Encode(f); e != nil {
				return e
			}
		}
	}
	return nil
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
	edges, err := os.Create(filepath.Join(out, "edges.jsonl"))
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, edges.Close()) }()
	enc := json.NewEncoder(edges)
	witnesses, err := os.Create(filepath.Join(out, "witnesses.jsonl"))
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, witnesses.Close()) }()
	we := json.NewEncoder(witnesses)
	packages := []string{""}
	if mode == "package" {
		set := map[string]bool{}
		err = eachJSON(filepath.Join(dir, "facts.jsonl"), func(f fact) error { set[f.Package] = true; return nil })
		if err != nil {
			return result, err
		}
		packages = nil
		for p := range set {
			packages = append(packages, p)
		}
		slices.Sort(packages)
	}
	for _, pkg := range packages {
		// The fallback keeps only lookup keys consumed by one source package, but
		// scans declarations once per package. This trades CPU/I/O for bounded RAM.
		needed := map[symbolKey]bool{}
		if mode == "package" {
			err = eachJSON(filepath.Join(dir, "facts.jsonl"), func(f fact) error {
				if f.Package == pkg {
					needed[f.Target] = true
				}
				return nil
			})
			if err != nil {
				return result, err
			}
		}
		table := map[symbolKey][]string{}
		err = eachJSON(filepath.Join(dir, "declarations.jsonl"), func(d declaration) error {
			if mode == "global" || needed[d.Key] {
				table[d.Key] = append(table[d.Key], d.ID)
			}
			return ctx.Err()
		})
		if err != nil {
			return result, err
		}
		err = eachJSON(filepath.Join(dir, "facts.jsonl"), func(f fact) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if mode == "package" && f.Package != pkg {
				return nil
			}
			result.Facts++
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
		})
		if err != nil {
			return result, err
		}
	}
	result.Status = "pass"
	if result.Resolved != result.Facts {
		result.Status = "partial"
	}
	result.Branch = mode
	if result.Facts > 0 {
		result.WitnessShare = float64(result.Missing+result.Ambiguous) / float64(result.Facts)
	}
	err = dataset.WriteJSON(filepath.Join(out, "result.json"), result)
	return result, err
}

package runner

import (
	"context"
	"crypto/sha256"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"gxstudy/oracle"
	"gxstudy/workload"
	"slices"
	"strings"
)

// MakeChange selects a Go source file with at least two function/method nodes.
// It renames one, removes one (and its owned edges/docs/vector), adds a new
// symbol/vector, and adds/changes call edges. It never changes its input.
func MakeChange(d dataset.Dataset) (adapter.FileChange, error) {
	byFile := map[string][]dataset.Node{}
	for _, n := range d.Nodes {
		if n.Language == "go" && n.FilePath != "" && n.StartLine > 0 && (n.Kind == "function" || n.Kind == "method") {
			byFile[n.FilePath] = append(byFile[n.FilePath], n)
		}
	}
	var paths []string
	for path, ns := range byFile {
		if len(ns) >= 2 {
			paths = append(paths, path)
		}
	}
	slices.SortFunc(paths, func(a, b string) int {
		ha := sha256.Sum256([]byte(a))
		hb := sha256.Sum256([]byte(b))
		return strings.Compare(string(ha[:]), string(hb[:]))
	})
	if len(paths) == 0 {
		return adapter.FileChange{}, fmt.Errorf("delta requires a Go source file with two functions")
	}
	path := paths[0]
	functions := slices.Clone(byFile[path])
	slices.SortFunc(functions, func(a, b dataset.Node) int { return strings.Compare(a.ID, b.ID) })
	rename, removed := functions[0], functions[1]
	added := removed
	added.ID += "__added"
	added.Name = "studyAddedFunction"
	added.QualName += "__added"
	r := dataset.Dataset{Repo: d.Repo}
	owned := map[string]bool{}
	for _, n := range d.Nodes {
		if n.FilePath != path {
			continue
		}
		owned[n.ID] = true
		if n.ID == removed.ID {
			continue
		}
		if n.ID == rename.ID {
			n.Name += "__renamed"
			n.QualName += "__renamed"
		}
		r.Nodes = append(r.Nodes, n)
	}
	r.Nodes = append(r.Nodes, added)
	for _, e := range d.Edges {
		if e.FilePath != path && !owned[e.From] {
			continue
		}
		if e.From == removed.ID {
			continue
		}
		if e.To == removed.ID {
			if e.From == rename.ID {
				e.To = added.ID
			} else {
				continue
			}
		}
		r.Edges = append(r.Edges, e)
	}
	for i, e := range r.Edges {
		if e.From == rename.ID {
			r.Edges[i].To = added.ID
			break
		}
	}
	r.Edges = append(r.Edges, dataset.Edge{From: added.ID, To: rename.ID, Kind: "calls", FilePath: path, Line: added.StartLine, Confidence: 1})
	for _, f := range d.Files {
		if f.Path == path {
			hash := sha256.Sum256([]byte(f.ContentHash + "\ndeltaprobe graph mutation\n"))
			f.ContentHash = fmt.Sprintf("%x", hash)
			f.Size += int64(len("\ndeltaprobe graph mutation\n"))
			r.Files = append(r.Files, f)
		}
	}
	for _, s := range d.Symbols {
		if owned[s.NodeID] && s.NodeID != removed.ID {
			if s.NodeID == rename.ID {
				s.Tokens += " deltaprobe"
			}
			r.Symbols = append(r.Symbols, s)
		}
	}
	r.Symbols = append(r.Symbols, dataset.Symbol{NodeID: added.ID, Tokens: "studyAddedFunction deltaprobe"})
	for _, c := range d.Content {
		if c.FilePath == path && c.NodeID != removed.ID {
			if c.NodeID == rename.ID {
				c.Body += "\ndeltaprobe\n"
			}
			r.Content = append(r.Content, c)
		}
	}
	r.Content = append(r.Content, dataset.Content{ChunkID: added.ID + "#0", NodeID: added.ID, FilePath: path, Body: "func studyAddedFunction() { /* deltaprobe */ }"})
	for _, v := range d.Vectors {
		if owned[v.NodeID] && v.NodeID != removed.ID {
			r.Vectors = append(r.Vectors, v)
		}
	}
	r.Vectors = append(r.Vectors, dataset.MakeVector(added.ID, 20261022))
	return adapter.FileChange{Path: path, Replacement: r}, nil
}
func deltaIdentities(base dataset.Dataset, c adapter.FileChange) (dataset.Node, dataset.Node, dataset.Node) {
	old := map[string]dataset.Node{}
	for _, n := range base.Nodes {
		if n.FilePath == c.Path {
			old[n.ID] = n
		}
	}
	var renamed, removed, added dataset.Node
	for _, n := range c.Replacement.Nodes {
		before, ok := old[n.ID]
		if !ok {
			added = n
		} else if before.Name != n.Name {
			renamed = before
		}
		delete(old, n.ID)
	}
	for _, n := range old {
		removed = n
	}
	return renamed, removed, added
}
func checkDelta(ctx context.Context, b adapter.Backend, ref adapter.SegmentRef, base, changed dataset.Dataset, c adapter.FileChange) ([]workload.Check, error) {
	s, e := b.Open(ctx, ref.Path, ref)
	if e != nil {
		return nil, e
	}
	defer s.Close()
	o := oracle.New(changed)
	renamed, removed, added := deltaIdentities(base, c)
	qs := []workload.Query{{ID: "delta-file", Op: "file", Repo: changed.Repo, Key: c.Path}, {ID: "delta-old-name", Op: "name", Repo: changed.Repo, Key: renamed.Name, K: -1}, {ID: "delta-new-name", Op: "name", Repo: changed.Repo, Key: renamed.Name + "__renamed", K: -1}, {ID: "delta-removed-id", Op: "id", Repo: changed.Repo, Key: removed.ID}, {ID: "delta-added-id", Op: "id", Repo: changed.Repo, Key: added.ID}, {ID: "delta-removed-out", Op: "out", Repo: changed.Repo, Key: removed.ID, Kind: "any"}, {ID: "delta-added-out", Op: "out", Repo: changed.Repo, Key: added.ID, Kind: "any"}, {ID: "delta-renamed-out", Op: "out", Repo: changed.Repo, Key: renamed.ID, Kind: "any"}, {ID: "delta-added-in", Op: "in", Repo: changed.Repo, Key: added.ID, Kind: "any"}, {ID: "delta-dangling-in", Op: "in", Repo: changed.Repo, Key: removed.ID, Kind: "any"}, {ID: "delta-symbol", Op: "symbols", Repo: changed.Repo, Key: "deltaprobe", K: len(changed.Symbols), MinOverlap: 1, ExactRank: true, ForbiddenIDs: []string{removed.ID}}, {ID: "delta-content", Op: "content", Repo: changed.Repo, Key: "deltaprobe", K: len(changed.Content), MinOverlap: 1, ExactRank: true}, {ID: "delta-vector", Op: "vector", Repo: changed.Repo, Vec: perturb(dataset.MakeVector(added.ID, 20261022).Values), K: 10, RequiredIDs: []string{added.ID}, ForbiddenIDs: []string{removed.ID}}}
	var want []workload.Expected
	for _, q := range qs {
		a, e := workload.Evaluate(ctx, map[string]adapter.Segment{changed.Repo: o}, nil, q)
		if e != nil {
			return nil, e
		}
		want = append(want, workload.Expected{QueryID: q.ID, Answer: a})
	}
	want, e = oracle.ReferenceAnswers([]dataset.Dataset{changed}, qs, want)
	if e != nil {
		return nil, e
	}
	_, checks, e := measuredQueries(ctx, map[string]adapter.Segment{changed.Repo: s}, nil, qs, want, b.Capabilities())
	return checks, e
}

// A held-out perturbation, not an indexed vector. The mutation's added vector
// remains a near neighbour, while deleted vectors must not appear.
func perturb(v []float32) []float32 {
	noise := dataset.MakeVector("delta-heldout", 20261022).Values
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = .97*x + .03*noise[i]
	}
	return out
}

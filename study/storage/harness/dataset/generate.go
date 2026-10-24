package dataset

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"slices"
	"strings"
)

const CrossRepoPolicy = "every-100th-local-call-seeded-source-functions-with-hubs"

// Derive selects an exact count of files by seeded hash order, retains all
// file-less nodes, then filters file-owned nodes and edges. Unknown endpoints
// survive; known endpoints in omitted files do not. full fraction preserves all.
func Derive(base Dataset, repo string, fraction float64, seed int64) Dataset {
	d := Dataset{Repo: repo, Meta: base.Meta}
	d.Meta.Origin = "derived"
	d.Meta.ParentRepo = base.Repo
	d.Meta.DerivedFraction = fraction
	d.Meta.Seed = seed
	d.Meta.Counts.SkippedSpans = 0
	files := slices.Clone(base.Files)
	slices.SortFunc(files, func(a, b File) int {
		ha := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", seed, a.Path)))
		hb := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", seed, b.Path)))
		return strings.Compare(string(ha[:]), string(hb[:]))
	})
	n := int(math.Ceil(float64(len(files)) * fraction))
	if n > len(files) {
		n = len(files)
	}
	if n < 0 {
		n = 0
	}
	selected := map[string]bool{}
	for _, f := range files[:n] {
		selected[f.Path] = true
		d.Files = append(d.Files, f)
	}
	slices.SortFunc(d.Files, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	all := map[string]bool{}
	keep := map[string]bool{}
	id := func(s string) string {
		if base.Meta.Namespace != "" {
			prefix := base.Meta.Namespace + "::"
			if strings.HasPrefix(s, prefix) {
				return repo + "::" + strings.TrimPrefix(s, prefix)
			}
			return s // an already-qualified real cross-repository endpoint
		}
		return repo + "::" + s
	}
	d.Meta.Namespace = repo
	for _, v := range base.Nodes {
		all[v.ID] = true
		if fraction >= 1 || v.FilePath == "" || selected[v.FilePath] {
			keep[v.ID] = true
			v.ID = id(v.ID)
			d.Nodes = append(d.Nodes, v)
		}
	}
	for _, v := range base.Edges {
		if fraction < 1 && ((v.FilePath != "" && !selected[v.FilePath]) || (all[v.From] && !keep[v.From]) || (all[v.To] && !keep[v.To])) {
			continue
		}
		v.From = id(v.From)
		v.To = id(v.To)
		d.Edges = append(d.Edges, v)
	}
	for _, v := range base.Symbols {
		if keep[v.NodeID] {
			v.NodeID = id(v.NodeID)
			d.Symbols = append(d.Symbols, v)
		}
	}
	for _, v := range base.Content {
		if keep[v.NodeID] {
			v.NodeID = id(v.NodeID)
			v.ChunkID = id(v.ChunkID)
			d.Content = append(d.Content, v)
		}
	}
	contentNodes := map[string]bool{}
	for _, v := range d.Content {
		contentNodes[v.NodeID] = true
	}
	for _, v := range d.Nodes {
		if v.Kind == "function" || v.Kind == "method" {
			d.Vectors = append(d.Vectors, MakeVector(v.ID, seed))
			if !contentNodes[v.ID] {
				d.Meta.Counts.SkippedSpans++
			}
		}
	}
	return d
}
func randomUnit(id string, seed int64) []float32 {
	v := make([]float32, Dimensions)
	norm := 0.
	for b := 0; b < Dimensions/16; b++ {
		h := sha256.Sum256([]byte(fmt.Sprintf("%d:%s:%d", seed, id, b)))
		for j := 0; j < 16; j++ {
			x := float32(int(binary.LittleEndian.Uint16(h[j*2:]))-32768) / 32768
			v[b*16+j] = x
			norm += float64(x) * float64(x)
		}
	}
	for i, x := range v {
		v[i] = float32(float64(x) / math.Sqrt(norm))
	}
	return v
}

// MakeVector creates one of 32 deterministic noisy unit-vector clusters.
// Held-out query IDs use this same distribution but are never stored in the index.
func MakeVector(id string, seed int64) Vector {
	hash := sha256.Sum256([]byte(id))
	center := randomUnit(fmt.Sprintf("cluster-%d", int(hash[0])%32), seed)
	noise := randomUnit(id, seed)
	v := Vector{NodeID: id, Values: make([]float32, Dimensions)}
	norm := 0.
	for i := range v.Values {
		v.Values[i] = .8*center[i] + .2*noise[i]
		norm += float64(v.Values[i]) * float64(v.Values[i])
	}
	for i, x := range v.Values {
		v.Values[i] = float32(float64(x) / math.Sqrt(norm))
	}
	return v
}

// LinkCrossRepo rewrites every 100th call edge to an existing function/method in
// the next repository. It does not add edges or change source ownership.
func LinkCrossRepo(repos []Dataset) {
	if len(repos) < 2 {
		return
	}
	for i := range repos {
		repos[i].Meta.CrossRepoPolicy = CrossRepoPolicy
		next := (i + 1) % len(repos)
		var targets []string
		for _, n := range repos[next].Nodes {
			if n.FilePath != "" && n.StartLine > 0 && (n.Kind == "function" || n.Kind == "method") {
				targets = append(targets, n.ID)
			}
		}
		slices.SortFunc(targets, func(a, b string) int {
			ha := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", repos[i].Meta.Seed, a)))
			hb := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", repos[i].Meta.Seed, b)))
			return strings.Compare(string(ha[:]), string(hb[:]))
		})
		if len(targets) == 0 {
			continue
		}
		calls := 0
		for j := range repos[i].Edges {
			e := &repos[i].Edges[j]
			if e.Kind != "calls" || e.CrossRepo {
				continue
			}
			calls++
			if calls%100 == 0 {
				ordinal := calls/100 - 1
				targetIndex := ordinal % len(targets)
				if ordinal%4 == 0 {
					targetIndex = (ordinal / 4) % min(3, len(targets))
				}
				e.To = targets[targetIndex]
				e.CrossRepo = true
			}
		}
	}
}

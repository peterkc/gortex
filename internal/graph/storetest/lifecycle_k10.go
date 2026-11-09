package storetest

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// K10 Replacement/invalidation (design-c.md 1.8 and 3.1). Given F, apply
// update, delete, rename, remove and re-add, changed dependency and a newly
// satisfied failed lookup in forward and reversed input order. Then
// incremental output equals a clean build, including incoming and cross
// edges and empty sets. Clean builds also compare file metadata, symbol
// search and vectors; incremental views compare graph answers only because
// graphview.Reader does not compose those corpora. Their comparison through
// the MCP search path remains a suite gap. Conflicting duplicate payload
// refuses S; identical contributions deduplicate.
//
// Each target state is a payload derived from F's E1 for R. Its incremental
// build carries only changed files and removals over published E0, except
// re-add, which builds on the removal version. Its clean build carries the
// whole state. R1 serves the incremental build, R2 the clean one.

// k10State is one target state of R.
type k10State struct {
	name   string
	inputs func(*FixtureF) Inputs
	want   func() Payload
}

func k10States(f *FixtureF) []k10State {
	e1 := func() Payload { return f.Payload("r", E1) }
	withConfig := func(config string) func(*FixtureF) Inputs {
		return func(f *FixtureF) Inputs {
			inputs := f.Inputs(CheckoutR1, E1)
			inputs.Config = config
			return inputs
		}
	}
	return []k10State{
		// E1 updates run.go, and S's E1 is the changed dependency.
		{"UpdateAndChangedDependency", withConfig("config-f"), e1},
		{"Delete", withConfig("k10-delete"), func() Payload { return withoutFile(e1(), "r/app/twice.go") }},
		{"RemoveAndReAdd", withConfig("k10-readd"), e1},
		{"Rename", withConfig("k10-rename"), func() Payload { return renamedFile(e1(), "r/app/twice.go", "r/app/double.go") }},
		{"NewlySatisfiedLookup", withConfig("k10-satisfied"), func() Payload { return satisfiedMissing(e1()) }},
	}
}

// testK10IncrementalEqualsClean runs every state with contributions written
// in forward and in reversed file order.
func testK10IncrementalEqualsClean(t *testing.T, backend Backend) {
	for _, reversed := range []bool{false, true} {
		order := "Forward"
		if reversed {
			order = "Reversed"
		}
		t.Run(order, func(t *testing.T) {
			ctx := caseContext(t)
			f := NewFixtureF(t)
			inst := newChainIn(t, ctx, backend, t.TempDir(), nil, f, CheckoutR1, CheckoutR2)
			e0 := publishVersion(t, ctx, inst, f, CheckoutR1, E0)
			base := f.Payload("r", E0)
			for i, state := range k10States(f) {
				want := state.want()
				inputs := state.inputs(f)
				label := fmt.Sprintf("%s (%s)", state.name, order)
				// Removing and re-adding: first an incremental build without
				// twice.go, then the next one with it.
				parent, previous := e0, base
				if state.name == "RemoveAndReAdd" {
					removed := withoutFile(want, "r/app/twice.go")
					removedInputs := inputs
					removedInputs.Tree = "k10-readd-removed"
					parent = k10Incremental(t, ctx, inst, CheckoutR1, e0, base, removed, removedInputs, reversed, fmt.Sprintf("k10-%d-remove", i))
					assertServesPayload(t, label+": removed", openView(t, ctx, inst, CheckoutR1).Reader(), "r", removed, false)
					previous = removed
				}
				k10Incremental(t, ctx, inst, CheckoutR1, parent, previous, want, inputs, reversed, fmt.Sprintf("k10-%d", i))
				clean := sealedBuild(t, ctx, inst, CheckoutR2, inputs, fmt.Sprintf("k10-%d-clean", i), want)
				if err := clean.Publish(ctx, current(t, ctx, inst, CheckoutR2), fmt.Sprintf("k10-%d-clean-publish", i)); err != nil {
					t.Fatalf("%s: publish the clean build: %v", label, err)
				}
				assertServesPayload(t, label+": clean", openView(t, ctx, inst, CheckoutR2).Reader(), "r", want, true)
				assertServesPayload(t, label+": incremental", openView(t, ctx, inst, CheckoutR1).Reader(), "r", want, false)
			}
		})
	}
}

// k10Incremental publishes an incremental build of want over parent, which
// holds base: one write per changed file, in file order or reversed.
func k10Incremental(t *testing.T, ctx context.Context, inst Instance, checkout string, parent Version, base, want Payload, inputs Inputs, reversed bool, operation string) Version {
	t.Helper()
	build, err := inst.Begin(ctx, BuildRequest{Checkout: checkout, Inputs: inputs, Parent: parent, Operation: operation})
	if err != nil {
		t.Fatalf("%s: begin over %q: %v", operation, parent, err)
	}
	changed, removed := fileDelta(base, want)
	if reversed {
		slices.Reverse(changed)
	}
	for _, file := range changed {
		if err := build.Write(ctx, fileContribution(want, file)); err != nil {
			t.Fatalf("%s: write %s: %v", operation, file, err)
		}
	}
	if err := build.Write(ctx, Payload{RemovedFiles: removed, Producers: want.Producers}); err != nil {
		t.Fatalf("%s: remove %v: %v", operation, removed, err)
	}
	if err := build.Seal(ctx); err != nil {
		t.Fatalf("%s: seal: %v", operation, err)
	}
	if err := build.Publish(ctx, current(t, ctx, inst, checkout), operation+"-publish"); err != nil {
		t.Fatalf("%s: publish: %v", operation, err)
	}
	return build.Version()
}

// testK10IdenticalContributionsDeduplicate writes every file's contribution
// of E0 twice: the version still serves each fact once.
func testK10IdenticalContributionsDeduplicate(t *testing.T, backend Backend) {
	ctx, f, inst := newK1Chain(t, backend)
	base := current(t, ctx, inst, CheckoutR1)
	build, err := inst.Begin(ctx, BuildRequest{Checkout: CheckoutR1, Inputs: f.Inputs(CheckoutR1, E0), Operation: "k10-twice"})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	want := f.Payload("r", E0)
	for range 2 {
		for _, file := range payloadFiles(want) {
			if err := build.Write(ctx, fileContribution(want, file)); err != nil {
				t.Fatalf("write %s: %v", file, err)
			}
		}
	}
	if err := build.Seal(ctx); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := build.Publish(ctx, base, "k10-twice-publish"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	assertServesF(t, ctx, openView(t, ctx, inst, CheckoutR1), f, CheckoutR1, E0)
}

// payloadFiles lists every file p contributes to, sorted.
func payloadFiles(p Payload) []string {
	files := map[string]bool{}
	for _, n := range p.Nodes {
		files[n.FilePath] = true
	}
	for _, e := range p.Edges {
		files[e.FilePath] = true
	}
	for _, m := range p.Files {
		files[m.FilePath] = true
	}
	return slices.Sorted(maps.Keys(files))
}

// fileContribution is one file's part of p: its nodes, the edges written
// from it, their symbols and vectors, its metadata and p's producers.
func fileContribution(p Payload, file string) Payload {
	out := Payload{Producers: p.Producers}
	ids := map[string]bool{}
	for _, n := range p.Nodes {
		if n.FilePath == file {
			out.Nodes = append(out.Nodes, n)
			ids[n.ID] = true
		}
	}
	for _, e := range p.Edges {
		if e.FilePath == file {
			out.Edges = append(out.Edges, e)
		}
	}
	for _, s := range p.Symbols {
		if ids[s.NodeID] {
			out.Symbols = append(out.Symbols, s)
		}
	}
	for _, v := range p.Vectors {
		if ids[v.NodeID] {
			out.Vectors = append(out.Vectors, v)
		}
	}
	for _, m := range p.Files {
		if m.FilePath == file {
			out.Files = append(out.Files, m)
		}
	}
	return out
}

// fileDelta lists the files whose contribution differs between base and
// want, and the files base has and want lacks.
func fileDelta(base, want Payload) (changed, removed []string) {
	render := func(p Payload) string {
		var parts []string
		for _, n := range p.Nodes {
			parts = append(parts, fmt.Sprintf("%#v", *n))
		}
		for _, e := range p.Edges {
			parts = append(parts, fmt.Sprintf("%#v", *e))
		}
		parts = append(parts, fmt.Sprintf("%#v %#v %#v", p.Symbols, p.Vectors, p.Files))
		slices.Sort(parts)
		return strings.Join(parts, "\n")
	}
	wantFiles := payloadFiles(want)
	for _, file := range wantFiles {
		if render(fileContribution(base, file)) != render(fileContribution(want, file)) {
			changed = append(changed, file)
		}
	}
	for _, file := range payloadFiles(base) {
		if !slices.Contains(wantFiles, file) {
			removed = append(removed, file)
		}
	}
	return changed, removed
}

// withoutFile is p with file deleted: its contribution goes, and so do the
// edges other files wrote to its nodes.
func withoutFile(p Payload, file string) Payload {
	out := Payload{Producers: p.Producers}
	gone := map[string]bool{}
	for _, n := range p.Nodes {
		if n.FilePath == file {
			gone[n.ID] = true
		} else {
			out.Nodes = append(out.Nodes, n)
		}
	}
	for _, e := range p.Edges {
		if e.FilePath != file && !gone[e.To] {
			out.Edges = append(out.Edges, e)
		}
	}
	for _, s := range p.Symbols {
		if !gone[s.NodeID] {
			out.Symbols = append(out.Symbols, s)
		}
	}
	for _, v := range p.Vectors {
		if !gone[v.NodeID] {
			out.Vectors = append(out.Vectors, v)
		}
	}
	for _, m := range p.Files {
		if m.FilePath != file {
			out.Files = append(out.Files, m)
		}
	}
	return out
}

// renamedFile is p with file moved to to: node IDs, paths and the edges and
// corpora that name them follow the move.
func renamedFile(p Payload, from, to string) Payload {
	id := func(s string) string {
		if rest, ok := strings.CutPrefix(s, from+"::"); ok {
			return to + "::" + rest
		}
		return s
	}
	path := func(s string) string {
		if s == from {
			return to
		}
		return s
	}
	out := Payload{Producers: p.Producers}
	for _, n := range p.Nodes {
		c := *n
		c.ID, c.FilePath = id(c.ID), path(c.FilePath)
		out.Nodes = append(out.Nodes, &c)
	}
	for _, e := range p.Edges {
		c := *e
		c.From, c.To, c.FilePath = id(c.From), id(c.To), path(c.FilePath)
		out.Edges = append(out.Edges, &c)
	}
	for _, s := range p.Symbols {
		s.NodeID = id(s.NodeID)
		out.Symbols = append(out.Symbols, s)
	}
	for _, v := range p.Vectors {
		v.NodeID = id(v.NodeID)
		out.Vectors = append(out.Vectors, v)
	}
	for _, m := range p.Files {
		m.FilePath = path(m.FilePath)
		out.Files = append(out.Files, m)
	}
	return out
}

// satisfiedMissing is p with a new file that declares Missing: run.go's
// failed lookup now resolves to it.
func satisfiedMissing(p Payload) Payload {
	const file, id = "r/app/missing.go", "r/app/missing.go::Missing"
	out := Payload{Producers: p.Producers, Symbols: p.Symbols, Vectors: p.Vectors, Files: p.Files}
	out.Nodes = append(slices.Clone(p.Nodes), &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: "Missing", QualName: "app.Missing",
		FilePath: file, StartLine: 3, EndLine: 3, Language: "go", RepoPrefix: "r",
	})
	for _, e := range p.Edges {
		c := *e
		if c.To == graph.UnresolvedMarker+"Missing" {
			c.To, c.Origin, c.Tier = id, graph.OriginASTResolved, "ast"
		}
		out.Edges = append(out.Edges, &c)
	}
	out.Symbols = append(slices.Clone(out.Symbols), graph.SymbolFTSItem{NodeID: id, Tokens: "missing"})
	out.Vectors = append(slices.Clone(out.Vectors), graph.VectorItem{NodeID: id, Vec: []float32{0.6, 0, 0.8}})
	out.Files = append(slices.Clone(out.Files), graph.FileMetaRow{FilePath: file, ContentHash: "k10-missing", Size: 40, NodeCount: 1})
	return out
}

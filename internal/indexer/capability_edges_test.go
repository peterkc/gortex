package indexer

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestSynthesizeCapabilityEdges(t *testing.T) {
	g := graph.New()

	// reads_env: a function reads $AWS_SECRET via reads_config -> cfg::env.
	g.AddNode(&graph.Node{ID: "svc.go::Load", Kind: graph.KindFunction, Name: "Load", FilePath: "svc.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "cfg::env::AWS_SECRET", Kind: graph.KindConfigKey, Name: "AWS_SECRET", Meta: map[string]any{"source": "env"}})
	g.AddEdge(&graph.Edge{From: "svc.go::Load", To: "cfg::env::AWS_SECRET", Kind: graph.EdgeReadsConfig, FilePath: "svc.go", Line: 3})
	// A non-env config read must NOT produce a reads_env edge.
	g.AddNode(&graph.Node{ID: "cfg::viper::db.host", Kind: graph.KindConfigKey, Name: "db.host", Meta: map[string]any{"source": "viper"}})
	g.AddEdge(&graph.Edge{From: "svc.go::Load", To: "cfg::viper::db.host", Kind: graph.EdgeReadsConfig, FilePath: "svc.go", Line: 4})

	// accesses_field: a function writes a struct field.
	g.AddNode(&graph.Node{ID: "svc.go::Server.cache", Kind: graph.KindField, Name: "cache", FilePath: "svc.go"})
	g.AddEdge(&graph.Edge{From: "svc.go::Load", To: "svc.go::Server.cache", Kind: graph.EdgeWrites, FilePath: "svc.go", Line: 5})
	// A write to a non-field target must NOT produce accesses_field.
	g.AddEdge(&graph.Edge{From: "svc.go::Load", To: "unresolved::someGlobal", Kind: graph.EdgeWrites, FilePath: "svc.go", Line: 6})

	// executes_process: a function shells out via exec.Command.
	g.AddEdge(&graph.Edge{From: "svc.go::Load", To: "unresolved::exec.Command", Kind: graph.EdgeCalls, FilePath: "svc.go", Line: 7})
	// A normal call must NOT produce executes_process.
	g.AddEdge(&graph.Edge{From: "svc.go::Load", To: "unresolved::fmt.Println", Kind: graph.EdgeCalls, FilePath: "svc.go", Line: 8})

	re, ep, fa, err := synthesizeCapabilityEdges(g)
	if err != nil {
		t.Fatal(err)
	}
	if re != 1 {
		t.Errorf("reads_env count = %d, want 1", re)
	}
	if ep != 1 {
		t.Errorf("executes_process count = %d, want 1", ep)
	}
	if fa != 1 {
		t.Errorf("accesses_field count = %d, want 1", fa)
	}

	want := map[graph.EdgeKind]string{
		graph.EdgeReadsEnv:        "cfg::env::AWS_SECRET",
		graph.EdgeAccessesField:   "svc.go::Server.cache",
		graph.EdgeExecutesProcess: "string::process::exec.Command",
	}
	got := map[graph.EdgeKind]string{}
	for _, e := range g.AllEdges() {
		switch e.Kind {
		case graph.EdgeReadsEnv, graph.EdgeAccessesField, graph.EdgeExecutesProcess:
			if e.From != "svc.go::Load" {
				t.Errorf("%s edge from %q, want svc.go::Load", e.Kind, e.From)
			}
			got[e.Kind] = e.To
		}
	}
	for k, to := range want {
		if got[k] != to {
			t.Errorf("%s edge -> %q, want %q", k, got[k], to)
		}
	}

	// The synthetic process node exists and is typed.
	pn := g.GetNode("string::process::exec.Command")
	if pn == nil {
		t.Fatal("process node string::process::exec.Command not created")
	}
	if pn.Kind != graph.KindString || pn.Meta["context"] != "process" {
		t.Errorf("process node kind/context = %v/%v, want string/process", pn.Kind, pn.Meta["context"])
	}

	// Idempotent at the graph level: a second pass produces no duplicate
	// edges (AddEdge dedupes by edge key), even though the per-pass
	// telemetry counters re-count from the current base edges.
	countCap := func() int {
		n := 0
		for _, e := range g.AllEdges() {
			switch e.Kind {
			case graph.EdgeReadsEnv, graph.EdgeAccessesField, graph.EdgeExecutesProcess:
				n++
			}
		}
		return n
	}
	before := countCap()
	if _, _, _, err := synthesizeCapabilityEdges(g); err != nil {
		t.Fatal(err)
	}
	if after := countCap(); after != before {
		t.Errorf("second pass created duplicate capability edges: before=%d after=%d", before, after)
	}
}

// TestSynthesizeCapabilityEdges_ResolvedExecForms guards executes_process
// recognition of call targets the resolver has bound to fully-qualified
// external node IDs (the common case for Go os/exec) — not just the
// unresolved::exec.Command spelling.
func TestSynthesizeCapabilityEdges_ResolvedExecForms(t *testing.T) {
	cases := map[string]string{
		"unresolved::exec.Command":           "string::process::exec.Command",
		"stdlib::os/exec::Command":           "string::process::exec.Command",
		"gortex::stdlib::os/exec::Command":   "string::process::exec.Command",
		"stdlib::os/exec::CommandContext":    "string::process::exec.CommandContext",
		"stdlib::syscall::Exec":              "string::process::syscall.Exec",
		"dep::github.com/x/sh::Command::new": "", // not an exec API
	}
	for to, wantTarget := range cases {
		g := graph.New()
		g.AddNode(&graph.Node{ID: "a.go::F", Kind: graph.KindFunction, Name: "F", FilePath: "a.go"})
		g.AddEdge(&graph.Edge{From: "a.go::F", To: to, Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1})
		_, ep, _, err := synthesizeCapabilityEdges(g)
		if err != nil {
			t.Fatal(err)
		}
		var got string
		for _, e := range g.AllEdges() {
			if e.Kind == graph.EdgeExecutesProcess {
				got = e.To
			}
		}
		if wantTarget == "" {
			if ep != 0 || got != "" {
				t.Errorf("callee %q: expected no executes_process edge, got %q (ep=%d)", to, got, ep)
			}
			continue
		}
		if ep != 1 || got != wantTarget {
			t.Errorf("callee %q: executes_process -> %q (ep=%d), want %q", to, got, ep, wantTarget)
		}
	}
}

package indexer

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/resolver"
)

// A save that shifts every later line of the file keeps the file's unchanged
// references bound without re-resolving them, and the file's rows equal a
// whole index of the edited tree. The case the shape-keyed reuse
// (incremental_resolve.go) refuses is the one exercised: one declaration
// calling two different functions of the same name (q.Helper, r.Helper) is an
// ambiguous shape there, and the carry tells them apart by their place in the
// declaration. A rename of a called function in the same save is not
// carried: its callers re-resolve.
func TestPriorBindingsCarryUnchangedReferencesAcrossAReparse(t *testing.T) {
	builderIsolateGit(t)
	root := builderTempDir(t, "prior-bindings")
	builderGit(t, root, "init", "--initial-branch=main")
	builderWriteTree(t, root, map[string]string{
		"go.mod": "module example.test/m\n\ngo 1.23\n",
		"p/a.go": "package p\n\nimport (\n\t\"strings\"\n\n\t\"example.test/m/q\"\n\t\"example.test/m/r\"\n)\n\nfunc First() int {\n\ta := q.Helper()\n\tb := r.Helper()\n\t_ = strings.ToUpper(\"x\")\n\treturn a + b + Local() + Unknown()\n}\n\nfunc Local() int { return 1 }\n\nfunc Second() int {\n\treturn q.Helper() + Local() + Gone() + Later()\n}\n\nfunc Gone() int { return 2 }\n",
		"q/q.go": "package q\n\nfunc Helper() int { return 3 }\n",
		"r/r.go": "package r\n\nfunc Helper() int { return 4 }\n",
	})
	builderGit(t, root, "add", "-A")
	builderGit(t, root, "commit", "-m", "fixture")
	index := func(logger *zap.Logger) (*graph.Graph, *Indexer) {
		g := graph.New()
		cfg := config.Default().Index
		cfg.Workers = 1
		idx := New(g, builderRegistry(), cfg, zap.NewNop())
		idx.SetRepoPrefix(builderRepoPrefix)
		if logger != nil {
			idx.resolver.SetLogger(logger)
		}
		_, err := idx.Index(root)
		require.NoError(t, err)
		return g, idx
	}
	rows := func(g *graph.Graph) []string {
		var out []string
		for _, e := range g.AllEdges() {
			if e.FilePath == builderRepoPrefix+"/p/a.go" {
				out = append(out, fmt.Sprintf("%s -%s-> %s @%d o=%s c=%.2f", e.From, e.Kind, e.To, e.Line, e.Origin, e.Confidence))
			}
		}
		sort.Strings(out)
		return out
	}
	core, logs := observer.New(zap.InfoLevel)
	g, idx := index(zap.New(core))
	logs.TakeAll()

	// A declaration inserted above First shifts every later line; Gone is
	// renamed, so Second's call to it must not carry.
	aPath := filepath.Join(root, "p", "a.go")
	bumpMtime(t, aPath, "package p\n\nimport (\n\t\"strings\"\n\n\t\"example.test/m/q\"\n\t\"example.test/m/r\"\n)\n\nfunc Zero() int { return 0 }\n\nfunc First() int {\n\ta := q.Helper()\n\tb := r.Helper()\n\t_ = strings.ToUpper(\"x\")\n\treturn a + b + Local() + Unknown()\n}\n\nfunc Local() int { return 1 }\n\nfunc Second() int {\n\treturn q.Helper() + Local() + Gone2() + Later()\n}\n\nfunc Gone2() int { return 2 }\n\nfunc Later() int { return 5 }\n")
	_, err := idx.IncrementalReindexPaths(root, []string{aPath})
	require.NoError(t, err)

	carried := 0
	for _, entry := range logs.FilterMessage("resolver: incremental phase complete").All() {
		if entry.ContextMap()["phase"] == "carry_prior_bindings" {
			carried += int(entry.ContextMap()["carried"].(int64))
		}
	}
	require.Positive(t, carried, "no reference carried its prior binding across the reparse")
	// Later, unresolved before, is declared by the edit and must bind.
	fresh, _ := index(nil)
	require.Equal(t, rows(fresh), rows(g), "the carried rows must equal a whole index of the edited tree")
	require.Contains(t, strings.Join(rows(g), "\n"), "-calls-> "+builderRepoPrefix+"/p/a.go::Later", "the reference the edit made resolvable is bound")
}

func TestPriorBindingsKeepFreshBridgeCallSpelling(t *testing.T) {
	for _, key := range []string{"cargo_call_stub", "python_call_stub"} {
		t.Run(key, func(t *testing.T) {
			g := graph.New()
			caller := &graph.Node{ID: "app.py::run", Name: "run", FilePath: "app.py", Kind: graph.KindFunction, StartLine: 1}
			provider := &graph.Node{ID: "provider.py::f", Name: "f", FilePath: "provider.py", Kind: graph.KindFunction}
			prior := &graph.Edge{From: caller.ID, To: provider.ID, Kind: graph.EdgeCalls, FilePath: "app.py", Line: 2, CrossRepo: true, Meta: map[string]any{key: "unresolved::old.f", "keep": "metadata"}}
			g.AddBatch([]*graph.Node{caller, provider}, []*graph.Edge{prior})
			fresh := &graph.Edge{From: caller.ID, To: "unresolved::new.f", Kind: graph.EdgeCalls, FilePath: "app.py", Line: 2}
			stage := &incrementalBatchStage{graphPath: "app.py", priorNodes: []*graph.Node{caller}, result: &parser.ExtractionResult{Nodes: []*graph.Node{caller}, Edges: []*graph.Edge{fresh}}, reuse: map[reuseKey]*reuseVal{}}
			stages := []*incrementalBatchStage{stage}
			carried := stagePriorBindings(g, stages, loadIncrementalPriorView(g, stages))
			binding, ok := carried[resolver.PriorBindingKey(fresh.From, fresh.Kind, fresh.Line, fresh.To)]
			require.True(t, ok, "the prior binding must carry")
			require.Equal(t, fresh.To, binding.Meta[key])
			require.Equal(t, "metadata", binding.Meta["keep"])
			require.Equal(t, "unresolved::old.f", prior.Meta[key], "carry must not change prior metadata")
		})
	}
}

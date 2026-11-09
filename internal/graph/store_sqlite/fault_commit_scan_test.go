package store_sqlite

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// commitHelpers are the commit paths that carry fault points.
var commitHelpers = map[string]string{
	"fault_hook.go:commitPayloadTx":     "payload write commit",
	"fault_hook.go:commitPayloadSealTx": "payload write that also seals a generation",
	"catalog.go:withTx":                 "multi-record catalog transition",
	"analysis_write_context.go:commit":  "ownedWriteTx: AddBatch, purge and analysis writes",
}

// commitExempt are the Commit calls that are not payload or catalog writes,
// each with its reason.
// Two analysis-state autocommits are also exempt from FR-6's payload, seal,
// publish and catalog list (this scanner checks Commit calls, not statements):
// analysis_generation_state.go:initAnalysisGenerationState marks interrupted
// analysis builders stale on open; analysis_generation_write.go:AbortAnalysisGeneration
// marks a cancelled analysis builder stale. Neither publishes graph payloads
// or routes; analysis snapshots are derived state, not the storage catalog.
var commitExempt = map[string]string{
	"analysis_membership.go:AnalysisMembershipsContext":                   "read-only transaction on the read pool",
	"bounded_adjacency.go:findEndpointEdgeIdentitiesBounded":              "read-only transaction on the read pool",
	"bounded_adjacency.go:FindOutgoingSiteEdgeIdentitiesBounded":          "read-only transaction on the read pool",
	"bounded_edge_existence.go:FindExistingEdgeEndpoints":                 "read-only transaction on the read pool",
	"bounded_incoming_sources.go:FindIncomingSourcesBounded":              "read-only transaction on the read pool",
	"constant_value_projection.go:ReadConstantValueProjectionContext":     "read-only transaction on the read pool",
	"contract_file_nodes.go:LayerContractFileNodesContext":                "read-only transaction on the read pool",
	"contract_file_projection.go:loadContractProjection":                  "read-only transaction on the read pool",
	"contract_projection_footprint.go:contractProjectionFootprintContext": "read-only transaction on the read pool",
	"contract_repo_projection.go:layerContractRepoProjectionContext":      "read-only transaction on the read pool",
	"file_editing_context_context.go:WithFileEditingContextReader":        "read-only transaction on the read pool",
	"incoming_source_candidates.go:ReadIncomingSourceCandidates":          "read-only transaction on the read pool",
	"incoming_sources_scoped.go:FindIncomingSourcesScoped":                "read-only transaction on the read pool",
	"localization_projection.go:FindNodesByNameBounded":                   "read-only transaction on the read pool",
	"localization_projection.go:FindFileNodesBounded":                     "read-only transaction on the read pool",
	"outline_communities.go:ScopedAnalysisCommunitySummaries":             "read-only transaction on the read pool",
	"scoped_outline.go:ScopedOutlineCountsFiltered":                       "read-only transaction on the read pool",
	"store_call_reference_outgoing.go:GetCallReferenceOutEdgesContext":    "read-only transaction on the read pool",
	"schema_version.go:applyInPlaceMigrationsWith":                        "schema migration; its crash points are the upgrade hook",
	"row_counters.go:EnsureRowCounters":                                   "row-counter maintenance: counter tables and triggers only",
	"row_counters.go:CheckRowCounters":                                    "row-counter maintenance: counter tables only",
	"store_fts_tokenize.go:SymbolFTSTokenize":                             "scratch :memory: database, not the store",
}

// TestStorageFaultPayloadCommitsUseTheHelpers holds every payload and catalog
// write to a commit path with fault points: each Commit call in this
// package's non-test code sits in a helper or is exempt with a reason, and
// every exemption still names a real call.
func TestStorageFaultPayloadCommitsUseTheHelpers(t *testing.T) {
	t.Run("ScannerReportsAPlantedViolation", func(t *testing.T) {
		dir := t.TempDir()
		files := map[string]string{
			"helper.go":      "package p\n\nfunc commitTx(tx interface{ Commit() error }) error { return tx.Commit() }\n",
			"reader.go":      "package p\n\nfunc read(tx interface{ Commit() error }) error { return tx.Commit() }\n",
			"writer.go":      "package p\n\nfunc write(tx interface{ Commit() error }) error {\n\treturn tx.Commit()\n}\n",
			"helper_test.go": "package p\n\nfunc raw(tx interface{ Commit() error }) error { return tx.Commit() }\n",
		}
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		got := commitViolations(t, dir,
			map[string]string{"helper.go:commitTx": "helper"},
			map[string]string{"reader.go:read": "read-only", "gone.go:removed": "stale"})
		want := []string{"stale exemption gone.go:removed", "writer.go:4 write"}
		if !slices.Equal(got, want) {
			t.Fatalf("violations = %v, want %v", got, want)
		}
	})
	t.Run("EveryCommitIsCovered", func(t *testing.T) {
		if got := commitViolations(t, ".", commitHelpers, commitExempt); len(got) > 0 {
			t.Fatalf("Commit calls outside the fault-point helpers and the exemptions:\n%s", strings.Join(got, "\n"))
		}
	})
}

// commitViolations lists each Commit() call in dir's non-test Go files whose
// enclosing function is neither a helper nor exempt, and each exemption that
// names no Commit call, sorted.
func commitViolations(t *testing.T, dir string, helpers, exempt map[string]string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	used := map[string]bool{}
	var out []string
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		base := filepath.Base(path)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			key := base + ":" + fn.Name.Name
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) != 0 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Commit" {
					return true
				}
				if _, ok := helpers[key]; ok {
					return true
				}
				if _, ok := exempt[key]; ok {
					used[key] = true
					return true
				}
				out = append(out, fmt.Sprintf("%s:%d %s", base, fset.Position(call.Pos()).Line, fn.Name.Name))
				return true
			})
		}
	}
	for key := range exempt {
		if !used[key] {
			out = append(out, "stale exemption "+key)
		}
	}
	slices.Sort(out)
	return out
}

// The slot snapshot exists only to select fault points. Production callers
// with no hook must not pay for that extra SELECT.
func TestStorageFaultNilHookSkipsSlotRead(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "catalog.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	total, guarded := 0, 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "FlipCheckoutRouteSlot" {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				if method, ok := call.Fun.(*ast.SelectorExpr); ok && method.Sel.Name == "QueryRowContext" {
					total++
				}
			}
			branch, ok := node.(*ast.IfStmt)
			if !ok {
				return true
			}
			condition, ok := branch.Cond.(*ast.BinaryExpr)
			if !ok || condition.Op != token.NEQ {
				return true
			}
			hook, hookOK := condition.X.(*ast.SelectorExpr)
			nilValue, nilOK := condition.Y.(*ast.Ident)
			if !hookOK || hook.Sel.Name != "faultHook" || !nilOK || nilValue.Name != "nil" {
				return true
			}
			ast.Inspect(branch.Body, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok {
					if method, ok := call.Fun.(*ast.SelectorExpr); ok && method.Sel.Name == "QueryRowContext" {
						guarded++
					}
				}
				return true
			})
			return true
		})
	}
	if total != 1 || guarded != total {
		t.Fatalf("slot snapshot SELECT calls: total=%d, guarded by non-nil hook=%d; want one guarded call", total, guarded)
	}
}

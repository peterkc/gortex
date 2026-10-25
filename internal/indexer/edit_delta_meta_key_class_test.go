package indexer

import (
	"fmt"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser/languages"
)

// metaKeysRestatedOutsideTheFingerprint names every node or edge metadata key
// a clean index carries that the file's metadata fingerprint does not digest,
// and who restates it when an edit leaves the file inert. A key missing here
// and from the fingerprint is the class of the clone_body bug: it changes
// while the fingerprint does not, and an inert verdict keeps the prior value.
var metaKeysRestatedOutsideTheFingerprint = map[string]string{
	// Builtin stubs are pathless: no file owns them, the resolver writes them
	// when it binds a call, and no inert verdict touches them.
	"builtin":      "resolver (go_builtins_attribution: pathless builtin stubs)",
	"builtin_kind": "resolver (go_builtins_attribution: pathless builtin stubs)",
	// The body's signature: the clone pass, or the delta's clone carry.
	"clone_sig": "clone pass (finaliseCloneSignatures) / delta clone carry",
	// Contract nodes and owner edges: the contract pass of every re-derived file.
	// For an ID two files share, symbol_id and contract_owner_* are the
	// registry's pick across the files: an edit of one can change the pick
	// while the other stays inert and keeps the old one (the shared-ID case the
	// parity rules exclude).
	"confidence":                 "contracts pass (commitIncrementalContractFiles)",
	"contract_meta":              "contracts pass (commitIncrementalContractFiles)",
	"contract_owner_confidence":  "contracts pass (owner edges)",
	"contract_owner_meta":        "contracts pass (owner edges)",
	"contract_owner_project":     "contracts pass (owner edges)",
	"contract_owner_record":      "contracts pass (owner edges)",
	"contract_owner_repo_prefix": "contracts pass (owner edges)",
	"contract_owner_symbol_id":   "contracts pass (owner edges)",
	"contract_owner_type":        "contracts pass (owner edges)",
	"contract_owner_workspace":   "contracts pass (owner edges)",
	"contract_type":              "contracts pass (handles_route edges)",
	"line":                       "contracts pass (contract nodes)",
	"role":                       "contracts pass (contract nodes)",
	"symbol_id":                  "contracts pass (contract nodes)",
	// External module stubs: the module scanner / external-call attribution.
	"ecosystem":     "modules scanner / resolver external_call_attribution (module nodes)",
	"import_path":   "modules scanner / resolver external_call_attribution (module nodes)",
	"python_import": "resolver attributeNonGoModuleImports / resolveTrackedPythonImports (exact dotted import spelling)",
	// Test classification after extraction (test_edges.go).
	"is_test":      "test pass (test_edges.go)",
	"is_test_file": "test pass (test_edges.go)",
	"test_role":    "test pass (test_edges.go)",
	"test_runner":  "test pass (test_edges.go)",
	// Synthesized edges: the resolver's synthesizers.
	"provenance":     "resolver synthesizers (value-ref, framework)",
	"synthesized_by": "resolver synthesizers (value-ref, framework)",
	"resolution":     "resolver (renders_child / extension resolution)",
}

// A body too short to shingle whose comment changes on the same line: the
// file's lines and declarations are unchanged, only the body identity moves.
// The edit is not inert, and the view equals a clean index.
func TestCommentEditInAShortBodyMatchesACleanIndex(t *testing.T) {
	f, _, l := mcpChainFixture(t, builderTreeA(), false)
	for k := 1; k <= 2; k++ {
		mcpEdit(t, l, f, func() {
			builderWriteFile(t, f.worktree, "island.go",
				fmt.Sprintf("package fixture\n\nfunc Island() {\n\t// comment %d\n}\n", k))
		})
		chainAssertFlat(t, f, fmt.Sprintf("comment-%d", k))
	}
}

// Every metadata key of a clean index is either digested by the metadata
// fingerprint at extraction or named, with its restater, in
// metaKeysRestatedOutsideTheFingerprint.
func TestEveryMetadataKeyIsFingerprintedOrRestated(t *testing.T) {
	for name, tree := range map[string]map[string]string{"treeA": builderTreeA(), "restate": kindParityRestateTree()} {
		t.Run(name, func(t *testing.T) {
			dir := builderTempDir(t, "meta-keys")
			builderWriteTree(t, dir, tree)
			idx := New(builderOpenStore(t, "meta-keys-probe"), builderRegistry(), config.Default().Index, zap.NewNop())
			defer idx.Close()
			idx.SetRepoPrefix(builderRepoPrefix)
			if _, err := idx.Index(dir); err != nil {
				t.Fatalf("index: %v", err)
			}
			digested := map[string]bool{}
			note := func(meta map[string]any, keepPresentation bool) {
				for _, key := range fingerprintMetaKeys(meta, fingerprintMetadata, keepPresentation) {
					digested[key] = true
				}
			}
			for rel := range tree {
				if _, ok := idx.prepareFileDelta(filepath.Join(dir, rel)); !ok {
					continue
				}
				prepared, ok := idx.takePreparedSnapshot(filepath.Join(dir, rel))
				if !ok {
					continue
				}
				for _, n := range prepared.result.Nodes {
					if n != nil {
						note(n.Meta, isArtifactNodeKind(n.Kind))
					}
				}
				for _, e := range prepared.result.Edges {
					if e != nil {
						note(e.Meta, false)
					}
				}
				prepared.release()
			}

			clean := builderOpenStore(t, "meta-keys-clean")
			builderIndex(t, clean, dir)
			present := map[string]string{}
			for _, n := range clean.AllNodes() {
				for key := range n.Meta {
					present[key] = "node " + string(n.Kind)
				}
			}
			for _, e := range clean.AllEdges() {
				for key := range e.Meta {
					present[key] = "edge " + string(e.Kind)
				}
			}
			var missing []string
			for key, where := range present {
				if digested[key] || isFingerprintMeta(key) {
					continue
				}
				if _, named := metaKeysRestatedOutsideTheFingerprint[key]; named {
					continue
				}
				missing = append(missing, key+" ("+where+")")
			}
			sort.Strings(missing)
			if len(missing) > 0 {
				t.Errorf("metadata keys neither fingerprinted nor named with a restater: %v", missing)
			}
		})
	}
}

func TestPythonImportSpellingEditColdEquivalent(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			openStore := func() graph.Store {
				if backend == "memory" {
					return graph.New()
				}
				db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, db.Close()) })
				return db
			}
			newIndexer := func(g graph.Store) *Indexer {
				reg := newTestRegistry()
				reg.Register(languages.NewPythonExtractor())
				idx := New(g, reg, config.Default().Index, zap.NewNop())
				idx.SetRepoPrefix("consumer")
				t.Cleanup(idx.Close)
				return idx
			}
			imports := func(g graph.Store) []*graph.Edge {
				var out []*graph.Edge
				for _, edge := range g.GetOutEdges("consumer/app.py") {
					if edge.Kind == graph.EdgeImports {
						out = append(out, edge)
					}
				}
				require.Len(t, out, 1)
				return out
			}
			// Given an attributed import with a stable local alias and module target.
			root := t.TempDir()
			file := filepath.Join(root, "app.py")
			writeFile(t, file, "import pkg.mod as current\ndef run(): return current.open()\n")
			warm := openStore()
			idx := newIndexer(warm)
			_, err := idx.Index(root)
			require.NoError(t, err)
			before := imports(warm)[0]
			require.Equal(t, "pkg.mod", before.Meta["python_import"])
			require.Equal(t, "module::pypi:pkg", before.To)
			// When only the dotted module spelling changes, keeping the alias and top.
			writeFile(t, file, "import pkg.other as current\ndef run(): return current.open()\n")
			require.NoError(t, idx.IndexFile(file))
			cold := openStore()
			_, err = newIndexer(cold).Index(root)
			require.NoError(t, err)
			// Then the incremental edge, including python_import, equals a cold index.
			require.Equal(t, "pkg.other", imports(warm)[0].Meta["python_import"])
			require.Equal(t, "module::pypi:pkg", imports(warm)[0].To)
			require.Equal(t, imports(cold), imports(warm))
		})
	}
}

package store_sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

const oldContractClosureSQL = `SELECT DISTINCT to_id FROM edges
WHERE from_id IN (SELECT CAST(value AS TEXT) FROM json_each(?))
  AND view_gen = ? AND kind IN (?, ?, ?)`

// Follow the purge plan-test precedent: observe the real transaction, replacing
// only the selected query when comparing against the previous implementation.
type closureConnector struct {
	driver driver.Driver
	dsn    string
	calls  *int
}

func (c closureConnector) Driver() driver.Driver { return c.driver }
func (c closureConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.Driver().Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &closureConn{Conn: conn, calls: c.calls}, nil
}

type closureConn struct {
	driver.Conn
	calls *int
}

func (c *closureConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if query == contractFileEvictionClosureSQL {
		*c.calls++
		query = oldContractClosureSQL
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c *closureConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func useOldClosureQuery(t *testing.T, s *Store) *int {
	t.Helper()
	calls := new(int)
	// Join the checkpoint loop first: its goroutine reads writerDB.
	s.stopCheckpointLoop()
	s.writeMu.Lock()
	original := s.writerDB
	writer := sql.OpenDB(closureConnector{driver: original.Driver(), dsn: sqliteWriterDSN(s.dbPath), calls: calls})
	writer.SetMaxOpenConns(1)
	s.writerDB = writer
	s.writeMu.Unlock()
	t.Cleanup(func() {
		s.writeMu.Lock()
		s.writerDB = original
		s.writeMu.Unlock()
		require.NoError(t, writer.Close())
	})
	return calls
}

type closureFixture struct {
	store *Store
	path  string
	files []string
	roots []string
}

func newClosureFixture(t *testing.T, count int, options ...Option) closureFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "closure.sqlite")
	s, err := Open(path, options...)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	fixture := closureFixture{store: s, path: path}
	var nodes []*graph.Node
	var edges []*graph.Edge
	node := func(id, file, repo string, kind graph.NodeKind) *graph.Node {
		return &graph.Node{ID: id, Kind: kind, Name: id, QualName: "qualified." + id,
			FilePath: file, RepoPrefix: repo, Language: "go", StartLine: 2, EndLine: 9,
			StartColumn: 1, EndColumn: 8, WorkspaceID: "workspace", ProjectID: "project",
			Meta: map[string]any{"contract_owner_record": kind == graph.KindContract,
				"type": "env", "nested": map[string]any{"values": []any{"exact", float64(17)}}}}
	}
	owner := func(from, to, file, repo string, kind graph.EdgeKind) *graph.Edge {
		return &graph.Edge{From: from, To: to, Kind: kind, FilePath: file, Line: 4,
			Confidence: 0.75, Origin: "ast", Meta: map[string]any{
				"contract_owner_repo_prefix": repo, "nested": map[string]any{"values": []any{"edge", float64(23)}}}}
	}
	for i := range count {
		file, id, root := fmt.Sprintf("repo/f%02d.go", i), fmt.Sprintf("repo/source%02d", i), fmt.Sprintf("env::ROOT%02d", i)
		fixture.files = append(fixture.files, file)
		fixture.roots = append(fixture.roots, root)
		nodes = append(nodes, node(id, file, "repo", graph.KindFunction), node(root, "records/root.env", "repo", graph.KindContract))
		edges = append(edges, owner(id, root, file, "repo", graph.EdgeProvides))
		for _, kind := range []graph.EdgeKind{graph.EdgeProvides, graph.EdgeConsumes, graph.EdgeHandlesRoute} {
			edges = append(edges, owner(root, "env::CHAIN", "records/root.env", "repo", kind))
		}
	}
	shared := node("env::SHARED", fixture.files[0], "repo", graph.KindContract)
	shared.Meta["contract_owner_record"] = false // retained legacy scalar gets its removed marker
	nodes = append(nodes, shared, node("env::CHAIN", "records/chain.env", "repo", graph.KindContract),
		node("env::TAIL", "records/tail.env", "repo", graph.KindContract),
		node("other/live", "other/live.go", "other", graph.KindFunction))
	edges = append(edges, owner(fixture.roots[0], shared.ID, "records/root.env", "repo", graph.EdgeProvides),
		owner("repo/source00", shared.ID, fixture.files[0], "repo", graph.EdgeProvides),
		owner("other/live", shared.ID, "other/live.go", "other", graph.EdgeConsumes),
		owner("env::CHAIN", "env::TAIL", "records/chain.env", "repo", graph.EdgeProvides),
		owner("other/live", "env::TAIL", "other/live.go", "wrong-owner", graph.EdgeProvides),
		&graph.Edge{From: "other/live", To: "env::CHAIN", Kind: graph.EdgeReferences, FilePath: "other/live.go", Line: 11})
	for _, generation := range []int64{0, 7} {
		h := s.AtGeneration(generation)
		require.NoError(t, h.AddBatchCheckedContext(t.Context(), nodes, edges))
		var documents []graph.SymbolFTSItem
		for _, n := range nodes {
			if n.Kind == graph.KindContract {
				documents = append(documents, graph.SymbolFTSItem{NodeID: n.ID, Tokens: "closuretoken " + n.ID})
			}
		}
		require.NoError(t, h.BatchUpsertSymbolFTS(documents))
		require.NoError(t, h.ReplaceSemanticBindingTypes("repo", []graph.SemanticBindingType{
			{Site: graph.SemanticBindingSite{RepoPrefix: "repo", FilePath: fixture.files[0], Line: 4, Name: "removed"}, TypeName: "ExactRemoved"},
			{Site: graph.SemanticBindingSite{RepoPrefix: "repo", FilePath: "repo/keep.go", Line: 8, Name: "kept"}, TypeName: "ExactKept"},
		}))
		require.NoError(t, h.BulkSetFileMtimes("repo", map[string]int64{fixture.files[0]: 123, "repo/keep.go": 456}))
		require.NoError(t, h.SetRepoIndexState(graph.RepoIndexState{RepoPrefix: "repo", IndexedSHA: "committed-tree", IndexedAt: 42, WorkspaceFP: "fingerprint", ExtractorVersions: `{"go":3}`, NodeCount: len(nodes), EdgeCount: len(edges)}))
		require.NoError(t, h.ReplaceFileIndexFailures("repo", []graph.FileIndexFailure{{Path: fixture.files[0], RepoPrefix: "repo", Error: "pending parser update"}}))
	}
	return fixture
}

func closureRows(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(query, args...)
	require.NoError(t, err)
	columns, err := rows.Columns()
	require.NoError(t, err)
	var result []string
	for rows.Next() {
		values, pointers := make([]any, len(columns)), make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		require.NoError(t, rows.Scan(pointers...))
		encoded, err := json.Marshal(values)
		require.NoError(t, err)
		result = append(result, string(encoded))
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	slices.Sort(result)
	return result
}

// Compare every physical node/edge column, including binary metadata and
// generated/promoted columns. FTS documents use ownership keys, not docids.
func closureState(t *testing.T, s *Store) map[string][]string {
	t.Helper()
	result := map[string][]string{
		"nodes":             closureRows(t, s.db, `SELECT * FROM nodes`),
		"edges":             closureRows(t, s.db, `SELECT * FROM edges`),
		"symbol_documents":  closureRows(t, s.db, `SELECT o.view_gen, o.node_id, o.repo_prefix, f.* FROM symbol_fts_rowid o JOIN symbol_fts f ON f.rowid=o.fts_rowid`),
		"content_documents": closureRows(t, s.db, `SELECT o.view_gen, o.repo_prefix, o.file_path, f.* FROM content_fts_rowid o JOIN content_fts f ON f.rowid=o.fts_rowid`),
	}
	for _, sidecar := range viewGenSidecars {
		switch sidecar.table {
		case "symbol_fts_rowid", "content_fts_rowid":
			continue // docids are allocation identities, compared by ownership above
		default:
			result[sidecar.table] = closureRows(t, s.db, `SELECT * FROM `+sidecar.table)
		}
	}
	return result
}

func closureStatistics(t *testing.T, s *Store, regime string) {
	t.Helper()
	_, err := s.writerDB.Exec(`ANALYZE sqlite_schema; DELETE FROM sqlite_stat1; DELETE FROM sqlite_stat4`)
	require.NoError(t, err)
	switch regime {
	case "none":
	case "repaired":
		repaired, reason := healPlannerStats(s.writerDB)
		require.True(t, repaired, reason)
	case "fresh":
		_, err := s.writerDB.Exec(`ANALYZE`)
		require.NoError(t, err)
	default:
		t.Fatalf("unknown statistics regime %q", regime)
	}
	_, err = s.writerDB.Exec(`ANALYZE sqlite_schema`)
	require.NoError(t, err)
	recycleStatsReadPool(s.db, s.writerDB)
}

func TestContractFileEvictionClosureProjectionIsFrontierDriven(t *testing.T) {
	for _, count := range []int{1, 32} {
		for _, regime := range []string{"none", "repaired", "fresh"} {
			t.Run(fmt.Sprintf("files=%d/%s", count, regime), func(t *testing.T) {
				// Given doomed canonical sources, duplicate targets and unrelated generations.
				f := newClosureFixture(t, count)
				closureStatistics(t, f.store, regime)
				roots, ok := projectionJSON(f.roots)
				require.True(t, ok)
				args := []any{roots, int64(0), string(graph.EdgeProvides), string(graph.EdgeConsumes), string(graph.EdgeHandlesRoute)}
				// When explaining the exact SQL passed by the production closure.
				rows, err := f.store.db.Query(`EXPLAIN QUERY PLAN `+contractFileEvictionClosureSQL, args...)
				require.NoError(t, err)
				var details []string
				for rows.Next() {
					var id, parent, unused int
					var detail string
					require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
					details = append(details, detail)
				}
				require.NoError(t, rows.Err())
				require.NoError(t, rows.Close())
				// Then JSON is outermost and every edges lookup is keyed by its source.
				require.NotEmpty(t, details)
				require.Contains(t, details[0], "SCAN doomed VIRTUAL TABLE", "%v", details)
				sourceSearch := false
				for _, detail := range details {
					require.NotContains(t, detail, "DISTINCT", "%v", details)
					fields := strings.Fields(detail)
					if len(fields) < 2 || (fields[1] != "e" && fields[1] != "edges") {
						continue
					}
					require.Equal(t, "SEARCH", fields[0], "%v", details)
					require.Contains(t, detail, "from_id=?", "no generation-only or kind-only access: %v", details)
					sourceSearch = true
				}
				require.True(t, sourceSearch, "%v", details)
				projected := closureRows(t, f.store.db, contractFileEvictionClosureSQL, args...)
				previous := closureRows(t, f.store.db, oldContractClosureSQL, args...)
				require.NotEmpty(t, projected, "a vacuous projection cannot verify this closure")
				// The existing discovered set also excludes an already-seen target. Decode
				// each newly discovered payload once despite the three edge kinds per root.
				discovered := map[string]bool{"env::SHARED": true}
				var unseen []string
				targets, err := f.store.db.Query(contractFileEvictionClosureSQL, args...)
				require.NoError(t, err)
				for targets.Next() {
					var id string
					require.NoError(t, targets.Scan(&id))
					if !discovered[id] {
						discovered[id] = true
						unseen = append(unseen, id)
					}
				}
				require.NoError(t, targets.Err())
				require.NoError(t, targets.Close())
				encoded, ok := projectionJSON(unseen)
				require.True(t, ok)
				payloads, err := f.store.db.Query(`SELECT `+lookupNodeCols+` FROM nodes WHERE view_gen=0 AND id IN (SELECT CAST(value AS TEXT) FROM json_each(?))`, encoded)
				require.NoError(t, err)
				var decoded []string
				for payloads.Next() {
					n, err := scanNodeCursor(payloads)
					require.NoError(t, err)
					require.Equal(t, map[string]any{"values": []any{"exact", float64(17)}}, n.Meta["nested"])
					decoded = append(decoded, n.ID)
				}
				require.NoError(t, payloads.Err())
				require.NoError(t, payloads.Close())
				require.NotEmpty(t, decoded)
				require.Greater(t, len(projected), len(previous), "fixture must exercise duplicate projected targets")
				require.Equal(t, previous, slices.Compact(slices.Clone(projected)))
				require.Equal(t, []string{"env::CHAIN"}, unseen)
				require.Equal(t, unseen, decoded, "one payload decode per unseen target")
			})
		}
	}
}

func TestContractFileEvictionClosurePreservesCompleteRows(t *testing.T) {
	for _, count := range []int{1, 32} {
		for _, regime := range []string{"none", "repaired", "fresh"} {
			t.Run(fmt.Sprintf("files=%d/%s", count, regime), func(t *testing.T) {
				// Given equal complete fixtures, one uses the actual previous query.
				candidate, previous := newClosureFixture(t, count), newClosureFixture(t, count)
				calls := useOldClosureQuery(t, previous.store)
				closureStatistics(t, candidate.store, regime)
				closureStatistics(t, previous.store, regime)
				beforeForeign := closureRows(t, candidate.store.db, `SELECT * FROM nodes WHERE view_gen=7`)
				beforeForeignEdges := closureRows(t, candidate.store.db, `SELECT * FROM edges WHERE view_gen=7`)
				require.Equal(t, closureState(t, previous.store), closureState(t, candidate.store))
				// When all requested files are evicted through the normal entry point.
				n, e := candidate.store.EvictFiles(candidate.files)
				oldN, oldE := previous.store.EvictFiles(previous.files)
				// Then complete graph, search, sidecars and freshness equal the old path.
				require.Positive(t, *calls, "the comparison must reach the outgoing closure")
				require.Equal(t, oldN, n)
				require.Equal(t, oldE, e)
				require.Equal(t, count*2+2, n)
				require.Equal(t, closureState(t, previous.store), closureState(t, candidate.store))
				require.Equal(t, previous.store.AnalysisMutationRevision(), candidate.store.AnalysisMutationRevision())
				require.Equal(t, previous.store.payloadInputAdminRevision.Load(), candidate.store.payloadInputAdminRevision.Load())
				require.Equal(t, beforeForeign, closureRows(t, candidate.store.db, `SELECT * FROM nodes WHERE view_gen=7`))
				require.Equal(t, beforeForeignEdges, closureRows(t, candidate.store.db, `SELECT * FROM edges WHERE view_gen=7`))
				require.Nil(t, candidate.store.GetNode("env::CHAIN"))
				require.Nil(t, candidate.store.GetNode("env::TAIL"))
				retained := candidate.store.GetNode("env::SHARED")
				require.NotNil(t, retained)
				require.Equal(t, true, retained.Meta["contract_owner_removed"])
				require.Len(t, candidate.store.GetInEdges(retained.ID), 1)
				require.Equal(t, "other/live", candidate.store.GetInEdges(retained.ID)[0].From)
				var bindings int
				require.NoError(t, candidate.store.db.QueryRow(`SELECT COUNT(*) FROM semantic_binding_types WHERE view_gen=0`).Scan(&bindings))
				require.Equal(t, 1, bindings)
			})
		}
	}
}

func TestContractFileEvictionClosureRollbackAndReopen(t *testing.T) {
	for _, count := range []int{1, 32} {
		for _, point := range []FaultPoint{FaultPayloadWriteBeforeCommit, FaultPayloadWriteAfterCommit} {
			t.Run(fmt.Sprintf("files=%d/%s", count, point), func(t *testing.T) {
				// Given identical durable state and the existing commit fault hook.
				stop := &faultStop{point: point}
				candidate := newClosureFixture(t, count, WithFaultHook(stop.hook))
				previous := newClosureFixture(t, count)
				before := closureState(t, candidate.store)
				previous.store.EvictFiles(previous.files)
				committed := closureState(t, previous.store)
				// When eviction is stopped immediately before or after its one commit.
				files, ok := projectionJSON(candidate.files)
				require.True(t, ok)
				revision := candidate.store.AnalysisMutationRevision()
				stop.armed.Store(true)
				_, _, err := candidate.store.evictByPredicateResult(evictFilesPredicate, files, evictThisGeneration)
				stop.requireStopped(t, "file closure eviction", err)
				require.Equal(t, revision, candidate.store.AnalysisMutationRevision(), "no post-commit witness on an operation that returned a fault")
				want := committed
				if point == FaultPayloadWriteBeforeCommit {
					want = before
				}
				require.Equal(t, want, closureState(t, candidate.store))
				// Then reopening finds exactly the old or complete new state, never half.
				require.NoError(t, candidate.store.Close())
				reopened, err := Open(candidate.path)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, reopened.Close()) })
				require.Equal(t, want, closureState(t, reopened))
				reopened.EvictFiles(candidate.files)
				require.Equal(t, committed, closureState(t, reopened), "resume converges without dropping failure/freshness sidecars")
			})
		}
	}
}

func TestContractFileEvictionClosureRepeatedReaddedAndScoped(t *testing.T) {
	for _, count := range []int{1, 32} {
		t.Run(fmt.Sprintf("files=%d", count), func(t *testing.T) {
			// Given a removed frontier and its untouched sibling generation.
			candidate, previous := newClosureFixture(t, count), newClosureFixture(t, count)
			useOldClosureQuery(t, previous.store)
			originalNodes, originalEdges := candidate.store.AllNodes(), candidate.store.AllEdges()
			for _, fixture := range []closureFixture{candidate, previous} {
				fixture.store.EvictFiles(fixture.files)
			}
			after := closureState(t, candidate.store)
			revision := candidate.store.AnalysisMutationRevision()
			// When missing/duplicate paths are evicted repeatedly, then no rows change.
			for _, fixture := range []closureFixture{candidate, previous} {
				files := append(slices.Clone(fixture.files), "", fixture.files[0], "missing.go")
				n, e := fixture.store.EvictFiles(files)
				require.Zero(t, n)
				require.Zero(t, e)
			}
			require.Equal(t, after, closureState(t, candidate.store))
			require.Equal(t, revision, candidate.store.AnalysisMutationRevision())
			// When the same identities are re-added and removed again, then both paths
			// retain the same complete state and the foreign generation still survives.
			for _, fixture := range []closureFixture{candidate, previous} {
				require.NoError(t, fixture.store.AddBatchCheckedContext(t.Context(), originalNodes, originalEdges))
				var documents []graph.SymbolFTSItem
				for _, node := range originalNodes {
					if node.Kind == graph.KindContract {
						documents = append(documents, graph.SymbolFTSItem{NodeID: node.ID, Tokens: "closuretoken " + node.ID})
					}
				}
				require.NoError(t, fixture.store.BatchUpsertSymbolFTS(documents))
				require.NoError(t, fixture.store.ReplaceSemanticBindingTypesForFiles("repo", fixture.files, []graph.SemanticBindingType{
					{Site: graph.SemanticBindingSite{RepoPrefix: "repo", FilePath: fixture.files[0], Line: 4, Name: "removed"}, TypeName: "ExactRemoved"},
				}))
				n, _ := fixture.store.EvictFiles(fixture.files)
				require.Equal(t, count*2+2, n)
			}
			require.Equal(t, closureState(t, previous.store), closureState(t, candidate.store))
			require.Equal(t, after, closureState(t, candidate.store))
		})
	}
}

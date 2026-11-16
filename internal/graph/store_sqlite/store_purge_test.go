package store_sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"github.com/zzet/gortex/internal/graph"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"modernc.org/sqlite"
)

// openPurgeStore opens a throwaway on-disk store for the hygiene tests.
func openPurgeStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "purge.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seedRepoRows inserts exactly one row keyed to prefix into nodes, edges,
// vectors, and every repo_prefix-keyed sidecar table, so a PurgeRepo /
// RekeyRepoPrefix test can assert each table is cleared/moved. Node/vector
// ids embed the prefix (`<prefix>::a.go::X`) so the node-id-keyed vectors +
// edges land in the prefix's scope. Uses raw SQL for exhaustiveness: some
// tables have no public setter.
func seedRepoRows(t *testing.T, db *sql.DB, prefix string) {
	t.Helper()
	nodeID := prefix + "::a.go::X"
	chunkID := nodeID + "#chunk0"
	exec := func(q string, args ...any) {
		t.Helper()
		_, err := db.Exec(q, args...)
		require.NoError(t, err, q)
	}
	exec(`INSERT INTO nodes (id, kind, name, file_path, repo_prefix) VALUES (?, 'function', 'X', 'a.go', ?)`, nodeID, prefix)
	// An edge from the repo's node to a shared '' global external: PurgeRepo
	// must delete this edge (its from_id is a repo node) but NEVER the ''
	// target node.
	exec(`INSERT INTO edges (from_id, to_id, kind) VALUES (?, 'external_call::dep:shared', 'calls')`, nodeID)
	exec(`INSERT INTO vectors (node_id, repo_prefix, parent_id, dims, vec) VALUES (?, ?, '', 1, X'00')`, nodeID, prefix)
	exec(`INSERT INTO vectors (node_id, repo_prefix, parent_id, dims, vec) VALUES (?, ?, ?, 1, X'00')`, chunkID, prefix, nodeID)

	exec(`INSERT INTO file_mtimes (repo_prefix, file_path, mtime_ns) VALUES (?, 'a.go', 123)`, prefix)
	exec(`INSERT INTO repo_index_state (repo_prefix, indexed_sha) VALUES (?, 'sha')`, prefix)
	exec(`INSERT INTO enrichment_state (repo_prefix, provider) VALUES (?, 'lsp')`, prefix)
	exec(`INSERT INTO clone_shingles (node_id, repo_prefix, shingles) VALUES (?, ?, X'00')`, nodeID, prefix)
	exec(`INSERT INTO constant_values (node_id, repo_prefix, file_path, value) VALUES (?, ?, 'a.go', 'v')`, nodeID, prefix)
	exec(`INSERT INTO files (repo_prefix, file_path, content_hash) VALUES (?, 'a.go', 'h')`, prefix)
	exec(`INSERT INTO ref_facts (repo_prefix, from_id, to_id, kind, line) VALUES (?, ?, 'a.go::Y', 'ref', 1)`, prefix, nodeID)
	exec(`INSERT INTO churn_enrichment (node_id, repo_prefix, commit_count) VALUES (?, ?, 3)`, nodeID, prefix)
	exec(`INSERT INTO coverage_enrichment (node_id, repo_prefix, coverage_pct) VALUES (?, ?, 0.5)`, nodeID, prefix)
	exec(`INSERT INTO release_enrichment (node_id, repo_prefix, added_in) VALUES (?, ?, 'v1')`, nodeID, prefix)
	exec(`INSERT INTO blame_enrichment (node_id, repo_prefix, email) VALUES (?, ?, 'a@b')`, nodeID, prefix)
	exec(`INSERT INTO symbol_fts (node_id, repo_prefix, tokens) VALUES (?, ?, 'x')`, nodeID, prefix)
	var symbolRowid int64
	require.NoError(t, db.QueryRow(`SELECT rowid FROM symbol_fts WHERE node_id = ?`, nodeID).Scan(&symbolRowid))
	exec(`INSERT INTO symbol_fts_rowid (node_id, repo_prefix, fts_rowid) VALUES (?, ?, ?)`, nodeID, prefix, symbolRowid)
	exec(`INSERT INTO content_fts (node_id, repo_prefix, file_path, ordinal, body) VALUES (?, ?, 'a.go', 0, 'body')`, nodeID, prefix)
	var contentRowid int64
	require.NoError(t, db.QueryRow(`SELECT rowid FROM content_fts WHERE node_id = ?`, nodeID).Scan(&contentRowid))
	exec(`INSERT INTO content_fts_rowid (fts_rowid, repo_prefix, file_path) VALUES (?, ?, 'a.go')`, contentRowid, prefix)
}

// countByPrefix reports how many rows a repo_prefix-keyed table holds for
// prefix. nodes and every sidecar carry a repo_prefix column.
func countByPrefix(t *testing.T, db *sql.DB, table, prefix string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE repo_prefix = ?`, prefix).Scan(&n))
	return n
}

// prefixKeyedTables is every repo_prefix-keyed table PurgeRepo/Rekey touch,
// minus nodes (asserted separately) — used to loop assertions.
var prefixKeyedTables = []string{
	"file_mtimes", "repo_index_state", "enrichment_state", "clone_shingles",
	"constant_values", "files", "ref_facts", "churn_enrichment",
	"coverage_enrichment", "release_enrichment", "blame_enrichment",
	"symbol_fts", "symbol_fts_rowid", "content_fts", "content_fts_rowid",
}

func TestPurgeRepo_ClearsEveryTable_LeavesOthersAndGlobals(t *testing.T) {
	s := openPurgeStore(t)
	// Two real repos plus a shared '' global-external node the purge must
	// never touch.
	seedRepoRows(t, s.writerDB, "repoA")
	seedRepoRows(t, s.writerDB, "repoB")
	_, err := s.writerDB.Exec(`INSERT INTO nodes (id, kind, name, file_path, repo_prefix) VALUES ('external_call::dep:shared', 'external', 'shared', '', '')`)
	require.NoError(t, err)

	require.NoError(t, s.PurgeRepo("repoA"))

	// repoA: nodes, edges, vectors, and every sidecar cleared.
	assert.Equal(t, 0, countByPrefix(t, s.db, "nodes", "repoA"), "repoA nodes gone")
	assert.Equal(t, 0, countByPrefix(t, s.db, "vectors", "repoA"), "repoA ordinary and chunk vectors gone")
	for _, tbl := range prefixKeyedTables {
		assert.Equal(t, 0, countByPrefix(t, s.db, tbl, "repoA"), "repoA %s cleared", tbl)
	}
	var edgesFromA int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM edges WHERE from_id LIKE 'repoA::%'`).Scan(&edgesFromA))
	assert.Equal(t, 0, edgesFromA, "repoA edges gone")

	// repoB untouched across the board.
	assert.Equal(t, 1, countByPrefix(t, s.db, "nodes", "repoB"), "repoB nodes intact")
	assert.Equal(t, 2, countByPrefix(t, s.db, "vectors", "repoB"), "repoB ordinary and chunk vectors intact")
	for _, tbl := range prefixKeyedTables {
		assert.Equal(t, 1, countByPrefix(t, s.db, tbl, "repoB"), "repoB %s intact", tbl)
	}

	// The shared '' global external survives — nothing may purge ''.
	assert.Equal(t, 1, countByPrefix(t, s.db, "nodes", ""), "'' global external survives")
}

func TestPurgeRepo_RefusesEmptyPrefix(t *testing.T) {
	s := openPurgeStore(t)
	seedRepoRows(t, s.writerDB, "")
	require.Error(t, s.PurgeRepo(""), "PurgeRepo must refuse the empty prefix (global externals / solo data)")
	// The '' rows are still there.
	assert.Equal(t, 1, countByPrefix(t, s.db, "file_mtimes", ""), "'' file_mtimes untouched by refused purge")
}

func TestOrphanRepoPrefixes_SidecarOnlyResidue(t *testing.T) {
	s := openPurgeStore(t)
	// gone: a repo whose NODES were evicted but whose sidecars linger — the
	// exact leaked-untrack shape (residue in file_mtimes + repo_index_state,
	// no nodes). live: a fully-present tracked repo.
	_, err := s.writerDB.Exec(`INSERT INTO file_mtimes (repo_prefix, file_path, mtime_ns) VALUES ('gone', 'x.go', 1)`)
	require.NoError(t, err)
	_, err = s.writerDB.Exec(`INSERT INTO repo_index_state (repo_prefix) VALUES ('gone')`)
	require.NoError(t, err)
	// A vector-only legacy leak must also be discoverable even when every graph
	// node and other sidecar for the repository is gone.
	_, err = s.writerDB.Exec(`INSERT INTO vectors (node_id, repo_prefix, parent_id, dims, vec) VALUES ('vector-only::ghost#chunk0', 'vector-only', 'vector-only::ghost', 1, X'00')`)
	require.NoError(t, err)
	seedRepoRows(t, s.writerDB, "live")
	// A '' row must never be reported as an orphan.
	_, err = s.writerDB.Exec(`INSERT INTO file_mtimes (repo_prefix, file_path, mtime_ns) VALUES ('', 'g.go', 1)`)
	require.NoError(t, err)

	orphans := s.OrphanRepoPrefixes([]string{"live"})
	assert.ElementsMatch(t, []string{"gone", "vector-only"}, orphans,
		"both sidecar-only and vector-only residue must be reported")

	// Case-fold safety net: a case-only spelling drift of a tracked repo is
	// NOT an orphan.
	assert.Empty(t, s.OrphanRepoPrefixes([]string{"LIVE", "GONE", "VECTOR-ONLY"}),
		"case-insensitive known set covers every prefix")
}

func TestRekeyRepoPrefix_MovesProvenanceDropsNodeIDKeyed(t *testing.T) {
	s := openPurgeStore(t)
	seedRepoRows(t, s.writerDB, "") // solo repo: everything under ''

	require.NoError(t, s.RekeyRepoPrefix("", "drools"))

	// Prefix/path-keyed provenance MOVED '' -> drools (so warm restart finds
	// the repo's mtimes under the new prefix instead of full-re-tracking).
	moveTables := []string{"file_mtimes", "files", "repo_index_state", "enrichment_state"}
	for _, tbl := range moveTables {
		assert.Equal(t, 0, countByPrefix(t, s.db, tbl, ""), "%s '' rows moved out", tbl)
		assert.Equal(t, 1, countByPrefix(t, s.db, tbl, "drools"), "%s rows now under new prefix", tbl)
	}

	// node_id-keyed tables DROPPED (their old ids are dangling after the
	// re-mint) — the FTS decision included.
	dropTables := []string{
		"clone_shingles", "constant_values", "ref_facts", "churn_enrichment",
		"coverage_enrichment", "release_enrichment", "blame_enrichment",
		"symbol_fts", "symbol_fts_rowid", "content_fts", "content_fts_rowid",
	}
	for _, tbl := range dropTables {
		assert.Equal(t, 0, countByPrefix(t, s.db, tbl, ""), "%s '' rows dropped", tbl)
		assert.Equal(t, 0, countByPrefix(t, s.db, tbl, "drools"), "%s NOT relabeled to new prefix", tbl)
	}
	assert.Equal(t, 0, countByPrefix(t, s.db, "vectors", ""), "old-prefix vectors dropped")
	assert.Equal(t, 0, countByPrefix(t, s.db, "vectors", "drools"), "vectors are not relabeled to new IDs")

	assert.Error(t, s.RekeyRepoPrefix("repoA", ""), "rekey INTO the empty prefix is refused")
}

// TestContentCrashWindow simulates the D4 kill-window at the store level:
// per-file delete+append leaves a mix of old+new content instead of an empty
// table, and the end-of-track sweep (keep = files that STREAMED content this
// run) reaps both files that vanished from disk and files that still exist
// but no longer yield content sections.
func TestContentCrashWindow(t *testing.T) {
	s := openPurgeStore(t)
	item := func(id, file, body string) graph.ContentFTSItem {
		return graph.ContentFTSItem{NodeID: id, FilePath: file, Ordinal: 0, Body: body}
	}
	// Prior full index: three content files present.
	require.NoError(t, s.AppendContent("r", []graph.ContentFTSItem{item("r::f1", "f1.md", "old one")}))
	require.NoError(t, s.AppendContent("r", []graph.ContentFTSItem{item("r::f2", "f2.md", "old two")}))
	require.NoError(t, s.AppendContent("r", []graph.ContentFTSItem{item("r::f3", "f3.md", "old three")}))

	// A new full index re-streams only f1 (crash before it reached the
	// rest): delete f1's rows then re-append. The other files' OLD rows must
	// survive — no empty-table window.
	require.NoError(t, s.WipeContentFileInRepo("r", "f1.md"))
	require.NoError(t, s.AppendContent("r", []graph.ContentFTSItem{item("r::f1", "f1.md", "new one")}))

	countFile := func(file string) int {
		var n int
		require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM content_fts WHERE repo_prefix='r' AND file_path=?`, file).Scan(&n))
		return n
	}
	assert.Equal(t, 1, countFile("f1.md"), "f1 refreshed (delete+append, not doubled)")
	assert.Equal(t, 1, countFile("f2.md"), "f2's old rows survive the mid-parse kill (no empty table)")
	assert.Equal(t, 1, countFile("f3.md"), "f3's old rows survive the mid-parse kill (no empty table)")

	// The next SUCCESSFUL completion: f2 was deleted from the repo, and f3
	// STILL EXISTS on disk but was emptied — it streamed no content sections
	// this run, so it is absent from the streamed set. The sweep keeps
	// exactly the streamed set {f1}, reaping both the vanished file and the
	// content->no-content transition (a disk-survival keep would have
	// protected f3's stale rows forever).
	require.NoError(t, s.DeleteContentFilesForRepoNotIn("r", map[string]struct{}{"f1.md": {}}))
	assert.Equal(t, 1, countFile("f1.md"), "still-streaming file kept")
	assert.Equal(t, 0, countFile("f2.md"), "vanished file swept")
	assert.Equal(t, 0, countFile("f3.md"), "content->no-content transition swept despite surviving on disk")

	// Empty keep is a deliberate no-op (the never-wipe-from-empty safety
	// net; a zero-content walk routes to WipeContent instead).
	require.NoError(t, s.DeleteContentFilesForRepoNotIn("r", nil))
	assert.Equal(t, 1, countFile("f1.md"), "empty keep never wipes")
}

// purgeTableRows compares every column, including generation and physical IDs.
func purgeTableRows(t *testing.T, db *sql.DB) map[string][]string {
	t.Helper()
	out := make(map[string][]string)
	for _, table := range append([]string{"nodes", "edges"}, purgeSidecarTables...) {
		rows, err := db.Query(`SELECT * FROM ` + table)
		require.NoError(t, err)
		columns, err := rows.Columns()
		require.NoError(t, err)
		out[table] = nil
		for rows.Next() {
			values := make([]any, len(columns))
			targets := make([]any, len(columns))
			for i := range values {
				targets[i] = &values[i]
			}
			require.NoError(t, rows.Scan(targets...))
			encoded, err := json.Marshal(values)
			require.NoError(t, err)
			out[table] = append(out[table], string(encoded))
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		slices.Sort(out[table])
	}
	return out
}

func seedPurgeEdgeFixture(t *testing.T, s *Store) []string {
	t.Helper()
	seedRepoRows(t, s.writerDB, "repoA")
	seedRepoRows(t, s.writerDB, "repoB")
	ids := []string{"repoA::a.go::X", "quote'\"::路径::λ", "positive-only::A"}
	for _, gen := range []int{0, 7, 11} {
		for _, id := range ids {
			if gen == 0 && (id == ids[0] || id == ids[2]) {
				continue
			}
			_, err := s.writerDB.Exec(`INSERT INTO nodes (id, view_gen, kind, name, file_path, repo_prefix) VALUES (?, ?, 'function', 'A', 'a.go', 'repoA')`, id, gen)
			require.NoError(t, err)
		}
		for _, id := range ids {
			for _, endpoints := range [][2]string{{id, id}, {id, "repoB::a.go::X"}, {"repoB::a.go::X", id}} {
				_, err := s.writerDB.Exec(`INSERT INTO edges (from_id, to_id, kind, view_gen) VALUES (?, ?, 'calls', ?)`, endpoints[0], endpoints[1], gen)
				require.NoError(t, err)
			}
		}
		_, err := s.writerDB.Exec(`INSERT INTO edges (from_id, to_id, kind, view_gen) VALUES ('repoB::a.go::X', 'repoB::a.go::X', 'calls', ?)`, gen)
		require.NoError(t, err)
	}
	_, err := s.writerDB.Exec(`INSERT INTO file_mtimes (view_gen, repo_prefix, file_path, mtime_ns) VALUES (7, 'repoA', 'positive.go', 42), (11, 'repoB', 'retained.go', 43)`)
	require.NoError(t, err)
	return ids
}

func TestPurgeRepo_EdgeRowsMatchLegacyAcrossGenerations(t *testing.T) {
	// Given identical stores with duplicate and odd IDs, all edge directions,
	// positive-only nodes, and sidecars in base and positive generations.
	s := openPurgeStore(t)
	legacy := openPurgeStore(t)
	ids := seedPurgeEdgeFixture(t, s)
	seedPurgeEdgeFixture(t, legacy)
	require.Equal(t, purgeTableRows(t, legacy.db), purgeTableRows(t, s.db))

	// Freeze the base implementation's semantics independently of purge helpers:
	// delete edges by the fixture's known IDs, then sidecars and nodes by prefix.
	tx, err := legacy.writerDB.Begin()
	require.NoError(t, err)
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op
	for _, col := range []string{"from_id", "to_id"} {
		for _, id := range ids {
			_, err := tx.Exec(`DELETE FROM edges WHERE `+col+` = ?`, id)
			require.NoError(t, err)
		}
	}
	for _, table := range append(slices.Clone(purgeSidecarTables), "nodes") {
		_, err := tx.Exec(`DELETE FROM ` + table + ` WHERE repo_prefix = 'repoA'`)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	want := purgeTableRows(t, legacy.db)
	require.Len(t, want["edges"], 4, "three B→B generations and B→global survive")

	// When purge runs through a positive-generation handle, then the complete
	// surviving rows match the base operation, not just their counts.
	before := s.WriteMark()
	require.NoError(t, s.AtGeneration(7).PurgeRepo("repoA"))
	require.Equal(t, want, purgeTableRows(t, s.db))
	require.Equal(t, int64(28), s.WriteMark().Since(before).Rows.Edges, "each deleted edge is counted once")
	for _, table := range append([]string{"nodes"}, purgeSidecarTables...) {
		require.Zero(t, countByPrefix(t, s.db, table, "repoA"), table)
	}
	// Repeating a completed purge is a no-op, with no new mutation witness.
	revision := s.payloadInputAdminRevision.Load()
	require.NoError(t, s.PurgeRepo("repoA"))
	require.Equal(t, revision, s.payloadInputAdminRevision.Load())
	require.Equal(t, want, purgeTableRows(t, s.db))
}

type purgeEdgeStatement struct {
	query        string
	args         []any
	plan         []string
	rowsAffected int64
}

// This test-only driver observes real ExecContext calls without changing SQL.
type purgeEdgeConnector struct {
	dsn        string
	statements *[]purgeEdgeStatement
}

func (c purgeEdgeConnector) Driver() driver.Driver { return &sqlite.Driver{} }
func (c purgeEdgeConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.Driver().Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &purgeEdgeConn{Conn: conn, statements: c.statements}, nil
}

type purgeEdgeConn struct {
	driver.Conn
	statements *[]purgeEdgeStatement
}

func (c *purgeEdgeConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	result, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
	if err != nil || !strings.HasPrefix(query, "DELETE FROM edges WHERE ") {
		return result, err
	}
	statement := purgeEdgeStatement{query: query}
	for _, arg := range args {
		statement.args = append(statement.args, arg.Value)
	}
	statement.rowsAffected, err = result.RowsAffected()
	if err != nil {
		return nil, err
	}
	// Explain on the same connection while the purge's TEMP table still exists.
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, `EXPLAIN QUERY PLAN `+query, args)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read cursor only
	values := make([]driver.Value, 4)
	for {
		err := rows.Next(values)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		statement.plan = append(statement.plan, values[3].(string))
	}
	*c.statements = append(*c.statements, statement)
	return result, nil
}

func TestPurgeRepo_EdgeDeletePlansAreBounded(t *testing.T) {
	for _, count := range []int{1, 2000} {
		for _, edgeCount := range []int{30000, 30001} {
			t.Run(strconv.Itoa(count)+"/"+strconv.Itoa(edgeCount), func(t *testing.T) {
				// Given multiple incoming-edge chunks with gapped rowids, a whole
				// chunk with no matches, and an exact or partial final chunk.
				s := openPurgeStore(t)
				tx, err := s.writerDB.Begin()
				require.NoError(t, err)
				defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op
				for i := range count {
					id := strconv.Itoa(i)
					_, err := tx.Exec(`INSERT INTO nodes (id, kind, name, file_path, repo_prefix) VALUES (?, 'function', 'A', 'a.go', 'repoA')`, id)
					require.NoError(t, err)
					_, err = tx.Exec(`INSERT INTO edges (from_id, to_id, kind) VALUES (?, 'other', 'calls')`, id)
					require.NoError(t, err)
				}
				_, err = tx.Exec(`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i+1 < ?)
INSERT INTO edges (id, from_id, to_id, kind, line)
SELECT 100000+i*3, 'other', CASE WHEN i >= 10000 AND i < 20000 THEN 'kept' ELSE '0' END, 'calls', i FROM n`, edgeCount)
				require.NoError(t, err)
				require.NoError(t, tx.Commit())
				var statements []purgeEdgeStatement
				// Join the checkpoint loop first: its goroutine reads writerDB.
				s.stopCheckpointLoop()
				writer := sql.OpenDB(purgeEdgeConnector{dsn: s.dbPath, statements: &statements})
				writer.SetMaxOpenConns(1)
				s.writeMu.Lock()
				original := s.writerDB
				s.writerDB = writer
				s.writeMu.Unlock()
				t.Cleanup(func() {
					s.writeMu.Lock()
					s.writerDB = original
					s.writeMu.Unlock()
					require.NoError(t, writer.Close())
				})

				// When the purge executes, then outgoing deletes use bounded ID
				// seeks and incoming deletes visit each primary-key chunk once.
				require.NoError(t, s.PurgeRepo("repoA"))
				fromStatements, toStatements, emptyChunks := 0, 0, 0
				var previousUpper int64
				for _, statement := range statements {
					for _, detail := range statement.plan {
						require.NotContains(t, detail, "CORRELATED")
						require.NotContains(t, detail, "SCAN edges")
					}
					switch strings.Fields(statement.query)[4] {
					case "from_id":
						require.True(t, strings.HasPrefix(statement.query, "DELETE FROM edges WHERE from_id IN ("))
						fromStatements++
						require.NotEmpty(t, statement.args)
						require.LessOrEqual(t, len(statement.args), 900)
						require.Equal(t, len(statement.args), strings.Count(statement.query, "?"))
						require.Contains(t, strings.Join(statement.plan, "\n"), "SEARCH edges")
					case "id":
						require.True(t, strings.HasPrefix(statement.query, "DELETE FROM edges WHERE id > ? AND id <= ? AND to_id IN ("))
						toStatements++
						require.Len(t, statement.args, 2)
						require.Equal(t, previousUpper, statement.args[0])
						previousUpper = statement.args[1].(int64)
						wantUpper := int64(100000 + (minInt(toStatements*10000, edgeCount)-1)*3)
						require.Equal(t, wantUpper, previousUpper, "each range must cover at most 10,000 fixture rows")
						require.Contains(t, statement.plan, "SEARCH edges USING INTEGER PRIMARY KEY (rowid>? AND rowid<?)")
						require.LessOrEqual(t, statement.rowsAffected, int64(10000))
						if statement.rowsAffected == 0 {
							emptyChunks++
						}
					default:
						t.Fatalf("unbounded edge DELETE: %s; plan: %v", statement.query, statement.plan)
					}
				}
				// The exact multiple must not add an empty tail statement. The
				// empty middle chunk must not end the scan before later matches.
				require.Equal(t, (count+899)/900, fromStatements)
				require.Equal(t, (edgeCount+9999)/10000, toStatements)
				require.Equal(t, 1, emptyChunks)
				var remaining int
				require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM edges`).Scan(&remaining))
				require.Equal(t, 10000, remaining)
			})
		}
	}
}

func TestPurgeRepo_TempIDsDoNotLeakAcrossWriterReuse(t *testing.T) {
	installShadowDeleteFunction(t)
	s := openPurgeStore(t)
	seedRepoRows(t, s.writerDB, "gone")
	seedRepoRows(t, s.writerDB, "next")
	shadowDeleteTrigger(t, s, "nodes", "OLD.repo_prefix")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reached := false
	shadowDeleteObserver.Store(&shadowDeleteProbe{fire: func(prefix string) {
		if prefix == "gone" {
			reached = true
			cancel()
		}
	}})

	// Given cancellation in the final node DELETE, after edge work and
	// TEMP-table cleanup, the rollback must leave the same writer reusable.
	err := s.PurgeRepoContext(ctx, "gone")
	require.True(t, reached)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, countByPrefix(t, s.db, "nodes", "gone"))
	var tempTables int
	require.NoError(t, s.writerDB.QueryRow(`SELECT count(*) FROM temp.sqlite_master WHERE name='purge_node_ids'`).Scan(&tempTables))
	require.Zero(t, tempTables)
	_, err = s.writerDB.Exec(`DROP TRIGGER test_shadow_delete`)
	require.NoError(t, err)

	// When purging again after rollback, then both it and a later completed
	// purge succeed without a name clash or residual TEMP IDs on the writer.
	for _, prefix := range []string{"gone", "next"} {
		require.NoError(t, s.PurgeRepo(prefix))
		require.Zero(t, countByPrefix(t, s.db, "nodes", prefix))
		require.NoError(t, s.writerDB.QueryRow(`SELECT count(*) FROM temp.sqlite_master WHERE name='purge_node_ids'`).Scan(&tempTables))
		require.Zero(t, tempTables)
	}
}

func TestPurgeRepo_MissingPrefixPreservesRowsAndAnalysis(t *testing.T) {
	// Given a different repo and a ready persisted analysis generation.
	s := openPurgeStore(t)
	seedRepoRows(t, s.writerDB, "repoB")
	generation := buildMinimalAnalysisGeneration(t, s, "purge-missing", 0, true)
	before := purgeTableRows(t, s.db)
	analysisRevision := s.AnalysisMutationRevision()
	payloadRevision := s.payloadInputAdminRevision.Load()

	// When a missing prefix is purged, then neither rows nor analysis change.
	require.NoError(t, s.PurgeRepo("missing"))
	require.Equal(t, before, purgeTableRows(t, s.db))
	require.Equal(t, analysisRevision, s.AnalysisMutationRevision())
	require.Equal(t, payloadRevision, s.payloadInputAdminRevision.Load())
	require.True(t, s.analysisGenerationPresent)
	var state int
	require.NoError(t, s.db.QueryRow(`SELECT state FROM analysis_generations WHERE generation_id = ?`, generation).Scan(&state))
	require.Equal(t, analysisGenerationReady, state)
}

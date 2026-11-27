package resolver

import (
	"cmp"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestRebuildGoBuiltinsNativeSourceReplay(t *testing.T) {
	const file, caller = "repo/main.go", "repo/main.go::run"
	const wrongRecord = "repo/not-exact.go"
	const bare, qualified = "unresolved::append", "repo/unresolved::append"
	const appendID, lenID = "repo::builtin::go::append", "repo::builtin::go::len"
	const env = "GORTEX_RESOLVE_FROM_RECONCILE"
	nodes := []*graph.Node{
		{ID: file, Kind: graph.KindFile, FilePath: file, Language: "go", RepoPrefix: "repo", WorkspaceID: "ws", ProjectID: "proj"},
		{ID: caller, Name: "run", Kind: graph.KindFunction, FilePath: file, RepoPrefix: "repo", WorkspaceID: "ws", ProjectID: "proj"},
		{ID: wrongRecord, Kind: graph.KindFile, FilePath: "repo/different.go", RepoPrefix: "repo"},
	}
	for _, mode := range []struct {
		name       string
		namedGo    bool
		reconcile  bool
		nativeEnv  string
		privateEnv string
	}{
		{"repoint", true, true, "1", "0"},
		{"frozen-off", true, false, "0", "1"},
		{"unnamed-Go-only", false, true, "1", "0"},
	} {
		t.Run(mode.name, func(t *testing.T) {
			seed := slices.Clone(nodes)
			if mode.namedGo {
				// A named local, outside the caller's repo, suffices for the global guard.
				seed = append(seed, &graph.Node{ID: "other/decl.go::F#local:gate", Name: "gate", Kind: graph.KindLocal,
					FilePath: "other/decl.go", Language: "go", RepoPrefix: "other"})
			}
			var input, want []*graph.Edge
			for _, row := range []struct {
				from, to string
				kind     graph.EdgeKind
				line     int
				filePath string
				payload  string
			}{
				// The incumbent consumes the target update, not its intended repoint.
				{caller, bare, graph.EdgeReferences, 7, file, "old reference"},
				{caller, appendID, graph.EdgeReferences, 7, file, "incumbent reference"},
				{caller, bare, graph.EdgeReferences, 8, file, "second reference"},
				// Insert qualified first. The loose destination takes the bare payload.
				{qualified, "unresolved::len", graph.EdgeArgOf, 7, file, "qualified loose"},
				{bare, "unresolved::len", graph.EdgeArgOf, 7, file, "bare loose"},
				{qualified, "unresolved::len", graph.EdgeArgOf, 8, file, "qualified blocked"},
				{bare, "unresolved::len", graph.EdgeArgOf, 8, file, "bare blocked"},
				{appendID, lenID, graph.EdgeArgOf, 8, file, "incumbent arg"},
				{qualified, "unresolved::len", graph.EdgeArgOf, 9, file, "wrong line"},
				{bare, caller, graph.EdgeArgOf, 7, "repo/other.go", "wrong file"},
				{"foreign/" + bare, "unresolved::len", graph.EdgeArgOf, 7, file, "foreign file boundary"},
				// The node found by exact ID does not describe this recording file.
				{"foreign/unresolved::value", "unresolved::new", graph.EdgeArgOf, 7, wrongRecord, "no exact file"},
			} {
				// SQLite would synthesize named Go sentinels for seeded builtin targets.
				if !mode.namedGo && (row.to == appendID || row.to == lenID) {
					continue
				}
				e := &graph.Edge{From: row.from, To: row.to, Kind: row.kind, FilePath: row.filePath, Line: row.line,
					Confidence: 0.75, ConfidenceLabel: "confirmed", Origin: "fixture", Tier: "semantic", CrossRepo: true,
					Meta: map[string]any{"payload": row.payload}}
				input = append(input, e)
				saved := *e
				if mode.namedGo {
					switch row.payload {
					case "old reference":
						continue
					case "second reference":
						saved.To = appendID
					case "no exact file":
						saved.To = "foreign::builtin::go::new"
					case "bare loose", "qualified loose", "bare blocked", "qualified blocked", "wrong line":
						saved.To = lenID
						if mode.reconcile && row.line != 9 {
							if row.payload != "bare loose" {
								continue
							}
							saved.From = appendID
						}
					}
				}
				want = append(want, &saved)
			}
			rebuildFnPointerSortRows(input)
			rebuildFnPointerSortRows(want)

			t.Setenv(env, mode.nativeEnv)
			sql := openRebuildGoBuiltinSQLite(t)
			require.NoError(t, sql.AddBatchChecked(seed, input))
			require.Equal(t, mode.namedGo, sql.HasLanguage("go"))
			require.Equal(t, input, rebuildGoBuiltinSQLiteRows(sql))
			New(sql).attributeGoBuiltins()
			native := rebuildGoBuiltinSQLiteRows(sql)
			require.Equal(t, want, native)

			// The worker uses its frozen bool, not the current environment.
			t.Setenv(env, mode.privateEnv)
			build := openRebuildBareScopeFixture(t)
			build.AddBatch(seed, input)
			require.Equal(t, input, rebuildFnPointerSaved(t, build))
			require.NoError(t, runRebuildGoBuiltins(t.Context(), build, mode.reconcile, 1))
			require.Equal(t, native, rebuildFnPointerSaved(t, build))
			if mode.namedGo {
				rebuildGoBuiltinRequireAbsent(t, build, sql, rebuildGoBuiltinSentinel(lenID, "len", "ws", "proj"))
			}
			_, err := build.OpenSnapshot()
			require.ErrorIs(t, err, store_pebble.ErrUnpublishedBuild)
		})
	}
}

func TestRebuildGoBuiltinsNodeFreeOrder(t *testing.T) {
	const file = "repo/record.txt"
	const a, b = "repo/a.txt::A", "repo/b.go::B"
	const lenID, appendID, capID = "repo::builtin::go::len", "repo::builtin::go::append", "repo::builtin::go::cap"
	// Neither site ends in .go. A uses its owner node's language; B uses its source ID.
	kept := rebuildGoBuiltinSentinel(appendID, "append", "ws-a", "proj-a")
	kept.QualName, kept.StartLine = "unchecked field", 42
	stale := rebuildGoBuiltinSentinel(capID, "cap", "old", "old")
	stale.QualName, stale.StartLine = "replace this field too", 43
	nodes := []*graph.Node{
		{ID: a, Name: "A", Kind: graph.KindFunction, FilePath: "repo/a.txt", Language: "go", RepoPrefix: "repo", WorkspaceID: "ws-a", ProjectID: "proj-a"},
		{ID: b, Name: "B", Kind: graph.KindFunction, FilePath: "repo/b.go", Language: "typescript", RepoPrefix: "repo", WorkspaceID: "ws-b", ProjectID: "proj-b"},
		kept, stale,
	}
	var input, want []*graph.Edge
	for _, row := range []struct{ from, name, target string }{
		{a + "#local:arg", "len", lenID},
		{b, "len", lenID},
		{a, "append", appendID},
		{a, "cap", capID},
	} {
		e := &graph.Edge{From: row.from, To: "unresolved::" + row.name, Kind: graph.EdgeCalls, FilePath: file, Line: 7,
			Meta: map[string]any{"payload": row.from + " -> " + row.name}}
		input = append(input, e)
		saved := *e
		saved.To = row.target
		want = append(want, &saved)
	}
	rebuildFnPointerSortRows(want)

	for _, reverse := range []bool{false, true} {
		order, nativeLen := "A-first", rebuildGoBuiltinSentinel(lenID, "len", "ws-a", "proj-a")
		arrival := slices.Clone(input)
		if reverse {
			order, nativeLen = "B-first", rebuildGoBuiltinSentinel(lenID, "len", "ws-b", "proj-b")
			slices.Reverse(arrival)
		}
		t.Run(order, func(t *testing.T) {
			sql := openRebuildGoBuiltinSQLite(t)
			require.NoError(t, sql.AddBatchChecked(nodes, arrival))
			New(rebuildGoBuiltinSQLiteOrder{Store: sql, reverse: reverse}).attributeGoBuiltins()
			require.Equal(t, want, rebuildGoBuiltinSQLiteRows(sql))
			require.Equal(t, nativeLen, sql.GetNode(lenID), "native follows the forced per-target reader order")

			build := openRebuildBareScopeFixture(t)
			build.AddBatch(nodes[:2], arrival)
			require.NoError(t, runRebuildGoBuiltins(t.Context(), build, true, 1))
			require.Equal(t, want, rebuildFnPointerSaved(t, build))
			rebuildGoBuiltinRequireAbsent(t, build, sql, nativeLen)
			rebuildGoBuiltinRequireAbsent(t, build, sql, kept)
			rebuildGoBuiltinRequireAbsent(t, build, sql, rebuildGoBuiltinSentinel(capID, "cap", "ws-a", "proj-a"))
		})
	}
}

func rebuildGoBuiltinSentinel(id, name, workspace, project string) *graph.Node {
	return &graph.Node{ID: id, Name: name, Kind: graph.KindBuiltin, Language: "go", RepoPrefix: "repo",
		WorkspaceID: workspace, ProjectID: project, Meta: map[string]any{"builtin": true, "builtin_kind": "func"}}
}

func rebuildGoBuiltinRequireAbsent(t *testing.T, build *store_pebble.BuildStore, sql *store_sqlite.Store, want *graph.Node) {
	t.Helper()
	require.Equal(t, want, sql.GetNode(want.ID))
	saved, err := build.RebuildView().Node(t.Context(), want.ID)
	require.NoError(t, err)
	require.Nil(t, saved)
}

func openRebuildGoBuiltinSQLite(t *testing.T) *store_sqlite.Store {
	t.Helper()
	sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sql.Close()) })
	return sql
}

func rebuildGoBuiltinSQLiteRows(sql *store_sqlite.Store) []*graph.Edge {
	rows := sql.AllEdges()
	rebuildFnPointerSortRows(rows)
	return rows
}

// SQLite's incoming projection has no ORDER BY. Force its per-target order
// only; node reads, target collision handling and persisted payload are native.
type rebuildGoBuiltinSQLiteOrder struct {
	*store_sqlite.Store
	reverse bool
}

func (s rebuildGoBuiltinSQLiteOrder) GetInEdgeIdentitiesByNodeIDs(ids []string) map[string][]graph.EdgeIdentity {
	rows := s.Store.GetInEdgeIdentitiesByNodeIDs(ids)
	for _, group := range rows {
		slices.SortFunc(group, func(a, b graph.EdgeIdentity) int { return cmp.Compare(a.From, b.From) })
		if s.reverse {
			slices.Reverse(group)
		}
	}
	return rows
}

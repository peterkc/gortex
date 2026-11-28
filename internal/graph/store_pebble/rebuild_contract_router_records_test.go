package store_pebble

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
)

func TestRebuildRouterRecordMovesMatchCheckedRegistry(t *testing.T) {
	t.Run("one incoming source", func(t *testing.T) {
		testRebuildRouterRecordMovesMatchCheckedRegistry(t, false)
	})
	t.Run("two incoming sources", func(t *testing.T) {
		testRebuildRouterRecordMovesMatchCheckedRegistry(t, true)
	})
}

func testRebuildRouterRecordMovesMatchCheckedRegistry(t *testing.T, rootMove bool) {
	t.Helper()
	b, err := OpenBuild(t.TempDir(), FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	const rawID = "http::GET::/x"
	const intermediate = "http::GET::/api/x"
	const hidden = "http::GET::/local/x"
	record := func(id, path, file string, line int) contracts.Contract {
		return contracts.Contract{
			ID: id, Type: contracts.ContractHTTP, Role: contracts.RoleProvider, FilePath: file, SymbolID: file + "::router", Line: line,
			Meta: map[string]any{"framework": "fastapi/flask", "method": "GET", "path": path},
		}
	}
	input := []RebuildContractRecord{
		{Contract: record(rawID, "/x", "routes.py", 1)},
		{Contract: record(rawID, "/x", "routes.py", 2), BodyUnproved: true},
		{Contract: record(intermediate, "/api/x", "other.py", 1)},
		{Contract: record(intermediate, "/api/x", "other.py", 2), BodyUnproved: true},
		{Contract: record(intermediate, "/api/x", "other.py", 3)},
	}
	if rootMove {
		input = append(input, RebuildContractRecord{Contract: record("http::GET::/", "/", "root.py", 1)})
	}
	reg := contracts.NewRegistry()
	for _, item := range input {
		reg.Add(item.Contract)
	}
	receivers := func(line int, name string) contracts.RouterLineFacts {
		return contracts.RouterLineFacts{Line: line, Receivers: map[string]string{"fastapi/flask": name}}
	}
	files := map[string]contracts.RouterFacts{
		"routes.py": {LineCount: 2, Lines: []contracts.RouterLineFacts{receivers(1, "a"), receivers(2, "h")}},
		"other.py":  {LineCount: 3, Lines: []contracts.RouterLineFacts{receivers(1, "b"), {Line: 2}}},
		"mount.py":  {MountPrefix: map[string]string{"a": "/api", "h": "/local", "b": "/v2"}},
	}
	wantFiles := []string{"other.py", "routes.py"}
	if rootMove {
		files["root.py"] = contracts.RouterFacts{LineCount: 1, Lines: []contracts.RouterLineFacts{receivers(1, "r")}}
		files["mount.py"].MountPrefix["r"] = "/api/x"
		wantFiles = []string{"other.py", "root.py", "routes.py"}
	}
	want, missing := contracts.JoinRebuildRouterPrefixFacts(reg, files, nil)
	require.Empty(t, missing)
	require.Equal(t, wantFiles, want)
	// With rootMove, the clear drops the root move (/ < /api/x < /x).
	// In both cases it restores the two stationary rows, then the /x move
	// appends after them.
	require.Equal(t, []contracts.Contract{
		input[3].Contract, input[4].Contract, contracts.RewriteRouterContract(input[0].Contract, "/api"),
	}, reg.ByID(intermediate))
	prefix := func(c contracts.Contract) (string, error) {
		switch c.FilePath {
		case "root.py":
			return "/api/x", nil
		case "other.py":
			if c.Line != 1 {
				return "", nil
			}
			return "/v2", nil
		}
		if c.Line == 2 {
			return "/local", nil
		}
		return "/api", nil
	}
	stop := errors.New("missing checked mount")
	require.NoError(t, b.WithRebuildContractRecords(t.Context(), func(r *RebuildContractRecords) error {
		for _, item := range input {
			require.NoError(t, r.Add(item))
		}
		require.ErrorIs(t, r.RewriteRouterPrefixes(prefix, func() error { return stop }, func(string) error {
			t.Fatal("reported overlap for an unproved join")
			return nil
		}), stop)
		var before []RebuildContractRecord
		require.NoError(t, r.VisitByID(rawID, func(item RebuildContractRecord) error {
			before = append(before, item)
			return nil
		}))
		require.Equal(t, input[:2], before)
		var overlap []string
		require.NoError(t, r.RewriteRouterPrefixes(prefix, func() error { return nil }, func(path string) error {
			overlap = append(overlap, path)
			return nil
		}))
		require.Equal(t, want, overlap)
		var ids []string
		require.NoError(t, r.VisitIDs(func(id string) error {
			ids = append(ids, id)
			actual := make([]contracts.Contract, 0)
			require.NoError(t, r.VisitByID(id, func(item RebuildContractRecord) error {
				actual = append(actual, item.Contract)
				require.Equal(t, id == hidden || id == intermediate && item.Line == 2, item.BodyUnproved)
				return nil
			}))
			require.Equal(t, reg.ByID(id), actual)
			return nil
		}))
		require.ElementsMatch(t, reg.AllIDs(), ids)
		return nil
	}))
}

func TestRebuildRouterRecordUnopenedConvergence(t *testing.T) {
	b, err := OpenBuild(t.TempDir(), FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	reg := contracts.NewRegistry()
	facts := make(map[string]contracts.RouterFacts)
	require.NoError(t, b.WithRebuildContractRecords(t.Context(), func(r *RebuildContractRecords) error {
		for _, row := range []struct{ path, file, prefix string }{
			{"/x", "x.ts", "/api/part"},
			{"/part/x", "part.ts", "/api"},
			{"/api/part/x", "resident.ts", ""},
			// One source writes two rows to /solo/x. Neither file is affected.
			{"/x", "solo-a.ts", "/solo"},
			{"/x", "solo-b.ts", "/solo"},
		} {
			c := contracts.Contract{
				ID: "http::GET::" + row.path, Type: contracts.ContractHTTP, Role: contracts.RoleProvider, FilePath: row.file, Line: 1,
				Meta: map[string]any{"framework": "nestjs", "method": "GET", "path": row.path},
			}
			reg.Add(c)
			require.NoError(t, r.Add(RebuildContractRecord{Contract: c}))
			facts[row.file] = contracts.RouterFacts{LineCount: 1, Lines: []contracts.RouterLineFacts{{Line: 1, NestPrefix: row.prefix}}}
		}
		want := []string{"part.ts", "resident.ts", "x.ts"}
		files, missing := contracts.JoinRebuildRouterPrefixFacts(reg, facts, nil)
		require.Empty(t, missing)
		require.Equal(t, want, files)
		var overlap []string
		require.NoError(t, r.RewriteRouterPrefixes(func(c contracts.Contract) (string, error) {
			return facts[c.FilePath].Lines[0].NestPrefix, nil
		}, func() error { return nil }, func(path string) error {
			overlap = append(overlap, path)
			return nil
		}))
		require.Equal(t, want, overlap)
		const dest = "http::GET::/api/part/x"
		var rows []contracts.Contract
		require.NoError(t, r.VisitByID(dest, func(item RebuildContractRecord) error {
			rows = append(rows, item.Contract)
			return nil
		}))
		require.Equal(t, reg.ByID(dest), rows)
		require.Len(t, rows, 3)
		require.Equal(t, []string{"resident.ts", "part.ts", "x.ts"}, []string{rows[0].FilePath, rows[1].FilePath, rows[2].FilePath})
		return nil
	}))
}

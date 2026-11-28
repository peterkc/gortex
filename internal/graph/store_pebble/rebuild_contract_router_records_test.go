package store_pebble

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
)

func TestRebuildRouterRecordMovesMatchCheckedRegistry(t *testing.T) {
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
		"other.py":  {LineCount: 1, Lines: []contracts.RouterLineFacts{receivers(1, "b")}},
		"mount.py":  {MountPrefix: map[string]string{"a": "/api", "h": "/local", "b": "/v2"}},
	}
	want, missing := contracts.JoinRebuildRouterPrefixFacts(reg, files, "")
	require.Empty(t, missing)
	require.Equal(t, []string{"other.py", "routes.py"}, want)
	prefix := func(c contracts.Contract) (string, error) {
		if c.FilePath == "other.py" {
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
			var actual []contracts.Contract
			require.NoError(t, r.VisitByID(id, func(item RebuildContractRecord) error {
				actual = append(actual, item.Contract)
				require.Equal(t, id == hidden, item.BodyUnproved)
				return nil
			}))
			require.ElementsMatch(t, reg.ByID(id), actual)
			return nil
		}))
		require.ElementsMatch(t, reg.AllIDs(), ids)
		return nil
	}))
}

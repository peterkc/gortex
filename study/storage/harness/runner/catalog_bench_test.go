package runner

import (
	"context"
	"gxstudy/adapter"
	"gxstudy/oracle"
	"maps"
	"os"
	"path/filepath"
	"testing"
)

type catalogOnlyBackend struct{ oracle.Backend }

func (catalogOnlyBackend) Name() string { return "catalog-refs" }

type noOpCatalog struct{ adapter.Catalog }

func (*noOpCatalog) Close() error { return nil }
func (*noOpCatalog) Publish(context.Context, string, adapter.SegmentRef, *adapter.SegmentRef) error {
	return nil
}
func (*noOpCatalog) PublishSet(context.Context, adapter.View, adapter.View) error { return nil }
func (*noOpCatalog) Pin(context.Context, []string) (adapter.View, func(), error) {
	return adapter.View{}, func() {}, nil
}

func TestCatalogBenchmarkRejectsWrongCandidate(t *testing.T) {
	calls := 0
	factory := func(string) (adapter.Catalog, error) { calls++; return &noOpCatalog{}, nil }
	out := filepath.Join(t.TempDir(), "out")
	for _, candidate := range []string{"oracle", "catalog-sqlite"} {
		err := RunCatalogBenchmark(catalogOnlyBackend{}, factory, []string{"-candidate", candidate, "-out", out})
		if err == nil || calls != 0 {
			t.Fatal("wrong candidate was accepted or invoked", candidate, err, calls)
		}
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("rejected candidate created output", err)
	}
	if err := RunCatalogBenchmark(catalogOnlyBackend{}, nil, []string{"-candidate", "catalog-refs", "-out", out}); err == nil {
		t.Fatal("nil factory accepted")
	}
}

func TestCatalogBenchmarkRejectsNoOpPublication(t *testing.T) {
	root := t.TempDir()
	old, next, repos, _, err := catalogFixture(filepath.Join(root, "refs"), 1, 20261025)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	factory := func(string) (adapter.Catalog, error) { calls++; return &noOpCatalog{}, nil }
	_, _, _, err = catalogTrial("catalog-refs", factory, old, next, repos, root)
	if err == nil || calls != 1 {
		t.Fatal("unperformed publications inherited success", err, calls)
	}
}

func TestCatalogBenchmarkFixtureIsSealedAndDeterministic(t *testing.T) {
	root := t.TempDir()
	old, next, _, first, err := catalogFixture(filepath.Join(root, "a"), 1, 17)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, second, err := catalogFixture(filepath.Join(root, "b"), 1, 17)
	if err != nil || !maps.Equal(first, second) {
		t.Fatal(err, first, second)
	}
	for _, view := range []adapter.View{old, next} {
		for _, ref := range view {
			if err := oracle.VerifyRef(ref); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(ref.Path, "SEALED"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(ref.Path, "SEALED"), []byte("bad"), 0444); err != nil {
				t.Fatal(err)
			}
			if err := oracle.VerifyRef(ref); err == nil {
				t.Fatal("bad seal accepted")
			}
		}
	}
}

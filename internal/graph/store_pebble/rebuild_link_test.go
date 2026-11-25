package store_pebble

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func startLinkFixture(t *testing.T, view RebuildView, nodes []*graph.Node) (*RebuildLink, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	pass, err := v3FixturePass(t, ctx, rebuildCandidateCacheBytes, view, nodes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pass.Close() })
	return pass, ctx
}

func assertLinkReaped(t *testing.T, pass *RebuildLink) {
	t.Helper()
	if entries, err := os.ReadDir(filepath.Dir(pass.dir)); err != nil || len(entries) != 1 || entries[0].Name() != "keep" {
		t.Fatalf("parent scratch entries %v: %v", entries, err)
	}
}

// Raw names and IDs are byte strings, not UTF-8 input. A repeated identical
// native tuple must keep source ordinal order, even though file order differs.
func TestRebuildLinkEscapedOrder(t *testing.T) {
	a := &graph.Node{ID: "z\x00\xff", Name: "A\x00\xff", RepoPrefix: "r", Language: "go", Meta: map[string]any{"occurrence": 1}}
	b := *a
	b.Meta = map[string]any{"occurrence": 2}
	c := &graph.Node{ID: "a", Name: a.Name, RepoPrefix: "r", Language: "go"}
	pass, ctx := startLinkFixture(t, RebuildView{}, []*graph.Node{a, &b, c})
	var got []*graph.Node
	stats, err := pass.SelectCandidatePage(ctx, RebuildView{}, []LinkRequest{{Kind: LinkName, Key: a.Name, RepoPrefix: "r", Languages: []string{"go", "go"}}}, 1, func(pos int, n *graph.Node) error {
		if pos != 0 {
			t.Fatalf("position %d", pos)
		}
		got = append(got, n)
		return nil
	})
	if err != nil || stats.ResultRows != 3 || !reflect.DeepEqual(got, []*graph.Node{c, a, &b}) {
		t.Fatalf("got %v stats=%+v error=%v", got, stats, err)
	}
	if err := pass.Close(); err != nil {
		t.Fatal(err)
	}
	assertLinkReaped(t, pass)
}

func TestRebuildLinkSourceFailure(t *testing.T) {
	parent := t.TempDir()
	keep := filepath.Join(parent, "keep")
	if err := os.WriteFile(keep, []byte("owner data"), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := StartRebuildLink(t.Context(), RebuildLinkOptions{ScratchParent: parent, SpillBytes: 1 << 20}, RebuildView{}, func(yield func(*graph.Node) error) error {
		if err := yield(&graph.Node{ID: "a", Name: "A"}); err != nil {
			return err
		}
		return io.ErrUnexpectedEOF
	})
	if p != nil || !errors.Is(err, ErrRebuildLink) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("startup returned %v, %v", p, err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 1 || entries[0].Name() != "keep" {
		t.Fatalf("scratch %v: %v", entries, err)
	}
	data, err := os.ReadFile(keep)
	if err != nil || string(data) != "owner data" {
		t.Fatalf("owner data %q: %v", data, err)
	}
}

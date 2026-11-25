package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildCandidatePageOrder(t *testing.T) {
	goZ := &graph.Node{ID: "z", Name: "Run", QualName: "pkg.Run", RepoPrefix: "a", Language: "go"}
	python := &graph.Node{ID: "a", Name: "Run", QualName: "pkg.Run", RepoPrefix: "a", Language: "python"}
	goQ := &graph.Node{ID: "q", Name: "Run", QualName: "pkg.Run", RepoPrefix: "a", Language: "go"}
	old := &graph.Node{ID: "0", Name: "Run", QualName: "pkg.Run", RepoPrefix: "b", Language: "go"}
	nodes := []*graph.Node{goZ, old, python, goQ} // neither ID nor language order
	moved := &graph.Node{ID: "0", Name: "Moved", QualName: "pkg.Moved", RepoPrefix: "b", Language: "go"}
	synth := &graph.Node{ID: "b", Name: "Run", QualName: "pkg.Run", RepoPrefix: "b", Language: "go", Meta: map[string]any{"synthetic": true}}
	scoped := LinkRequest{Kind: LinkName, Key: "Run", RepoPrefix: "a", Languages: []string{"python", "go", "go"}}
	requests := []LinkRequest{
		scoped,
		{Kind: LinkName, Key: "Run", AllRepos: true},
		{Kind: LinkQualName, Key: "pkg.Run", AllRepos: true},
		{Kind: LinkName, Key: "miss", RepoPrefix: "a"},
		{Kind: LinkName, Key: "Run", RepoPrefix: "a"},
		{Kind: LinkName, Key: "Run", AllRepos: true, Languages: []string{"python", "go"}},
		{Kind: LinkName, Key: "Moved", AllRepos: true},
		scoped,
		{Kind: LinkQualName, Key: "pkg.Run", RepoPrefix: "a", Languages: []string{"go", "python"}},
		{Kind: LinkQualName, Key: "pkg.Moved", AllRepos: true},
		{Kind: LinkName, Key: "Run", RepoPrefix: "b"},
	}
	// The scopes use resolverNameScopeSort/p3SortNodes, not disk key order.
	// The ordinary qualified lookup includes the removed snapshot row, not
	// the new live row. Even a qualifying filtered qual read sorts by ID.
	want := [][]*graph.Node{
		{goQ, goZ, python}, {python, goQ, goZ, synth}, {old, python, goQ, goZ}, nil,
		{python, goQ, goZ}, {goQ, goZ, python, synth}, {moved}, {goQ, goZ, python}, {python, goQ, goZ}, nil, {old},
	}
	const wantRows = 26
	b := newBuild(t, FullProfile)
	b.AddBatch(nodes, nil)
	pass, ctx := startLinkFixture(t, b.RebuildView(), nodes)
	b.AddBatch([]*graph.Node{moved, synth}, nil)
	got := make([][]*graph.Node, len(requests))
	last := -1
	stats, err := pass.SelectCandidatePage(ctx, b.RebuildView(), requests, 1<<20, func(pos int, n *graph.Node) error {
		if pos < last || pos >= len(requests) {
			return fmt.Errorf("candidate position %d after %d", pos, last)
		}
		last = pos
		got[pos] = append(got[pos], n)
		return nil
	})
	if err != nil || stats.ResultRows != wantRows || stats.ResultBytes <= 0 {
		t.Fatalf("saved candidate page counts %+v: %v", stats, err)
	}
	for i := range want {
		if len(got[i]) != len(want[i]) {
			t.Errorf("position %d: got %d rows; want %d", i, len(got[i]), len(want[i]))
			continue
		}
		for j := range want[i] {
			if !reflect.DeepEqual(got[i][j], want[i][j]) {
				t.Errorf("position %d row %d: got %+v; want %+v", i, j, got[i][j], want[i][j])
			}
		}
	}
	assertCandidatePageScratch(t, pass)
}

func TestRebuildCandidatePageSpill(t *testing.T) {
	const rows = 384
	payload := strings.Repeat("payload#", 512)
	nodes := make([]*graph.Node, 0, rows)
	for i := range rows {
		nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("%04d", rows-i), Name: "Work", RepoPrefix: "repo",
			Meta: map[string]any{"payload": payload}})
	}
	requests := []LinkRequest{{Kind: LinkName, Key: "Work", RepoPrefix: "repo"}}
	pass, ctx := startLinkFixture(t, RebuildView{}, nodes)
	var first CandidatePageStats
	for _, threshold := range []int{1, 1 << 20} {
		t.Run(fmt.Sprintf("limit=%d", threshold), func(t *testing.T) {
			visits := 0
			stats, err := pass.SelectCandidatePage(ctx, RebuildView{}, requests, threshold, func(pos int, n *graph.Node) error {
				if pos != 0 || visits >= rows || !reflect.DeepEqual(n, nodes[rows-1-visits]) {
					return fmt.Errorf("unexpected candidate %q at visit %d (position %d)", n.ID, visits, pos)
				}
				visits++
				return nil
			})
			t.Logf("coordinator threshold=%d saved counts=%+v", threshold, stats)
			if err != nil || visits != rows || stats.ResultRows != rows || stats.ResultBytes <= 1<<20 {
				t.Fatalf("visits=%d counts=%+v: %v", visits, stats, err)
			}
			if threshold == 1 {
				first = stats
			} else if stats != first {
				t.Fatalf("logical charges differ by spill threshold: %+v vs %+v", first, stats)
			}
			assertCandidatePageScratch(t, pass)
		})
	}
}

func assertCandidatePageScratch(t *testing.T, pass *RebuildLink) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(pass.dir))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if !slices.Equal(names, []string{"keep", filepath.Base(pass.dir)}) {
		t.Fatalf("scratch entries %v", names)
	}
	owned, err := os.ReadDir(pass.dir)
	if err != nil || len(owned) != 1 || owned[0].Name() != "v3.sst" {
		t.Fatalf("unexpected candidate scratch %v: %v", owned, err)
	}
}

func TestRebuildCandidatePageFailure(t *testing.T) {
	visitorErr := errors.New("candidate consumer refused")
	for _, tc := range []struct {
		name       string
		cause      error
		message    string
		wantVisits int
	}{
		{name: "immutable", message: "checksum mismatch"},
		{name: "live", cause: ErrRebuildView, message: "stale working candidate index entry"},
		{name: "visitor", cause: visitorErr, wantVisits: 1},
		{name: "cancel", cause: context.Canceled, wantVisits: 1},
		{name: "request-bytes", message: "link request page exceeds wire byte bound"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			nodes := []*graph.Node{{ID: "a", Name: "A"}, {ID: "b", Name: "B"}, {ID: "c", Name: "B"}}
			b.AddBatch(nodes, nil)
			pass, ctx := startLinkFixture(t, b.RebuildView(), nodes)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			if tc.name == "immutable" {
				f, err := os.OpenFile(filepath.Join(pass.dir, "v3.sst"), os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.WriteAt(bytes.Repeat([]byte{0xff}, 8), 0)
				if err = errors.Join(err, f.Close()); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "live" {
				// Immutable checks already ran when the
				// live lookup encounters this dangling name index entry.
				key := keyStrings([]byte{familyName, nameRebuildScoped}, "A", "", "", "z")
				if err := b.pending.Set(key, []byte("z"), pebble.NoSync); err != nil {
					t.Fatal(err)
				}
			}
			requests := []LinkRequest{{Kind: LinkName, Key: "B"}, {Kind: LinkName, Key: "A", AllRepos: true}}
			if tc.name == "request-bytes" {
				requests[1].Key = strings.Repeat("a", linkFrameBytes)
			}
			visits, applied := 0, 0
			stats, err := pass.SelectCandidatePage(ctx, b.RebuildView(), requests, 1, func(_ int, _ *graph.Node) error {
				visits++
				switch tc.name {
				case "visitor":
					return visitorErr
				case "cancel":
					cancel()
				}
				return nil
			})
			if err == nil {
				applied += visits // only a complete successful page admits applies
			}
			if !errors.Is(err, ErrRebuildLink) || (tc.cause != nil && !errors.Is(err, tc.cause)) ||
				!strings.Contains(err.Error(), tc.message) || stats != (CandidatePageStats{}) || visits != tc.wantVisits || applied != 0 {
				t.Fatalf("visits=%d applies=%d stats=%+v: %v", visits, applied, stats, err)
			}
			assertLinkReaped(t, pass)
		})
	}
}

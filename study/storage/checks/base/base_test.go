package base

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cockroachdb/pebble/v2"
	"gxstudy/runner"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/blevesearch/bleve/v2"

	"gxstudy/adapter"
	"gxstudy/conformance"
	"gxstudy/dataset"
	"gxstudy/oracle"
	"gxstudy/workload"
)

func openFixture(t *testing.T, b Backend, d dataset.Dataset) (adapter.SegmentRef, *Segment) {
	t.Helper()
	ref, _, err := b.Build(context.Background(), d, filepath.Join(t.TempDir(), "version"))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := b.Open(context.Background(), ref.Path, ref)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := opened.Close(); err != nil {
			t.Error(err)
		}
	})
	return ref, opened.(*Segment)
}
func compareGraph(t *testing.T, d dataset.Dataset, s adapter.Segment) {
	t.Helper()
	ctx := context.Background()
	reference := oracle.New(d)
	var queries []workload.Query
	for _, n := range d.Nodes {
		queries = append(queries, workload.Query{Op: "id", Key: n.ID}, workload.Query{Op: "name", Key: n.Name, K: -1}, workload.Query{Op: "file", Key: n.FilePath}, workload.Query{Op: "in", Key: n.ID, Kind: "any"}, workload.Query{Op: "out", Key: n.ID, Kind: "any"}, workload.Query{Op: "traverse3", Key: n.ID, Depth: 3})
	}
	for _, e := range d.Edges {
		queries = append(queries, workload.Query{Op: "in", Key: e.To, Kind: e.Kind}, workload.Query{Op: "out", Key: e.From, Kind: e.Kind})
	}
	for _, op := range []string{"id", "name", "file", "in", "out"} {
		queries = append(queries, workload.Query{Op: op, Key: "not-present", K: -1, Kind: "any"})
	}
	for _, q := range queries {
		q.Repo = d.Repo
		want, err := workload.Evaluate(ctx, map[string]adapter.Segment{d.Repo: reference}, nil, q)
		if err != nil {
			t.Fatal(err)
		}
		got, err := workload.Evaluate(ctx, map[string]adapter.Segment{d.Repo: s}, nil, q)
		if err != nil {
			t.Fatal(err)
		}
		if check := workload.Compare(q, want, got); check.Mismatch {
			t.Fatalf("%s %q: %s", q.Op, q.Key, check.Detail)
		}
	}
}
func TestRoundTripAndLookupSemantics(t *testing.T) {
	for _, side := range []bool{false, true} {
		b := Backend{SideIndex: side}
		t.Run(b.Name(), func(t *testing.T) {
			// Given disk-backed full payloads, duplicate edges, nested paths and names,
			// when built/opened/reopened, then all Reader answers remain equal.
			conformance.CheckBackend(t, b)
			d := conformance.Fixture()
			d.Nodes[1].Name = d.Nodes[0].Name
			d.Nodes[1].FilePath = "nested/alias/pkg/f.go"
			d.Edges = append(d.Edges, d.Edges[0], dataset.Edge{From: "a", To: "foreign::unresolved", Kind: "calls", CrossRepo: true, Payload: map[string]json.RawMessage{"nullable": json.RawMessage(`null`), "nested": json.RawMessage(`{"values":[1,2]}`)}})
			slices.Reverse(d.Nodes)
			slices.Reverse(d.Edges)
			ref, s := openFixture(t, b, d)
			compareGraph(t, d, s)
			restored, err := s.records(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			assertRecords(t, d.Nodes, restored.Nodes)
			assertRecords(t, d.Edges, restored.Edges)
			assertRecords(t, d.Files, restored.Files)
			assertRecords(t, d.Symbols, restored.Symbols)
			assertRecords(t, d.Content, restored.Content)
			assertRecords(t, d.Vectors, restored.Vectors)
			contentIndex, err := s.TextIndex("content")
			if err != nil {
				t.Fatal(err)
			}
			if count, err := contentIndex.DocCount(); err != nil || count != uint64(len(d.Content)) {
				t.Fatal("content index missing", count, err)
			}
			borrowed, err := s.TextIndex("symbols")
			if err != nil {
				t.Fatal(err)
			}
			if borrowed.Index("bad", map[string]string{"text": "bad"}) == nil {
				t.Fatal("borrowed text index allowed write")
			}
			if borrowed.Delete("a") == nil {
				t.Fatal("borrowed text index allowed delete")
			}
			alias := bleve.NewIndexAlias(borrowed)
			answer, err := alias.Search(bleve.NewSearchRequest(bleve.NewMatchQuery("graph")))
			if err != nil || answer.Total != 1 {
				t.Fatal("borrowed index cannot join alias", answer, err)
			}
			if err := alias.Close(); err != nil {
				t.Fatal(err)
			}
			if err := borrowed.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := s.TextIndex("missing"); err == nil {
				t.Fatal("unknown text index accepted")
			}
			if len(restored.Files) != len(d.Files) || len(restored.Content) != len(d.Content) || len(restored.Symbols) != len(d.Symbols) || len(restored.Vectors) != len(d.Vectors) {
				t.Fatal("non-graph records omitted")
			}
			hits, err := s.SearchSymbols(context.Background(), "graph", 10)
			if err != nil || len(hits) != 1 || hits[0].ID != "a" {
				t.Fatal("symbol corpus mismatch", hits, err)
			}
			for _, k := range []int{0, 1, -1} {
				ns, err := s.NodesByName(context.Background(), "Alpha", k)
				if err != nil {
					t.Fatal(err)
				}
				want := 2
				if k >= 0 {
					want = k
				}
				if len(ns) != want {
					t.Fatal("name limit", k, len(ns))
				}
				if len(ns) > 1 && ns[0].ID > ns[1].ID {
					t.Fatal("name ordering")
				}
			}
			again, err := b.Open(context.Background(), ref.Path, ref)
			if err != nil {
				t.Fatal(err)
			}
			defer again.Close()
			compareGraph(t, d, again)
			// Deliberately missing a lookup or duplicate edge must fail the checker.
			want := workload.Answer{Nodes: d.Nodes}
			got := workload.Answer{Nodes: d.Nodes[:len(d.Nodes)-1]}
			if !workload.Compare(workload.Query{Op: "name"}, want, got).Mismatch {
				t.Fatal("dropped lookup accepted")
			}
			expected := workload.Answer{Edges: d.Edges}
			actual := workload.Answer{Edges: d.Edges[:len(d.Edges)-1]}
			if !workload.Compare(workload.Query{Op: "in"}, expected, actual).Mismatch {
				t.Fatal("dropped edge accepted")
			}
		})
	}
}
func TestReplacementLifecycleMatchesCleanData(t *testing.T) {
	for _, side := range []bool{false, true} {
		b := Backend{SideIndex: side}
		d := conformance.Fixture()
		old, oldReader := openFixture(t, b, d)
		changed := dataset.Dataset{Repo: d.Repo, Nodes: slices.Clone(d.Nodes[:2]), Files: slices.Clone(d.Files[:1])}
		cases := []struct {
			name        string
			replacement dataset.Dataset
		}{
			{"no-op", dataset.Dataset{Repo: d.Repo, Nodes: d.Nodes[:2], Edges: d.Edges[:2], Files: d.Files[:1], Symbols: d.Symbols, Content: d.Content, Vectors: d.Vectors}},
			{"delete", dataset.Dataset{Repo: d.Repo}},
			{"rename-body-dependency", changed},
			{"remove-readd-new-definition", dataset.Dataset{Repo: d.Repo, Nodes: []dataset.Node{{ID: "new", Name: "PreviouslyMissing", FilePath: "f.go", Language: "go"}}, Edges: []dataset.Edge{{From: "new", To: "outside", Kind: "calls", FilePath: "f.go"}}}},
		}
		for _, tc := range cases {
			t.Run(b.Name()+"/"+tc.name, func(t *testing.T) {
				replacement := tc.replacement
				if tc.name == "remove-readd-new-definition" {
					absent, err := oldReader.NodesByName(context.Background(), "PreviouslyMissing", -1)
					if err != nil || len(absent) != 0 {
						t.Fatal("failed lookup fixture invalid", absent, err)
					}
				}
				if tc.name == "rename-body-dependency" {
					replacement.Nodes = slices.Clone(replacement.Nodes)
					replacement.Nodes[0].Name = "renamed"
					replacement.Nodes[0].Payload = map[string]json.RawMessage{"body": json.RawMessage(`"changed body"`)}
					replacement.Edges = []dataset.Edge{{From: "a", To: "foreign::changed", Kind: "calls", FilePath: "f.go", CrossRepo: true}}
				}
				change := adapter.FileChange{Path: "f.go", Replacement: replacement}
				ref, stats, err := b.BuildDelta(context.Background(), old, change, filepath.Join(t.TempDir(), "next"))
				if err != nil {
					t.Fatal(err)
				}
				if stats.Mode != "full" {
					t.Fatal("rebuild mode hidden")
				}
				next, err := b.Open(context.Background(), ref.Path, ref)
				if err != nil {
					t.Fatal(err)
				}
				defer next.Close()
				compareGraph(t, adapter.ApplyChange(d, change), next)
				compareGraph(t, d, oldReader)
				if tc.name == "delete" {
					readded, _, err := b.BuildDelta(context.Background(), ref, adapter.FileChange{Path: "f.go", Replacement: cases[0].replacement}, filepath.Join(t.TempDir(), "readded"))
					if err != nil {
						t.Fatal(err)
					}
					restored, err := b.Open(context.Background(), readded.Path, readded)
					if err != nil {
						t.Fatal(err)
					}
					defer restored.Close()
					compareGraph(t, d, restored)
				}
			})
		}
		if err := oldReader.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
func TestRefusalsNeverProduceASeal(t *testing.T) {
	for _, side := range []bool{false, true} {
		b := Backend{SideIndex: side}
		t.Run(b.Name()+"/duplicate", func(t *testing.T) {
			d := conformance.Fixture()
			d.Nodes = append(d.Nodes, d.Nodes[0])
			dir := filepath.Join(t.TempDir(), "invalid")
			if _, _, err := b.Build(context.Background(), d, dir); err == nil {
				t.Fatal("duplicate ID accepted")
			}
			if _, err := os.Stat(filepath.Join(dir, "seal.json")); !os.IsNotExist(err) {
				t.Fatal("invalid build sealed", err)
			}
		})
		t.Run(b.Name()+"/cancel", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			dir := filepath.Join(t.TempDir(), "cancelled")
			if _, _, err := b.Build(ctx, conformance.Fixture(), dir); err == nil {
				t.Fatal("cancelled build accepted")
			}
		})
		ref, reader := openFixture(t, b, conformance.Fixture())
		t.Run(b.Name()+"/existing", func(t *testing.T) {
			if _, _, err := b.Build(context.Background(), conformance.Fixture(), ref.Path); err == nil {
				t.Fatal("overwrote sealed version")
			}
		})
		t.Run(b.Name()+"/symlink", func(t *testing.T) {
			alias := filepath.Join(t.TempDir(), "alias")
			if err := os.Symlink(ref.Path, alias); err != nil {
				t.Fatal(err)
			}
			bad := ref
			bad.Path = alias
			if _, err := b.Open(context.Background(), alias, bad); err == nil {
				t.Fatal("symlink alias accepted")
			}
		})
		t.Run(b.Name()+"/truncated", func(t *testing.T) {
			if err := os.Truncate(filepath.Join(ref.Path, "nodes.parquet"), 1); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Open(context.Background(), ref.Path, ref); err == nil {
				t.Fatal("truncated artifact accepted")
			}
		})
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func assertRecords[T any](t *testing.T, want, got []T) {
	t.Helper()
	encode := func(values []T) []string {
		result := make([]string, len(values))
		for i, value := range values {
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			result[i] = string(raw)
		}
		slices.Sort(result)
		return result
	}
	if !slices.Equal(encode(want), encode(got)) {
		t.Fatal("complete record multiset changed")
	}
}
func TestRangesCrossRowGroupAndStreamStops(t *testing.T) {
	d := dataset.Dataset{Repo: "groups"}
	for i := 0; i < 1030; i++ {
		d.Nodes = append(d.Nodes, dataset.Node{ID: fmt.Sprintf("n%04d", i), Name: "Shared", FilePath: "nested/f.go"})
	}
	b := Backend{}
	_, s := openFixture(t, b, d)
	nodes, err := s.NodesByName(context.Background(), "Shared", -1)
	if err != nil || len(nodes) != 1030 {
		t.Fatal("range crossed row group incorrectly", len(nodes), err)
	}
	last, err := s.NodeByID(context.Background(), "n1029")
	if err != nil || last.ID != "n1029" {
		t.Fatal("last row missing", err)
	}
	count := 0
	for _, err := range s.NodeRows(context.Background()) {
		if err != nil {
			t.Fatal(err)
		}
		count++
		break
	}
	if count != 1 {
		t.Fatal("early stream stop failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.FileNodes(ctx, "unknown"); err == nil {
		t.Fatal("cancelled missing lookup accepted")
	}
}

func countedRows[T any](values []T, calls *int) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		*calls = *calls + 1
		for _, value := range values {
			if !yield(value, nil) {
				return
			}
		}
	}
}
func TestSingleDecodeFanoutAndCounts(t *testing.T) {
	for _, sideMode := range []bool{false, true} {
		d := conformance.Fixture()
		root := t.TempDir()
		spools := map[string]*rowSpool{}
		for _, name := range []string{"nodes", "names", "paths", "out", "in", "files", "symbols", "content", "vectors"} {
			spool, err := newRowSpool(filepath.Join(root, name))
			if err != nil {
				t.Fatal(err)
			}
			spools[name] = spool
		}
		var side *sideWriter
		if sideMode {
			var err error
			side, err = newSideWriter(filepath.Join(root, "side"))
			if err != nil {
				t.Fatal(err)
			}
		}
		calls := [6]int{}
		symbols, content := 0, 0
		counts, err := ingestRows(context.Background(), inputRows{countedRows(d.Nodes, &calls[0]), countedRows(d.Edges, &calls[1]), countedRows(d.Files, &calls[2]), countedRows(d.Symbols, &calls[3]), countedRows(d.Content, &calls[4]), countedRows(d.Vectors, &calls[5])}, spools, side, func(string, string) error { symbols++; return nil }, func(string, string) error { content++; return nil })
		for _, spool := range spools {
			if closeErr := spool.close(); closeErr != nil {
				t.Fatal(closeErr)
			}
		}
		if side != nil {
			err = errors.Join(err, side.finish(), side.close())
		}
		if err != nil {
			t.Fatal(err)
		}
		if calls != [6]int{1, 1, 1, 1, 1, 1} || counts != d.Counts() || symbols != len(d.Symbols) || content != len(d.Content) {
			t.Fatal("source pass or count mismatch", calls, counts)
		}
	}
}
func TestVerificationRejectsDeliberatePayloadDigestChange(t *testing.T) {
	root := t.TempDir()
	scratch := filepath.Join(root, "scratch")
	if err := os.Mkdir(scratch, 0755); err != nil {
		t.Fatal(err)
	}
	spool, err := newRowSpool(filepath.Join(scratch, "nodes.input"))
	if err != nil {
		t.Fatal(err)
	}
	if err := spool.put(row{Key: "a", ID: "a", Raw: []byte(`{"id":"a","name":"Alpha"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := spool.close(); err != nil {
		t.Fatal(err)
	}
	verified, err := writeSortedTable(context.Background(), root, scratch, "nodes", true)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Rows != 1 || verified.InputSHA256 != verified.OutputSHA256 {
		t.Fatal("round trip was not verified", verified)
	}
	if _, err := verifyTable(context.Background(), root, "nodes", strings.Repeat("0", 64), 1); err == nil {
		t.Fatal("changed payload digest accepted")
	}
	if _, err := verifyTable(context.Background(), root, "nodes", verified.InputSHA256, 2); err == nil {
		t.Fatal("dropped row accepted")
	}
}
func TestRetainedMetadataConcurrentReadsAndClose(t *testing.T) {
	_, s := openFixture(t, Backend{}, conformance.Fixture())
	original := s.tables["in"].parquet
	file := s.tables["in"].file
	var wait sync.WaitGroup
	for range 8 {
		wait.Go(func() {
			for range 5 {
				edges, err := s.InEdges(context.Background(), "a", "calls")
				if err != nil || len(edges) != 1 {
					t.Error("concurrent retained read", edges, err)
				}
			}
		})
	}
	wait.Wait()
	if s.tables["in"].parquet != original {
		t.Fatal("query reopened/redecoded metadata")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Stat(); err == nil {
		t.Fatal("close leaked table file")
	}
}
func TestCacheChargesAllocatedShortBlocks(t *testing.T) {
	cache := newBlockCache(2 * blockSize)
	for i := 0; i < 10; i++ {
		path := filepath.Join(t.TempDir(), fmt.Sprint(i))
		if err := os.WriteFile(path, []byte{byte(i)}, 0600); err != nil {
			t.Fatal(err)
		}
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var value [1]byte
		if _, err := (cachedFile{file: file, cache: cache}).ReadAt(value[:], 0); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if cache.used != 2*blockSize || len(cache.entries) != 2 {
		t.Fatal("short blocks bypassed cache budget", cache.used, len(cache.entries))
	}
}
func TestExportedTextHelpersPreserveMappingAndRefuseMissingTerms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "text")
	if err := BuildText(context.Background(), path, func(add func(string, string) error) error { return add("a", "Alpha graph") }); err != nil {
		t.Fatal(err)
	}
	index, err := bleve.OpenUsing(path, map[string]interface{}{"read_only": true})
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	for _, tc := range []struct {
		term    string
		k, want int
	}{{"GRAPH", 10, 1}, {"absent", 10, 0}, {"graph", 0, 0}, {"graph", -1, 1}} {
		hits, err := SearchText(context.Background(), index, tc.term, tc.k)
		if err != nil || len(hits) != tc.want {
			t.Fatal("shared text behavior changed", tc, hits, err)
		}
	}
}

func TestOpenRejectsInconsistentVerificationRows(t *testing.T) {
	ref, segment := openFixture(t, Backend{}, conformance.Fixture())
	if err := segment.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ref.Path, "seal.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest seal
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	witness := manifest.Verification["nodes"]
	witness.Rows++
	manifest.Verification["nodes"] = witness
	raw, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	ref.Digest = hex.EncodeToString(hash[:])
	ref.Version = ref.Digest
	if opened, err := (Backend{}).Open(context.Background(), ref.Path, ref); err == nil {
		opened.Close()
		t.Fatal("inconsistent row witness accepted")
	}
}

func TestSealedSideStoreHasNoWALBytes(t *testing.T) {
	ref, _, err := (Backend{SideIndex: true}).Build(context.Background(), conformance.Fixture(), filepath.Join(t.TempDir(), "sealed"))
	if err != nil {
		t.Fatal(err)
	}
	if err := filepath.WalkDir(filepath.Join(ref.Path, "side"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if filepath.Ext(path) == ".log" {
			t.Fatalf("sealed side retained WAL entry: %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestBulkCleanupRejectsNativeRecordBearingWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal-on")
	store, err := pebble.Open(path, &pebble.Options{})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := os.Lstat(path)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	value := []byte("real-wal-payload-must-not-be-discarded")
	if err := store.Set([]byte("key"), value, pebble.Sync); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Flush(); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	logs, err := filepath.Glob(filepath.Join(path, "*.log"))
	if err != nil {
		t.Fatal(err)
	}
	foundRecord := false
	for _, name := range logs {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		foundRecord = foundRecord || bytes.Contains(raw, value)
	}
	if !foundRecord {
		t.Fatal("native fixture has no record-bearing WAL")
	}
	if err := runner.RemoveEmptyBuildWALs(path, identity); err == nil {
		t.Fatal("record-bearing native WAL accepted")
	}
	for _, name := range logs {
		if _, err := os.Lstat(name); err != nil {
			t.Fatal("rejected native WAL removed", name, err)
		}
	}
}

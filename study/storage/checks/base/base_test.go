package base

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cockroachdb/pebble/v2"
	"gxstudy/runner"
	"io"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/blevesearch/bleve/v2"
	"github.com/parquet-go/parquet-go"

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
		for _, name := range []string{"nodes", "ids", "names", "qnames", "paths", "edges", "out", "in", "terminals", "kinds", "files", "symbols", "content", "vectors"} {
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

func resealFixture(t *testing.T, ref adapter.SegmentRef) adapter.SegmentRef {
	t.Helper()
	var manifest seal
	if err := dataset.ReadJSON(filepath.Join(ref.Path, "seal.json"), &manifest); err != nil {
		t.Fatal(err)
	}
	for i := range manifest.Files {
		path := filepath.Join(ref.Path, manifest.Files[i].Name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		hash, err := dataset.Hash(path)
		if err != nil {
			t.Fatal(err)
		}
		manifest.Files[i].Size = info.Size()
		manifest.Files[i].SHA256 = hash
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(ref.Path, "seal.json"), raw, 0644); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	ref.Digest = hex.EncodeToString(hash[:])
	ref.Version = ref.Digest
	return ref
}
func checkSideLookupKey(t *testing.T, key []byte, nodes []dataset.Node) {
	t.Helper()
	if !bytes.HasPrefix(key, []byte("A/")) && !bytes.HasPrefix(key, []byte("Q/")) && !bytes.HasPrefix(key, []byte("P/")) {
		return
	}
	separator := bytes.IndexByte(key[2:], '/')
	if separator < 0 {
		t.Fatal("missing side lookup field separator", key)
	}
	suffix := key[3+separator:]
	if len(suffix) != 8 {
		t.Fatal("side lookup suffix is not a fixed-width integer", key)
	}
	for _, node := range nodes {
		if bytes.Contains(suffix, []byte(node.ID)) {
			t.Fatal("logical ID copied into side lookup suffix", node.ID, key)
		}
	}
}
func TestCanonicalPayloadAndIDOnlyInventory(t *testing.T) {
	for _, side := range []bool{false, true} {
		t.Run(fmt.Sprint(side), func(t *testing.T) {
			d := conformance.Fixture()
			d.Edges = append(d.Edges, d.Edges[0], dataset.Edge{From: "a", To: "external", Kind: "calls"})
			ref, s := openFixture(t, Backend{SideIndex: side}, d)
			compareGraph(t, d, s)
			for _, name := range []string{"nodes", "edges", "ids", "names", "qnames", "paths", "out", "in"} {
				count := 0
				err := s.scan(context.Background(), name, nil, func(r row) error {
					count++
					if r.ID != "" {
						t.Fatalf("logical ID copied into %s", name)
					}
					if name == "nodes" || name == "edges" {
						if len(r.Raw) == 0 || r.Key != "" {
							t.Fatalf("missing canonical payload or string primary key: %s", name)
						}
					} else if len(r.Raw) != 0 {
						t.Fatalf("lookup duplicates payload: %s", name)
					}
					if numericTable(name) && r.Key != "" {
						t.Fatalf("noninteger graph key: %s", name)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				want := len(d.Nodes)
				if name == "edges" || name == "out" || name == "in" {
					want = len(d.Edges)
				}
				if name == "ids" {
					want++
				}
				if count != want {
					t.Fatalf("%s rows=%d want=%d", name, count, want)
				}
			}
			rangeFiles, err := filepath.Glob(filepath.Join(ref.Path, "*.ranges.parquet"))
			if err != nil {
				t.Fatal(err)
			}
			var rangeNames []string
			for _, path := range rangeFiles {
				rangeNames = append(rangeNames, filepath.Base(path))
			}
			if !slices.Equal(rangeNames, []string{"names.ranges.parquet", "paths.ranges.parquet", "terminals.ranges.parquet"}) {
				t.Fatal("unused serving range directory", rangeNames)
			}
			count := s.seal.Verification["ids"].Rows
			for _, name := range []string{"out", "in"} {
				info, err := os.Stat(filepath.Join(ref.Path, name+".offsets"))
				if err != nil || info.Size() != 16+8*(count+1)*s.seal.Verification["kinds"].Rows {
					t.Fatal("missing dense N+1 offsets", info, err)
				}
			}
			if side {
				it, err := s.side.NewIter(nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if it != nil {
						it.Close()
					}
				})
				for ok := it.First(); ok; ok = it.Next() {
					if len(it.Value()) != 8 {
						t.Fatal("side index stores a payload")
					}
					checkSideLookupKey(t, it.Key(), d.Nodes)
				}
				err = errors.Join(it.Error(), it.Close())
				it = nil
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
func TestCorruptLocalIDsAndOffsetsRefuseBeforeServing(t *testing.T) {
	for _, side := range []bool{false, true} {
		for _, corrupt := range []string{"offsets", "duplicate-node", "duplicate-edge"} {
			t.Run(fmt.Sprintf("side=%v/%s", side, corrupt), func(t *testing.T) {
				b := Backend{SideIndex: side}
				ref, s := openFixture(t, b, conformance.Fixture())
				var rows []row
				name := "nodes"
				if corrupt == "duplicate-edge" {
					name = "edges"
				}
				if corrupt != "offsets" {
					if err := s.scan(context.Background(), name, nil, func(r row) error { rows = append(rows, r); return nil }); err != nil {
						t.Fatal(err)
					}
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				expected := "local ID"
				if corrupt == "offsets" {
					path := filepath.Join(ref.Path, "in.offsets")
					info, err := os.Stat(path)
					if err != nil {
						t.Fatal(err)
					}
					if err = os.Truncate(path, info.Size()-1); err != nil {
						t.Fatal(err)
					}
					expected = "truncated offset directory"
				} else {
					rows[1].Local = rows[0].Local
					file, err := os.Create(filepath.Join(ref.Path, name+".parquet"))
					if err != nil {
						t.Fatal(err)
					}
					writer := parquet.NewGenericWriter[row](file)
					if _, err = writer.Write(rows); err != nil {
						t.Fatal(err)
					}
					if err = errors.Join(writer.Close(), file.Close()); err != nil {
						t.Fatal(err)
					}
				}
				ref = resealFixture(t, ref)
				opened, err := b.Open(context.Background(), ref.Path, ref)
				if opened != nil {
					opened.Close()
					t.Fatal("corrupt sealed base served a reader")
				}
				if err == nil || !strings.Contains(err.Error(), expected) {
					t.Fatalf("missing named refusal %q: %v", expected, err)
				}
			})
		}
	}
}

func TestBinaryPayloadRoundTripsWithoutRecordJSON(t *testing.T) {
	d := conformance.Fixture()
	for _, node := range d.Nodes {
		raw, err := encodeNode(node)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodeNode(raw)
		if err != nil {
			t.Fatal(err)
		}
		assertRecords(t, []dataset.Node{node}, []dataset.Node{got})
		if _, err = decodeNode(raw[:len(raw)-1]); err == nil {
			t.Fatal("truncated payload accepted")
		}
	}
	for _, edge := range d.Edges {
		raw, err := encodeEdge(edge, 0, 1)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodeEdge(raw, func(id uint64) (string, error) {
			if id == 0 {
				return edge.From, nil
			}
			return edge.To, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		assertRecords(t, []dataset.Edge{edge}, []dataset.Edge{got})
	}
}
func TestDenseKindOffsetsIncludeZeroDegreeAndTerminals(t *testing.T) {
	dir := t.TempDir()
	file, err := os.Create(filepath.Join(dir, "out.ranges.parquet"))
	if err != nil {
		t.Fatal(err)
	}
	writer := parquet.NewGenericWriter[rowRange](file)
	ranges := []rowRange{{Numeric: true, Grouped: true, Tag: 0, Number: 1, First: 0, End: 2}, {Numeric: true, Grouped: true, Tag: 1, Number: 3, First: 2, End: 3}}
	if _, err = writer.Write(ranges); err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(writer.Close(), file.Close()); err != nil {
		t.Fatal(err)
	}
	if err = writeOffsets(context.Background(), dir, "out", 4, 2); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "out.offsets"))
	if err != nil {
		t.Fatal(err)
	}
	want := []uint64{4, 2, 0, 0, 2, 2, 2, 2, 2, 2, 2, 3}
	if len(raw) != 8*len(want) {
		t.Fatal("wrong N+1 directory length", len(raw))
	}
	for i, value := range want {
		if got := binary.LittleEndian.Uint64(raw[i*8:]); got != value {
			t.Fatalf("offset %d=%d want=%d", i, got, value)
		}
	}
}

func TestFixedWidthLocalColumnsAllowOpenTimeProjection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodes.parquet")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := parquet.NewGenericWriter[row](file)
	if _, err = writer.Write([]row{{Local: 0, Raw: []byte("payload once")}, {Local: 1, Raw: []byte("second payload")}}); err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(writer.Close(), file.Close()); err != nil {
		t.Fatal(err)
	}
	table, err := openTable(path, newBlockCache(4<<20))
	if err != nil {
		t.Fatal(err)
	}
	defer table.file.Close()
	columns := table.parquet.Schema().Columns()
	if fmt.Sprint(columns) != "[[key] [id] [local] [payload]]" {
		t.Fatal("redundant stored columns", columns)
	}
	for _, group := range table.parquet.Metadata().RowGroups {
		for _, column := range group.Columns {
			if slices.Equal(column.MetaData.PathInSchema, []string{"local"}) && column.MetaData.Codec.String() != "UNCOMPRESSED" {
				t.Fatal("compressed local IDs")
			}
		}
	}
	type localRow struct {
		Local uint64 `parquet:"local,plain,uncompressed"`
	}
	reader := parquet.NewGenericReader[localRow](table.parquet)
	defer reader.Close()
	got := make([]localRow, 2)
	n, err := reader.Read(got)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if n != 2 || got[0].Local != 0 || got[1].Local != 1 {
		t.Fatal("local ID projection changed", got, n)
	}
}

func TestLayoutValidationIsOncePerAbsolutePathAndSeal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "base")
	calls := 0
	valid := func() error { calls++; return nil }
	if err := validateLayoutOnce(path, "first", valid); err != nil {
		t.Fatal(err)
	}
	if err := validateLayoutOnce(path, "first", valid); err != nil || calls != 1 {
		t.Fatal("second open rescanned layout", calls, err)
	}
	if err := validateLayoutOnce(filepath.Join(filepath.Dir(path), "copy"), "first", valid); err != nil || calls != 2 {
		t.Fatal("new path reused validation", calls, err)
	}
	if err := validateLayoutOnce(path, "second", valid); err != nil || calls != 3 {
		t.Fatal("new seal reused validation", calls, err)
	}
	for _, message := range []string{"duplicate or non-dense local ID: nodes", "truncated offset directory: out"} {
		bad := func() error { calls++; return errors.New(message) }
		before := calls
		for range 2 {
			if err := validateLayoutOnce(path, message, bad); err == nil || err.Error() != message {
				t.Fatal("bad first open accepted", err)
			}
		}
		if calls != before+2 {
			t.Fatal("failed validation was cached", calls, before)
		}
	}
}
func TestOnlyServingOrOffsetBuildRangesAreWritten(t *testing.T) {
	for _, name := range []string{"nodes", "ids", "names", "qnames", "paths", "edges", "out", "in", "terminals", "kinds", "files", "symbols", "content", "vectors"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writer, file, err := newRangeDirectory(dir, name, nil)
			if err != nil {
				t.Fatal(err)
			}
			if writer != nil {
				if err = errors.Join(writer.Close(), file.Close()); err != nil {
					t.Fatal(err)
				}
			}
			_, err = os.Stat(filepath.Join(dir, name+".ranges.parquet"))
			expected := name == "names" || name == "paths" || name == "terminals" || name == "out" || name == "in"
			if expected && err != nil {
				t.Fatal("required build directory missing", err)
			}
			if !expected && !os.IsNotExist(err) {
				t.Fatal("unused directory written", name, err)
			}
		})
	}
}

func TestOffsetValidationChecksAcrossLargeBlockBoundary(t *testing.T) {
	dir := t.TempDir()
	tables := map[string]*tableFile{}
	for _, name := range []string{"nodes", "edges"} {
		file, err := os.Create(filepath.Join(dir, name+".parquet"))
		if err != nil {
			t.Fatal(err)
		}
		writer := parquet.NewGenericWriter[row](file)
		rows := []row{{Local: 0}}
		if name == "edges" {
			rows = append(rows, row{Local: 1})
		}
		if _, err = writer.Write(rows); err != nil {
			t.Fatal(err)
		}
		if err = errors.Join(writer.Close(), file.Close()); err != nil {
			t.Fatal(err)
		}
		table, err := openTable(file.Name(), newBlockCache(4<<20))
		if err != nil {
			t.Fatal(err)
		}
		tables[name] = table
		t.Cleanup(func() { table.file.Close() })
	}
	tables["out"], tables["in"] = tables["edges"], tables["edges"]
	count := uint64(8193)
	offsets := map[string]*os.File{}
	for _, name := range []string{"out", "in"} {
		values := make([]uint64, 2+count+1)
		values[0], values[1] = count, 1
		values[len(values)-1] = 2
		raw := make([]byte, len(values)*8)
		for i, value := range values {
			binary.LittleEndian.PutUint64(raw[i*8:], value)
		}
		path := filepath.Join(dir, name+".offsets")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		offsets[name] = file
		t.Cleanup(func() { file.Close() })
	}
	segment := Segment{dir: dir, tables: tables, offsets: offsets, seal: seal{Verification: map[string]tableVerification{"ids": {Rows: int64(count)}, "kinds": {Rows: 1}}}}
	if err := segment.validateLocalLayout(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The final block must not escape monotonicity validation.
	var bad [8]byte
	binary.LittleEndian.PutUint64(bad[:], 3)
	if _, err := offsets["out"].WriteAt(bad[:], 16+8192*8); err != nil {
		t.Fatal(err)
	}
	if err := segment.validateLocalLayout(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid offset directory") {
		t.Fatal("bad final block accepted", err)
	}
}

func TestNameLookupLimitsPayloadDecoding(t *testing.T) {
	for _, side := range []bool{false, true} {
		t.Run(fmt.Sprint(side), func(t *testing.T) {
			dir := t.TempDir()
			nodes := []dataset.Node{{ID: "z", Name: "same", FilePath: "f"}, {ID: "a", Name: "same", FilePath: "f"}, {ID: "a\x00!", Name: "same", FilePath: "f"}}
			for i := range nodes {
				nodes[i].Kind = "function"
				nodes[i].QualName = "pkg.same"
				nodes[i].Language = "go"
				nodes[i].StartLine = i + 1
				nodes[i].EndLine = i + 10
				nodes[i].Payload = map[string]json.RawMessage{"nested": json.RawMessage(`{"a":[1,"value"]}`)}
			}
			rows := make([]row, len(nodes))
			for i, node := range nodes {
				raw, err := encodeNode(node)
				if err != nil {
					t.Fatal(err)
				}
				rows[i] = row{Local: uint64(i), Raw: raw}
			}
			// Any attempt to decode the unselected third result fails, not just a count.
			rows[0].Raw = []byte("invalid unselected node payload")
			tables := map[string]*tableFile{}
			for _, table := range []struct {
				name string
				rows []row
			}{{"nodes", rows}, {"names", []row{{Key: "same", Local: 1}, {Key: "same", Local: 2}, {Key: "same", Local: 0}}}, {"paths", []row{{Key: "f", Local: 0}, {Key: "f", Local: 1}, {Key: "f", Local: 2}}}} {
				path := filepath.Join(dir, table.name+".parquet")
				file, err := os.Create(path)
				if err != nil {
					t.Fatal(err)
				}
				writer := parquet.NewGenericWriter[row](file)
				if _, err = writer.Write(table.rows); err != nil {
					t.Fatal(err)
				}
				if err = errors.Join(writer.Close(), file.Close()); err != nil {
					t.Fatal(err)
				}
				opened, err := openTable(path, newBlockCache(4<<20))
				if err != nil {
					t.Fatal(err)
				}
				tables[table.name] = opened
				t.Cleanup(func() { opened.file.Close() })
			}
			for _, name := range []string{"names", "paths"} {
				key := "same"
				if name == "paths" {
					key = "f"
				}
				path := filepath.Join(dir, name+".ranges.parquet")
				file, err := os.Create(path)
				if err != nil {
					t.Fatal(err)
				}
				writer := parquet.NewGenericWriter[rowRange](file)
				if _, err = writer.Write([]rowRange{{Key: key, First: 0, End: 3}}); err != nil {
					t.Fatal(err)
				}
				if err = errors.Join(writer.Close(), file.Close()); err != nil {
					t.Fatal(err)
				}
				opened, err := openTable(path, newBlockCache(4<<20))
				if err != nil {
					t.Fatal(err)
				}
				tables[name+".ranges"] = opened
				t.Cleanup(func() { opened.file.Close() })
			}
			segment := Segment{tables: tables, seal: seal{Meta: dataset.Manifest{Counts: dataset.Counts{Nodes: 3}}}}
			if side {
				writer, err := newSideWriter(filepath.Join(dir, "side"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if writer != nil {
						writer.close()
					}
				})
				for i, node := range nodes {
					if err = writer.add(node, uint64(i)); err != nil {
						t.Fatal(err)
					}
				}
				identity := writer.identity
				err = errors.Join(writer.finish(), writer.close())
				writer = nil
				if err != nil {
					t.Fatal(err)
				}
				if err = runner.RemoveEmptyBuildWALs(filepath.Join(dir, "side"), identity); err != nil {
					t.Fatal(err)
				}
				writer, err = newSideWriter(filepath.Join(dir, "side"))
				if err != nil {
					t.Fatal(err)
				}
				if err = writer.addNames(context.Background(), dir); err != nil {
					t.Fatal(err)
				}
				if err = writer.finish(); err != nil {
					t.Fatal(err)
				}
				it, err := writer.db.NewIter(nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if it != nil {
						it.Close()
					}
				})
				for ok := it.First(); ok; ok = it.Next() {
					if len(it.Value()) != 8 {
						t.Fatal("side index stores a payload")
					}
					checkSideLookupKey(t, it.Key(), nodes)
				}
				err = errors.Join(it.Error(), it.Close())
				it = nil
				if err != nil {
					t.Fatal(err)
				}
				segment.side = writer.db
			}
			for _, limit := range []int{1, 2} {
				got, err := segment.NodesByName(context.Background(), "same", limit)
				if err != nil {
					t.Fatal("bounded payload decode", limit, err)
				}
				actual, err := json.Marshal(got)
				if err != nil {
					t.Fatal(err)
				}
				expected, err := json.Marshal(nodes[1 : 1+limit])
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(actual, expected) {
					t.Fatal("bounded full payload/order changed", limit, string(actual), string(expected))
				}
			}
			if _, err := segment.NodesByName(context.Background(), "same", -1); err == nil {
				t.Fatal("unlimited name lookup skipped payload")
			}
			if _, err := segment.FileNodes(context.Background(), "f"); err == nil {
				t.Fatal("file lookup was incorrectly limited")
			}
			for _, tc := range []struct {
				name string
				k    int
			}{{"same", 0}, {"missing", 2}} {
				got, err := segment.NodesByName(context.Background(), tc.name, tc.k)
				if err != nil || len(got) != 0 {
					t.Fatal("empty name lookup", got, err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := segment.NodesByName(ctx, "same", 1); !errors.Is(err, context.Canceled) {
				t.Fatal("canceled name lookup accepted", err)
			}
		})
	}
}

func TestNameSortKeysUseLogicalIDsNotInputOrder(t *testing.T) {
	d := conformance.Fixture()
	d.Nodes = []dataset.Node{{ID: "z", Name: "same"}, {ID: "a\x00!", Name: "same"}, {ID: "a", Name: "same"}}
	d.Edges = nil
	dir := t.TempDir()
	spools := map[string]*rowSpool{}
	for _, name := range []string{"nodes", "ids", "names", "qnames", "paths", "edges", "out", "in", "terminals", "kinds", "files", "symbols", "content", "vectors"} {
		spool, err := newRowSpool(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		spools[name] = spool
	}
	calls := [6]int{}
	_, err := ingestRows(context.Background(), inputRows{countedRows(d.Nodes, &calls[0]), countedRows(d.Edges, &calls[1]), countedRows(d.Files, &calls[2]), countedRows(d.Symbols, &calls[3]), countedRows(d.Content, &calls[4]), countedRows(d.Vectors, &calls[5])}, spools, nil, func(string, string) error { return nil }, func(string, string) error { return nil })
	for _, spool := range spools {
		err = errors.Join(err, spool.close())
	}
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "names"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	slices.Sort(lines)
	var locals []string
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		id, err := hex.DecodeString(fields[1])
		if err != nil {
			t.Fatal(err)
		}
		locals = append(locals, string(id[len(id)-16:]))
	}
	if !slices.Equal(locals, []string{localKey(2), localKey(1), localKey(0)}) {
		t.Fatal("name references are not in logical-ID order", locals)
	}
}

func TestRebuiltNameLookupsPreserveLogicalIDOrder(t *testing.T) {
	for _, side := range []bool{false, true} {
		t.Run(fmt.Sprint(side), func(t *testing.T) {
			d := conformance.Fixture()
			d.Nodes[0].ID = "z"
			d.Nodes[1].ID = "a\x00!"
			d.Nodes[2].ID = "a"
			for i := range d.Nodes {
				d.Nodes[i].Name = "same"
			}
			backend := Backend{SideIndex: side}
			ref, segment := openFixture(t, backend, d)
			for i := 0; i < 2; i++ {
				for _, limit := range []int{-1, 0, 1, 2, 5} {
					got, err := segment.NodesByName(context.Background(), "same", limit)
					if err != nil {
						t.Fatal(err)
					}
					want, err := oracle.New(d).NodesByName(context.Background(), "same", limit)
					if err != nil {
						t.Fatal(err)
					}
					// Encode as the checker does (workload.Answer omits an empty
					// list, so nil and [] are equal) but without Canonical's sort,
					// so order and multiplicity still have to match.
					actual, err := json.Marshal(workload.Answer{Nodes: got})
					if err != nil {
						t.Fatal(err)
					}
					expected, err := json.Marshal(workload.Answer{Nodes: want})
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(actual, expected) {
						t.Fatal("persisted name payload/order", limit, string(actual), string(expected))
					}
				}
				if err := segment.Close(); err != nil {
					t.Fatal(err)
				}
				if i == 0 {
					opened, err := backend.Open(context.Background(), ref.Path, ref)
					if err != nil {
						t.Fatal(err)
					}
					segment = opened.(*Segment)
				}
			}
		})
	}
}

func TestGraphRowsDoNotPayForRedundantNumericColumns(t *testing.T) {
	type legacyRow struct {
		Key    string `parquet:"key,dict"`
		ID     string `parquet:"id,dict"`
		Tag    uint64 `parquet:"tag,plain,uncompressed"`
		Local  uint64 `parquet:"local,plain,uncompressed"`
		Number uint64 `parquet:"number,plain,uncompressed"`
		Raw    []byte `parquet:"payload"`
	}
	dir := t.TempDir()
	currentPath := filepath.Join(dir, "current.parquet")
	oldPath := filepath.Join(dir, "legacy.parquet")
	file, err := os.Create(currentPath)
	if err != nil {
		t.Fatal(err)
	}
	writer := parquet.NewGenericWriter[row](file)
	oldRows := make([]legacyRow, 128)
	rows := make([]row, 128)
	for i := range rows {
		rows[i] = row{Local: uint64(i), Raw: []byte("one payload")}
		oldRows[i] = legacyRow{Local: uint64(i), Number: uint64(i), Raw: rows[i].Raw}
	}
	if _, err = writer.Write(rows); err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(writer.Close(), file.Close()); err != nil {
		t.Fatal(err)
	}
	file, err = os.Create(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	oldWriter := parquet.NewGenericWriter[legacyRow](file)
	if _, err = oldWriter.Write(oldRows); err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(oldWriter.Close(), file.Close()); err != nil {
		t.Fatal(err)
	}
	current, err := os.Stat(currentPath)
	if err != nil {
		t.Fatal(err)
	}
	old, err := os.Stat(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if current.Size() >= old.Size() {
		t.Fatal("redundant numeric columns still cost bytes", current.Size(), old.Size())
	}
}

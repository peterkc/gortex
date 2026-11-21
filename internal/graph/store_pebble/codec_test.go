package store_pebble

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func TestCompactGraphCodec(t *testing.T) {
	node := &graph.Node{
		ID: "pkg/main.go::hello", Kind: graph.KindMethod, Name: "hello", QualName: "pkg.T.hello",
		FilePath: "pkg/main.go", StartLine: -1, EndLine: 33, StartColumn: 2, EndColumn: 5,
		Language: "go", RepoPrefix: "repo", WorkspaceID: "test", ProjectID: "example",
		AbsoluteFilePath: "/fixture/pkg/main.go", Origin: "remote:repo", Stub: true,
		FetchedAt: time.Date(2026, 11, 19, 20, 0, 1, 3, time.UTC),
		Meta:      map[string]any{"args": []string{"a", "b"}, "scope": int64(2)},
	}
	edge := &graph.Edge{
		From: node.ID, To: "external::x", Kind: graph.EdgeCalls, FilePath: node.FilePath, Line: 24,
		Confidence: 0.93, ConfidenceLabel: "medium", Origin: "ast_resolved", Tier: "ast",
		CrossRepo: true, Context: "call", ReturnUsage: "returned", Via: "dispatch", Alias: "x",
		NameOnly: true, Provisional: true, Meta: map[string]any{"route": "receiver_method", "count": 11},
	}
	nb, err := encodeNode(node)
	if err != nil {
		t.Fatal(err)
	}
	gotNode, err := decodeNode(nb)
	if err != nil || !reflect.DeepEqual(gotNode, node) {
		t.Fatalf("node round trip = %#v, %v; want %#v", gotNode, err, node)
	}
	eb, err := encodeEdge(edge)
	if err != nil {
		t.Fatal(err)
	}
	gotEdge, err := decodeEdge(eb)
	if err != nil || !reflect.DeepEqual(gotEdge, edge) {
		t.Fatalf("edge round trip = %#v, %v; want %#v", gotEdge, err, edge)
	}
	for size := 0; size < len(nb); size++ {
		if got, err := decodeNode(nb[:size]); got != nil || !errors.Is(err, errInvalidRecord) {
			t.Fatalf("node truncated to %d bytes = %#v, %v", size, got, err)
		}
	}
	for size := 0; size < len(eb); size++ {
		if got, err := decodeEdge(eb[:size]); got != nil || !errors.Is(err, errInvalidRecord) {
			t.Fatalf("edge truncated to %d bytes = %#v, %v", size, got, err)
		}
	}
	for _, bad := range [][]byte{append([]byte{0}, eb[1:]...), append(bytes.Clone(eb), 0), append([]byte{edgeFormat}, bytes.Repeat([]byte{0xff}, 10)...)} {
		if got, err := decodeEdge(bad); got != nil || !errors.Is(err, errInvalidRecord) {
			t.Fatalf("corrupt edge = %#v, %v", got, err)
		}
	}
}

func TestCanonicalEdgeKeys(t *testing.T) {
	first := &graph.Edge{From: "main", To: "x", Kind: graph.EdgeCalls, Line: -2}
	second := *first
	second.Line = 1
	if bytes.Compare(outEdgeKey(first, edgeIdentity(first)), outEdgeKey(&second, edgeIdentity(&second))) >= 0 {
		t.Fatal("signed line order did not survive key encoding")
	}
	second = *first
	second.Origin = "ast_resolved"
	if edgeIdentity(first) == edgeIdentity(&second) {
		t.Fatal("distinct origins share one encoded identity")
	}
	name := keyStrings([]byte{familyName}, "main")
	for _, tc := range []struct {
		name string
		want bool
	}{{"main", true}, {"mainland", false}, {"main\x00", false}} {
		key := keyStrings([]byte{familyName}, tc.name, "id")
		inside := bytes.Compare(key, name) >= 0 && bytes.Compare(key, prefixEnd(name)) < 0
		if inside != tc.want {
			t.Errorf("exact-name scan contains %q = %t, want %t", tc.name, inside, tc.want)
		}
	}
}

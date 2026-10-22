package store_sqlite

import (
	"bytes"
	"encoding/gob"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestDecodeReceiverMutationMetaFormats(t *testing.T) {
	t.Parallel()
	wantReceiver := "pkg.Type"
	wantField := "items"
	wantScope := "com.example"
	meta := map[string]any{
		"receiver": wantReceiver, "recv_field": wantField, "recv_self": true, "scope_pkg": wantScope,
		"unrelated": []string{"a", "b"},
	}
	flat, err := encodeMeta(meta)
	if err != nil {
		t.Fatal(err)
	}
	jsonBlob, err := encodeMeta(map[string]any{
		"receiver": wantReceiver, "recv_field": wantField, "recv_self": true, "scope_pkg": wantScope,
		"unsupported": []int{1, 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if isFlatMeta(jsonBlob) {
		t.Fatal("unsupported value did not force JSON fallback")
	}
	var gobBuf bytes.Buffer
	if err := gob.NewEncoder(&gobBuf).Encode(meta); err != nil {
		t.Fatal(err)
	}

	for name, blob := range map[string][]byte{
		"flat": flat, "json": jsonBlob, "gob": gobBuf.Bytes(),
	} {
		t.Run(name, func(t *testing.T) {
			receiver, field, self, scope, err := decodeReceiverMutationMeta(blob)
			if err != nil {
				t.Fatal(err)
			}
			if receiver != wantReceiver || field != wantField || !self || scope != wantScope {
				t.Fatalf("decoded (%q, %q, %v, %q), want (%q, %q, true, %q)", receiver, field, self, scope, wantReceiver, wantField, wantScope)
			}
		})
	}

	if len(flat) < 2 {
		t.Fatal("flat fixture unexpectedly short")
	}
	if _, _, _, _, err := decodeReceiverMutationMeta(flat[:len(flat)-1]); err == nil {
		t.Fatal("truncated flat metadata was accepted")
	}
	wrongTypes, err := encodeMeta(map[string]any{
		"receiver": 7, "recv_field": false, "recv_self": "yes", "scope_pkg": 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	receiver, field, self, scope, err := decodeReceiverMutationMeta(wrongTypes)
	if err != nil {
		t.Fatal(err)
	}
	if receiver != "" || field != "" || self || scope != "" {
		t.Fatalf("wrongly typed selected values leaked through: (%q, %q, %v, %q)", receiver, field, self, scope)
	}
}

func TestReceiverMutationProjectionPagesAndReentry(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	nodes := []*graph.Node{
		{ID: "method::a", Kind: graph.KindMethod, Name: "A", FilePath: "pkg/type.java", Language: "java", Meta: map[string]any{"receiver": "Type", "scope_pkg": "com.example"}},
		{ID: "method::b", Kind: graph.KindMethod, Name: "B", Meta: map[string]any{"receiver": "Type"}},
		{ID: "method::target", Kind: graph.KindMethod, Name: "Target"},
		{ID: "field::items", Kind: graph.KindField, Name: "items", FilePath: "pkg/type.java", Language: "java", Meta: map[string]any{"receiver": "Type", "scope_pkg": "com.example"}},
	}
	edges := []*graph.Edge{
		{From: "method::a", To: "field::items", Kind: graph.EdgeWrites},
		{From: "method::b", To: "method::target", Kind: graph.EdgeCalls, FilePath: "b.go", Line: 9, Meta: map[string]any{"recv_self": true}},
		{From: "method::b", To: "external::append", Kind: graph.EdgeCalls, FilePath: "b.go", Line: 10, Meta: map[string]any{"recv_field": "items"}},
		{From: "method::b", To: "external::ignored", Kind: graph.EdgeCalls, Meta: map[string]any{"other": true}},
	}
	s.AddBatch(nodes, edges)

	var methods []graph.ReceiverMutationMethod
	insertedAfterHighWater := false
	s.ScanReceiverMutationMethods(1, func(page []graph.ReceiverMutationMethod) bool {
		methods = append(methods, page...)
		if len(page) > 0 && s.GetNode(page[0].ID) == nil {
			t.Fatalf("cursor callback could not re-enter store for %q", page[0].ID)
		}
		if !insertedAfterHighWater {
			s.AddBatch([]*graph.Node{{
				ID: "zzzz::late-method", Kind: graph.KindMethod, Name: "Late",
				Meta: map[string]any{"receiver": "Type"},
			}}, nil)
			insertedAfterHighWater = true
		}
		return true
	})
	if len(methods) != 3 {
		t.Fatalf("methods=%d, want frozen high-water count 3", len(methods))
	}
	assertReceiverMutationScope(t, methods[0])
	if s.GetNode("zzzz::late-method") == nil {
		t.Fatal("re-entrant high-water fixture was not persisted")
	}

	var fields []graph.ReceiverMutationField
	s.ScanReceiverMutationFields(1, func(page []graph.ReceiverMutationField) bool {
		fields = append(fields, page...)
		return true
	})
	if len(fields) != 1 || fields[0].ID != "field::items" || fields[0].Name != "items" || fields[0].Receiver != "Type" {
		t.Fatalf("fields=%#v", fields)
	}
	assertReceiverMutationScope(t, fields[0])

	var writes []graph.ReceiverMutationWrite
	s.ScanReceiverMutationWrites(1, func(page []graph.ReceiverMutationWrite) bool {
		writes = append(writes, page...)
		return true
	})
	if !reflect.DeepEqual(writes, []graph.ReceiverMutationWrite{{From: "method::a", To: "field::items"}}) {
		t.Fatalf("writes=%#v", writes)
	}

	var calls []graph.ReceiverMutationCall
	s.ScanReceiverMutationCalls(1, func(page []graph.ReceiverMutationCall) bool {
		calls = append(calls, page...)
		return true
	})
	wantCalls := []graph.ReceiverMutationCall{
		{From: "method::b", To: "method::target", TargetMethodName: "Target", RecvSelf: true, FilePath: "b.go", Line: 9},
		{From: "method::b", To: "external::append", RecvField: "items", FilePath: "b.go", Line: 10},
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls=%#v, want %#v", calls, wantCalls)
	}
}

func TestReceiverMutationProjectionSkipsMalformedMetadata(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.AddBatch(
		[]*graph.Node{
			{ID: "method::bad", Kind: graph.KindMethod, Name: "Bad", Meta: map[string]any{"receiver": "Type"}},
			{ID: "method::caller", Kind: graph.KindMethod, Name: "Caller", Meta: map[string]any{"receiver": "Type"}},
			{ID: "method::target", Kind: graph.KindMethod, Name: "Target"},
		},
		[]*graph.Edge{
			{From: "method::caller", To: "field::missing", Kind: graph.EdgeWrites},
			{From: "method::caller", To: "method::target", Kind: graph.EdgeCalls, Meta: map[string]any{"recv_self": true}},
		},
	)
	if _, err := s.writerDB.Exec(`UPDATE nodes SET meta = x'0001' WHERE id IN ('method::bad', 'method::target')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writerDB.Exec(`UPDATE edges SET meta = x'0001' WHERE kind = ?`, string(graph.EdgeWrites)); err != nil {
		t.Fatal(err)
	}

	var methods []graph.ReceiverMutationMethod
	s.ScanReceiverMutationMethods(8, func(page []graph.ReceiverMutationMethod) bool {
		methods = append(methods, page...)
		return true
	})
	for _, method := range methods {
		if method.ID == "method::bad" {
			t.Fatal("malformed method metadata was not omitted")
		}
	}
	var writes []graph.ReceiverMutationWrite
	s.ScanReceiverMutationWrites(8, func(page []graph.ReceiverMutationWrite) bool {
		writes = append(writes, page...)
		return true
	})
	if len(writes) != 0 {
		t.Fatalf("malformed write metadata produced %#v", writes)
	}
	var calls []graph.ReceiverMutationCall
	s.ScanReceiverMutationCalls(8, func(page []graph.ReceiverMutationCall) bool {
		calls = append(calls, page...)
		return true
	})
	if len(calls) != 1 || calls[0].TargetMethodName != "" {
		t.Fatalf("malformed target metadata should clear only target name: %#v", calls)
	}
}

func assertReceiverMutationScope(t *testing.T, row any) {
	t.Helper()
	for name, want := range map[string]string{"FilePath": "pkg/type.java", "Language": "java", "ScopePkg": "com.example"} {
		value := reflect.ValueOf(row).FieldByName(name)
		if !value.IsValid() || value.String() != want {
			t.Errorf("%T.%s=%v, want %q", row, name, value, want)
		}
	}
}

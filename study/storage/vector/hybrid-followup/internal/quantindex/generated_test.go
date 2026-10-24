package quantindex

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

func TestGeneratedRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "stream.bin")
	if err := BuildGenerated(context.Background(), p, 17, 384, 42); err != nil {
		t.Fatal(err)
	}
	idx, err := Open(p, "int8", 128)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	q := SyntheticVector(42, 0, 384)
	a, err := idx.Search(context.Background(), q, 10)
	if err != nil {
		t.Fatal(err)
	}
	b, err := idx.SearchWorkers(context.Background(), q, 10, 8)
	if err != nil || !reflect.DeepEqual(a, b) {
		t.Fatal(err)
	}
	if a[0].ID != "synthetic-000000000000" {
		t.Fatal(a[0])
	}
}

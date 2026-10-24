package main

import (
	"encoding/binary"
	"math"
	"reflect"
	"testing"
)

func TestTaskTieUsesBestChunkPosition(t *testing.T) {
	raw := make([]byte, 3*dimensions*4)
	binary.LittleEndian.PutUint32(raw[4:], math.Float32bits(1))
	binary.LittleEndian.PutUint32(raw[dimensions*4:], math.Float32bits(1))
	binary.LittleEndian.PutUint32(raw[2*dimensions*4:], math.Float32bits(1))
	docs := &matrix{raw: raw, Rows: 3}
	qraw := make([]byte, dimensions*4)
	binary.LittleEndian.PutUint32(qraw, math.Float32bits(1))
	queries := &matrix{raw: qraw, Rows: 1}
	d := metadata{Documents: []record{{ID: "a#first", Parent: "a"}, {ID: "b", Parent: "b"}, {ID: "a#last", Parent: "a"}}, Queries: []record{{ID: "b"}}, Parents: []string{"a", "b", "a"}}
	truth, err := exactTruth(d, docs, queries)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(truth.Queries[0].Parents, []string{"b", "a"}) {
		t.Fatal(truth.Queries[0].Parents)
	}
}

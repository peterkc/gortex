package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gxstudy/dataset"
)

const testRevision = "1111111111111111111111111111111111111111"
const testGenerator = "2222222222222222222222222222222222222222222222222222222222222222"

func tinyInput(t *testing.T) (string, string, input) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "fixture")
	if err := writeFixture(context.Background(), dir, testRevision, testGenerator, 20261101, 32, 3); err != nil {
		t.Fatal(err)
	}
	pin, err := dataset.Hash(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	in, err := loadInput(context.Background(), dir, pin)
	if err != nil {
		t.Fatal(err)
	}
	return dir, pin, in
}
func TestExistingNPYIdentityFormatRoundTrip(t *testing.T) {
	dir, pin, in := tinyInput(t)
	if in.matrix.dim != 768 || len(in.matrix.ids) != 32 || len(in.vectors) != 3 {
		t.Fatal("shape lost")
	}
	if !reflect.DeepEqual(in.matrix.raw[:768], in.matrix.raw[768:1536]) {
		t.Fatal("duplicate pair ties missing")
	}
	if reflect.DeepEqual(in.matrix.raw[:768], in.vectors[0]) {
		t.Fatal("held-out query copied an indexed vector")
	}
	second := filepath.Join(t.TempDir(), "again")
	if err := writeFixture(context.Background(), second, testRevision, testGenerator, 20261101, 32, 3); err != nil {
		t.Fatal(err)
	}
	secondPin, err := dataset.Hash(filepath.Join(second, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if secondPin != pin {
		t.Fatal("seeded generation is not reproducible")
	}
	if err := writeFixture(context.Background(), dir, testRevision, testGenerator, 20261101, 32, 3); err == nil {
		t.Fatal("existing fixture overwritten")
	}
}
func TestInputHashAndAliasRejection(t *testing.T) {
	dir, pin, _ := tinyInput(t)
	if _, err := loadInput(context.Background(), dir, testGenerator); err == nil {
		t.Fatal("wrong manifest hash accepted")
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := loadInput(context.Background(), alias, pin); err == nil {
		t.Fatal("symlink alias accepted")
	}
	f, err := os.OpenFile(filepath.Join(dir, "vectors.npy"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{0}, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := loadInput(context.Background(), dir, pin); err == nil {
		t.Fatal("changed input accepted under old manifest")
	}
}
func TestNPYRejectsUnsupportedAndPartialInputs(t *testing.T) {
	dir, _, in := tinyInput(t)
	original, err := os.ReadFile(filepath.Join(dir, "vectors.npy"))
	if err != nil {
		t.Fatal(err)
	}
	headerEnd := 10 + int(binary.LittleEndian.Uint16(original[8:10]))
	for _, name := range []string{"dtype", "fortran", "dimension", "partial", "trailing", "nonfinite", "unnormalized", "version"} {
		t.Run(name, func(t *testing.T) {
			data := append([]byte(nil), original...)
			switch name {
			case "dtype":
				data = []byte(strings.Replace(string(data), "'<f4'", "'|O4'", 1))
			case "fortran":
				data = []byte(strings.Replace(string(data), "False", "True ", 1))
			case "dimension":
				data = []byte(strings.Replace(string(data), "768)", "384)", 1))
			case "partial":
				data = data[:len(data)-1]
			case "trailing":
				data = append(data, 0)
			case "nonfinite":
				binary.LittleEndian.PutUint32(data[headerEnd:], math.Float32bits(float32(math.NaN())))
			case "unnormalized":
				binary.LittleEndian.PutUint32(data[headerEnd:], math.Float32bits(10))
			case "version":
				data[6] = 3
			}
			path := filepath.Join(t.TempDir(), "bad.npy")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := readNPY(context.Background(), path, 32, 3, in.documents); err == nil {
				t.Fatal("invalid NPY accepted", name)
			}
		})
	}
}
func TestIdentityRejections(t *testing.T) {
	for name, text := range map[string]string{
		"duplicate": "{\"id\":\"a\",\"parent_id\":\"a\",\"repo\":\"r\"}\n{\"id\":\"a\",\"parent_id\":\"a\",\"repo\":\"r\"}\n",
		"missing":   "{\"id\":\"a\",\"parent_id\":\"a\"}\n",
		"partial":   "{\"id\":",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "documents.jsonl")
			if err := os.WriteFile(path, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := readIdentities(path, 2); err == nil {
				t.Fatal("bad identity accepted")
			}
		})
	}
}
func TestEngineCheckerAndOmittedNeighbor(t *testing.T) {
	dir, pin, in := tinyInput(t)
	c := runConfig{Data: dir, FixturePin: pin, Out: t.TempDir(), Workers: 4, K: 3, Candidates: 16}
	for _, mode := range []string{"f32", "int8"} {
		if err := measure(context.Background(), c, mode); err != nil {
			t.Fatal(err)
		}
		if err := check(context.Background(), c, mode); err != nil {
			t.Fatal(err)
		}
		var engine engineResult
		var checked checkResult
		if err := readJSON(filepath.Join(c.Out, mode+"-engine.json"), &engine); err != nil {
			t.Fatal(err)
		}
		if err := readJSON(filepath.Join(c.Out, mode+"-check.json"), &checked); err != nil {
			t.Fatal(err)
		}
		if checked.Checked != 6 || (mode == "f32" && !checked.Exact) {
			t.Fatal("full workload not checked", checked)
		}
		originalID := engine.Samples[0].QueryID
		engine.Samples[0].QueryID = in.queries[1].ID
		if _, err := checkAnswers(context.Background(), in, c, mode, engine); err == nil {
			t.Fatal("reordered query identity accepted")
		}
		engine.Samples[0].QueryID = originalID
		engine.Samples[0].Hits = engine.Samples[0].Hits[1:]
		bad, err := checkAnswers(context.Background(), in, c, mode, engine)
		if err != nil || bad.Exact || len(bad.Mismatches) == 0 {
			t.Fatal("omitted neighbor did not fail exactness", bad, err)
		}
		engine.Samples = engine.Samples[1:]
		if _, err := checkAnswers(context.Background(), in, c, mode, engine); err == nil {
			t.Fatal("partial measured phase accepted")
		}
	}
	// The existing accuracy rows can contain body fields; this storage check consumes identities only.
	row := map[string]any{"id": "a", "parent_id": "a", "repo": "r", "body": "func f(){}"}
	body, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "documents.jsonl")
	if err := os.WriteFile(path, append(body, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readIdentities(path, 1); err != nil {
		t.Fatal(err)
	}
}

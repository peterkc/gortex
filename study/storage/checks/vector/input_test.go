package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gxstudy/adapter"
	"gxstudy/dataset"
)

const testRevision = "1111111111111111111111111111111111111111"
const testGenerator = "2222222222222222222222222222222222222222222222222222222222222222"

func tinyInput(t *testing.T) (string, string, input) {
	t.Helper()
	temporary, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(temporary, "fixture")
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
	dir, pin, in := tinyInput(t)
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
	path := filepath.Join(dir, "vectors.npy")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	headerEnd := 10 + int(binary.LittleEndian.Uint16(data[8:10]))
	bits := binary.LittleEndian.Uint32(data[headerEnd:])
	binary.LittleEndian.PutUint32(data[headerEnd:], bits^1)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	// This is still a valid, normalized NPY. Only the checksum can reject it.
	if _, _, err := readNPY(context.Background(), path, 32, 3, in.documents); err != nil {
		t.Fatal("payload mutation introduced an unrelated parse fault", err)
	}
	if _, err := loadInput(context.Background(), dir, pin); err == nil || err.Error() != "fixture input hash mismatch: vectors.npy" {
		t.Fatal("changed input did not fail its checksum guard", err)
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
	for _, test := range []struct {
		name, text string
		count      int
	}{
		{"duplicate", "{\"id\":\"a\",\"parent_id\":\"a\",\"repo\":\"r\"}\n{\"id\":\"a\",\"parent_id\":\"a\",\"repo\":\"r\"}\n", 2},
		{"missing-repo", "{\"id\":\"a\",\"parent_id\":\"a\"}\n", 1},
		{"partial", "{\"id\":", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "documents.jsonl")
			if err := os.WriteFile(path, []byte(test.text), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := readIdentities(path, test.count); err == nil {
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

// rewriteFixtureManifest changes only the named test fixture and returns its new pin.
func rewriteFixtureManifest(t *testing.T, dir string, change func(*fixtureManifest)) string {
	t.Helper()
	path := filepath.Join(dir, "manifest.json")
	var manifest fixtureManifest
	if err := readJSON(path, &manifest); err != nil {
		t.Fatal(err)
	}
	change(&manifest)
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	pin, err := dataset.Hash(path)
	if err != nil {
		t.Fatal(err)
	}
	return pin
}

func TestManifestGuards(t *testing.T) {
	for _, test := range []struct {
		name, message string
		change        func(*fixtureManifest)
	}{
		{"synthetic", "unsupported fixture identity/count/dimension/recipe", func(m *fixtureManifest) { m.Synthetic = false }},
		{"recipe", "unsupported fixture identity/count/dimension/recipe", func(m *fixtureManifest) { m.Recipe = "other recipe" }},
		{"source-revision", "unsupported fixture identity/count/dimension/recipe", func(m *fixtureManifest) { m.SourceRevision = "not-hex" }},
		{"generator-sha", "unsupported fixture identity/count/dimension/recipe", func(m *fixtureManifest) { m.GeneratorSHA256 = testRevision }},
		{"extra-hash", "fixture requires exactly documents.jsonl, queries.jsonl and vectors.npy hashes", func(m *fixtureManifest) { m.SHA256["extra"] = testGenerator }},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir, _, _ := tinyInput(t)
			pin := rewriteFixtureManifest(t, dir, test.change)
			if _, err := readManifest(dir, pin); err == nil || err.Error() != test.message {
				t.Fatal("manifest guard did not refuse its isolated fault", err)
			}
		})
	}
}

func TestQueryTargetGuards(t *testing.T) {
	for _, fault := range []string{"missing-parent", "wrong-repo"} {
		t.Run(fault, func(t *testing.T) {
			dir, _, in := tinyInput(t)
			rows := append([]identity(nil), in.queries...)
			if fault == "missing-parent" {
				rows[0].ID = "absent-parent"
			} else {
				rows[0].Repo = "other-repo"
			}
			var body strings.Builder
			encoder := json.NewEncoder(&body)
			for _, row := range rows {
				if err := encoder.Encode(row); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(dir, "queries.jsonl")
			if err := os.WriteFile(path, []byte(body.String()), 0600); err != nil {
				t.Fatal(err)
			}
			queryPin, err := dataset.Hash(path)
			if err != nil {
				t.Fatal(err)
			}
			pin := rewriteFixtureManifest(t, dir, func(m *fixtureManifest) { m.SHA256["queries.jsonl"] = queryPin })
			if _, err := loadInput(context.Background(), dir, pin); err == nil || err.Error() != "query target is absent from its repository" {
				t.Fatal("query-target guard did not refuse its isolated fault", err)
			}
		})
	}
}

func TestEngineIdentityGuards(t *testing.T) {
	dir, pin, in := tinyInput(t)
	c := runConfig{Data: dir, FixturePin: pin, Out: t.TempDir(), Workers: 4, K: 3, Candidates: 16}
	if err := measure(context.Background(), c, "f32"); err != nil {
		t.Fatal(err)
	}
	var original engineResult
	if err := readJSON(filepath.Join(c.Out, "f32-engine.json"), &original); err != nil {
		t.Fatal(err)
	}
	if checked, err := checkAnswers(context.Background(), in, c, "f32", original); err != nil || !checked.Exact {
		t.Fatal("unmodified engine result must be valid", checked, err)
	}
	for _, test := range []struct {
		name   string
		change func(*engineResult)
	}{
		{"mode", func(r *engineResult) { r.Mode = "int8" }},
		{"workers", func(r *engineResult) { r.Workers = 1 }},
		{"candidates", func(r *engineResult) { r.Candidates = 15 }},
		{"k", func(r *engineResult) { r.K = 2 }},
		{"documents", func(r *engineResult) { r.Documents = 31 }},
		{"dimensions", func(r *engineResult) { r.Dimensions = 384 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := original
			test.change(&changed)
			if _, err := checkAnswers(context.Background(), in, c, "f32", changed); err == nil || err.Error() != "engine result identity mismatch" {
				t.Fatal("engine-identity guard did not refuse its isolated fault", err)
			}
		})
	}
}

func TestMeasurePreservesSamplesAfterLateFailure(t *testing.T) {
	dir, pin, in := tinyInput(t)
	c := runConfig{Data: dir, FixturePin: pin, Out: t.TempDir(), Workers: 4, K: 3, Candidates: 16}
	complete := filepath.Join(c.Out, "f32-engine.json")
	old := []byte("existing output must not be adopted")
	if err := os.WriteFile(complete, old, 0600); err != nil {
		t.Fatal(err)
	}
	if err := measure(context.Background(), c, "f32"); !errors.Is(err, os.ErrExist) {
		t.Fatal("expected the final output collision after queries completed", err)
	}
	unchanged, err := os.ReadFile(complete)
	if err != nil || string(unchanged) != string(old) {
		t.Fatal("existing output changed", err)
	}
	partial, err := os.Open(filepath.Join(c.Out, "f32-samples.partial.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer partial.Close()
	decoder := json.NewDecoder(partial)
	var rows []sample
	for {
		var row sample
		err := decoder.Decode(&row)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	if len(rows) != 6 {
		t.Fatal("successful samples were lost on a later failure", len(rows))
	}
	for i, row := range rows {
		query := i % 3
		phase := "warmup"
		if i >= 3 {
			phase = "measure"
		}
		want, err := reference(context.Background(), in.matrix, in.vectors[query], c.K, make([]adapter.Hit, len(in.matrix.ids)))
		if err != nil || row.QueryID != in.queries[query].ID || row.Phase != phase || row.LatencyNS <= 0 || exactResults(want, row.Hits) != nil {
			t.Fatal("partial spool lost sample identity, order or answer", i, row, err)
		}
	}
}

func TestMeasureUnknownModeCreatesNoOutput(t *testing.T) {
	dir, pin, _ := tinyInput(t)
	c := runConfig{Data: dir, FixturePin: pin, Out: t.TempDir(), Workers: 1, K: 3, Candidates: 16}
	if err := measure(context.Background(), c, "unknown"); err == nil || err.Error() != `unknown scan mode "unknown"` {
		t.Fatal("unknown mode did not refuse at the pre-effect boundary", err)
	}
	entries, err := os.ReadDir(c.Out)
	if err != nil || len(entries) != 0 {
		t.Fatal("unknown mode created output before refusal", entries, err)
	}
}

package indexer

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer/merkle"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/resolver"
)

func TestRebuildGoOwnershipNativeAndRelocated(t *testing.T) {
	ctx := t.Context()
	const own = "example.org/main"
	const other = "example.org/nested"
	fixtures := map[string]string{
		"go.mod":                     "module " + own + "\n",
		"nested/go.mod":              "module " + other + "\n",
		"broken/go.mod":              "module malformed\nunknown-directive\n",
		"workspace/go.work":          "go 1.27.3\n",
		"workspace/deep/go.mod":      "module example.org/work\n",
		"caller.go":                  "package main\n",
		"shared/a.go":                "package duplicate\n",
		"shared/other.go":            "package duplicate\n",
		"nested/shared/b.go":         "package duplicate\n",
		"broken/shared/b.go":         "package duplicate\n",
		"workspace/deep/shared/b.go": "package duplicate\n",
		"vendor/example.org/d/c.go":  "package duplicate\n",
	}
	wantPaths := []string{
		"broken/go.mod", "broken/go.work", "broken/shared/go.mod", "broken/shared/go.work", "go.mod", "go.work",
		"nested/go.mod", "nested/go.work", "nested/shared/go.mod", "nested/shared/go.work", "shared/go.mod", "shared/go.work",
		"workspace/deep/go.work", "workspace/deep/shared/go.work", "workspace/go.work",
	}
	var first []rebuildGoManifest
	for i, dirname := range []string{"plain", "repo[1]"} {
		root := filepath.Join(t.TempDir(), dirname)
		var inputs []rebuildInput
		files := make(map[string]string)
		manifest := make(map[string]rebuildInput)
		for _, name := range slices.Sorted(maps.Keys(fixtures)) {
			data := fixtures[name]
			disk := filepath.Join(root, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(disk), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(disk, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if strings.HasSuffix(name, ".go") {
				inputs = append(inputs, rebuildGoTestInput(name, "go", data))
				dir := path.Dir(name)
				if dir == "." {
					dir = ""
				}
				files["r/"+name] = dir
			} else {
				manifest[name] = rebuildGoTestInput(name, "manifest", data)
			}
		}
		if i == 1 {
			slices.Reverse(inputs)
		}
		src, err := source.NewFilesystemSource(root)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = src.Close() })
		native, err := goPackageOwnershipFromFiles(ctx, src, "r", files)
		if err != nil || native == nil {
			t.Fatalf("native fixture authority: %v", err)
		}
		var rows []rebuildGoManifest
		limit := int64(1 << 20)
		observe := func(_ context.Context, name string) (*rebuildInput, error) {
			if in, ok := manifest[name]; ok {
				return &in, nil
			}
			if _, err := src.FrozenRoot().Lstat(filepath.FromSlash(name)); !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("no admitted identity or checked absence for %q: %v", name, err)
			}
			return nil, nil
		}
		err = captureRebuildGoManifests(ctx, inputs, &limit, observe, func(ctx context.Context, in rebuildInput) ([]byte, error) {
			return readFrozenRebuildInput(ctx, src, in)
		}, func(row rebuildGoManifest) error {
			rows = append(rows, row)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		byPath := make(map[string]rebuildGoManifest)
		var gotPaths []string
		for _, row := range rows {
			byPath[row.Path] = row
			gotPaths = append(gotPaths, row.Path)
		}
		if !slices.Equal(gotPaths, wantPaths) || byPath["go.mod"].RootClaim != own || byPath["nested/go.mod"].RootClaim != "" || byPath["nested/go.mod"].OwnerModule != other {
			t.Fatalf("captured probe group: %v", rows)
		}
		if first == nil {
			first = rows
		} else if !reflect.DeepEqual(rows, first) {
			t.Fatalf("moved root/reversed input changed the receipt: %v vs %v", rows, first)
		}
		for _, name := range []string{"go.mod", "nested/go.mod", "nested/shared/b.go"} {
			if err := os.Remove(filepath.Join(root, filepath.FromSlash(name))); err != nil {
				t.Fatal(err)
			}
		}
		cases := []struct {
			file, wantImport string
			kind             graph.NodeKind
			want             resolver.GoPackageOwnershipResult
		}{
			{"shared/a.go", own + "/shared", graph.KindFunction, resolver.GoPackageOwnershipExact},
			{"shared/other.go", own + "/shared", graph.KindFunction, resolver.GoPackageOwnershipExact},
			{"nested/shared/b.go", own + "/shared", graph.KindFunction, resolver.GoPackageOwnershipDifferent},
			{"nested/shared/b.go", other + "/shared", graph.KindFunction, resolver.GoPackageOwnershipExact},
			{"broken/shared/b.go", own + "/shared", graph.KindFunction, resolver.GoPackageOwnershipUnknown},
			{"workspace/deep/shared/b.go", "example.org/work/shared", graph.KindFunction, resolver.GoPackageOwnershipUnknown},
			{"vendor/example.org/d/c.go", "example.org/d", graph.KindFunction, resolver.GoPackageOwnershipUnknown},
			{"shared/absent.go", own + "/shared", graph.KindFunction, resolver.GoPackageOwnershipUnknown},
			{"shared/a.go", own + "/shared", graph.KindContract, resolver.GoPackageOwnershipUnknown},
		}
		for _, tc := range cases {
			candidate := resolver.GoImportCandidate{
				ImportPath: tc.wantImport, ImporterRepoPrefix: "r", ImporterFilePath: "r/caller.go",
				CandidateRepoPrefix: "r", CandidateFilePath: "r/" + tc.file,
				CandidateNode: &graph.Node{Kind: tc.kind, Language: "go", Name: "Shared"},
			}
			for _, compact := range []bool{false, true} {
				if compact && tc.kind == graph.KindContract {
					continue
				}
				if compact {
					candidate.CandidateNode = nil
				}
				got, err := rebuildGoOwnership(candidate, func(file string) (bool, error) {
					_, ok := files[file]
					return ok, nil
				}, func(rel string) (string, error) {
					if _, ok := files["r/"+rel]; !ok {
						t.Fatalf("raw provenance queried for graph nonmember %q", rel)
					}
					return rel, nil // graph and receipt paths are identical in this fixture
				}, func(name string) (rebuildGoManifest, error) { return byPath[name], nil })
				if err != nil || got != tc.want || got != native(candidate) {
					t.Fatalf("%s %s compact=%t: got %v, native %v, want %v, err %v", tc.file, tc.wantImport, compact, got, native(candidate), tc.want, err)
				}
			}
		}
		delete(byPath, "shared/go.mod")
		if _, err := rebuildGoNearest("shared", func(name string) (rebuildGoManifest, error) { return byPath[name], nil }); !errors.Is(err, errRebuildGoEvidence) {
			t.Fatalf("missing negative fell through to an outer module: %v", err)
		}
	}
}

func TestRebuildGoOwnershipRawAncestry(t *testing.T) {
	const raw = "cafe\u0301"
	const nfc = "caf\u00e9"
	candidate := resolver.GoImportCandidate{
		ImportPath: "example.org/raw/pkg", ImporterRepoPrefix: "r", ImporterFilePath: "r/caller.go",
		CandidateRepoPrefix: "r", CandidateFilePath: "r/" + nfc + "/pkg/b.go",
	}
	for _, omitRawModule := range []bool{false, true} {
		rows := rebuildGoManifests{
			"go.work":            {Path: "go.work"},
			"go.mod":             {Path: "go.mod", Present: true, OwnerModule: "example.org/root"},
			raw + "/pkg/go.work": {Path: raw + "/pkg/go.work"},
			raw + "/go.work":     {Path: raw + "/go.work"},
			raw + "/pkg/go.mod":  {Path: raw + "/pkg/go.mod"},
			raw + "/go.mod":      {Path: raw + "/go.mod", Present: true, OwnerModule: "example.org/raw"},
			nfc + "/pkg/go.work": {Path: nfc + "/pkg/go.work"},
			nfc + "/go.work":     {Path: nfc + "/go.work"},
			nfc + "/pkg/go.mod":  {Path: nfc + "/pkg/go.mod"},
			nfc + "/go.mod":      {Path: nfc + "/go.mod", Present: true, OwnerModule: "example.org/decoy"},
		}
		want := resolver.GoPackageOwnershipExact
		var wantErr error
		if omitRawModule {
			delete(rows, raw+"/go.mod")
			want, wantErr = resolver.GoPackageOwnershipUnknown, errRebuildGoEvidence
		}
		var graphFiles, origins, probes []string
		got, err := rebuildGoOwnership(candidate, func(file string) (bool, error) {
			graphFiles = append(graphFiles, file)
			return file == candidate.ImporterFilePath || file == candidate.CandidateFilePath, nil
		}, func(rel string) (string, error) {
			origins = append(origins, rel)
			if rel == nfc+"/pkg/b.go" {
				return raw + "/pkg/b.go", nil
			}
			return rel, nil // the importer has an ASCII receipt path
		}, func(name string) (rebuildGoManifest, error) {
			probes = append(probes, name)
			return rows.lookup(name)
		})
		if got != want || !errors.Is(err, wantErr) {
			t.Fatalf("omit raw module=%t: got %v, %v; want %v, %v", omitRawModule, got, err, want, wantErr)
		}
		if !slices.Equal(graphFiles, []string{"r/caller.go", "r/" + nfc + "/pkg/b.go"}) ||
			!slices.Equal(origins, []string{"caller.go", nfc + "/pkg/b.go"}) ||
			!slices.Equal(probes, []string{"go.work", "go.mod", raw + "/pkg/go.work", raw + "/go.work", "go.work", raw + "/pkg/go.mod", raw + "/go.mod"}) {
			t.Fatalf("omit raw module=%t: graph files %q, origins %q, probes %q", omitRawModule, graphFiles, origins, probes)
		}
	}
}

func TestRebuildGoOwnershipRawProvenanceRefusals(t *testing.T) {
	conflict := errors.New("conflicting raw receipts")
	candidate := resolver.GoImportCandidate{ImporterFilePath: "caller.go", CandidateFilePath: "pkg/b.go", ImportPath: "example.org/root/pkg"}
	for _, tc := range []struct {
		name, raw string
		cause     error
		noLookup  bool
	}{
		{name: "missing provider", noLookup: true},
		{name: "missing receipt"},
		{name: "different graph key", raw: "other.go"},
		{name: "escaping receipt", raw: "../caller.go"},
		{name: "conflicting receipts", raw: "caller.go", cause: conflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			origin := func(string) (string, error) { return tc.raw, tc.cause }
			if tc.noLookup {
				origin = nil
			}
			got, err := rebuildGoOwnership(candidate, func(file string) (bool, error) {
				return file == candidate.ImporterFilePath, nil
			}, origin, func(name string) (rebuildGoManifest, error) {
				t.Fatalf("manifest %q queried without raw provenance", name)
				return rebuildGoManifest{}, nil
			})
			if got != resolver.GoPackageOwnershipUnknown || !errors.Is(err, errRebuildGoEvidence) ||
				(tc.cause != nil && !errors.Is(err, tc.cause)) {
				t.Fatalf("got %v, %v; want Unknown with frozen Go evidence refusal (cause %v)", got, err, tc.cause)
			}
		})
	}
}

func TestRebuildGoOwnershipRootClaims(t *testing.T) {
	data := `module example.org/main
require example.org/dep v1.0.0
require example.org/dep/sub v1.0.0
require example.org/indirect v1.0.0 // indirect
replace example.org/dep => example.org/mirror v1.0.0
replace example.org/dep/sub => ./local
replace example.org/same => example.org/same v1.0.0
`
	in := rebuildGoTestInput("go.mod", "manifest", data)
	limit := int64(len(data))
	var rows []rebuildGoManifest
	err := captureRebuildGoManifests(t.Context(), nil, &limit, func(context.Context, string) (*rebuildInput, error) { return &in, nil },
		func(context.Context, rebuildInput) ([]byte, error) { return []byte(data), nil },
		func(row rebuildGoManifest) error { rows = append(rows, row); return nil })
	if err != nil || limit != 0 || len(rows) != 1 {
		t.Fatalf("root capture: rows %v remaining %d error %v", rows, limit, err)
	}
	row := rows[0]
	if row.RootClaim != "example.org/main" || row.OwnerModule != "" ||
		!slices.Equal(row.DirectDeps, []string{"example.org/dep", "example.org/dep/sub"}) {
		t.Fatalf("root claim should not certify ownership with replacements: %v", row)
	}
	for _, tc := range []struct {
		p, bridge string
		blocked   bool
		refuse    bool
	}{
		{"example.org/dep/sub/pkg", "example.org/dep/sub", false, true},
		{"example.org/dep/pkg", "example.org/dep", true, false},
		{"example.org/depot", "", false, false},
		{"example.org/same/pkg", "", false, false},
		{"example.org/indirect/pkg", "", false, false},
	} {
		blocked, err := row.rootReplaceBlocked(tc.p)
		if b := row.rootBridgeClaim(tc.p); b != tc.bridge || blocked != tc.blocked || errors.Is(err, errRebuildGoEvidence) != tc.refuse {
			t.Fatalf("%s: bridge %q, blocked %t, error %v", tc.p, b, blocked, err)
		}
	}
	for _, tc := range []struct {
		importPath string
		wantDir    string
		wantExists bool
	}{
		{"example.org/main/shared", "r/shared", true},
		{"example.org/main/no-go-file", "r/no-go-file", false},
		{"example.org/mainland/shared", "", false},
	} {
		var queried string
		exists, err := row.rootIndexedPackageExists("r", tc.importPath, func(dir string) (bool, error) {
			queried = dir
			return dir == "r/shared", nil
		})
		if err != nil || queried != tc.wantDir || exists != tc.wantExists {
			t.Fatalf("%s: queried %q exists %t error %v", tc.importPath, queried, exists, err)
		}
	}
	if _, err := row.rootIndexedPackageExists("r", "example.org/main/shared", func(string) (bool, error) {
		return false, errRebuildGoEvidence
	}); !errors.Is(err, errRebuildGoEvidence) {
		t.Fatalf("incomplete Go file group looked empty: %v", err)
	}
}

func TestRebuildGoOwnershipCaptureRefuses(t *testing.T) {
	const raw = "module example.org/main\n"
	good := rebuildGoTestInput("go.mod", "manifest", raw)
	link := good
	link.Kind = merkle.Symlink
	ignored := errors.New("ignored or unadmitted path")
	for _, tc := range []struct {
		name         string
		input        *rebuildInput
		observeErr   error
		remaining    int64
		bytes        string
		wantErr      error
		wantReads    int
		wantReceipts int
	}{
		{"known absent", nil, nil, 0, "", nil, 0, 1},
		{"ignored", nil, ignored, 0, "", ignored, 0, 0},
		{"symlink", &link, nil, 1 << 20, "", errRebuildGoEvidence, 0, 0},
		{"raw cap", &good, nil, 1, raw, errRebuildGoCaptureLimit, 0, 0},
		{"drift", &good, nil, 1 << 20, "module example.org/evil\n", ErrBuildInputChanged, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads, receipts := 0, 0
			err := captureRebuildGoManifests(t.Context(), nil, &tc.remaining, func(_ context.Context, name string) (*rebuildInput, error) {
				if name != "go.mod" {
					t.Fatalf("unrequested config %q", name)
				}
				return tc.input, tc.observeErr
			}, func(context.Context, rebuildInput) ([]byte, error) {
				reads++
				return []byte(tc.bytes), nil
			}, func(row rebuildGoManifest) error {
				receipts++
				if row.Path != "go.mod" || row.Present || row.Digest != (rebuildDigest{}) {
					t.Fatalf("not an explicit negative: %v", row)
				}
				return nil
			})
			if !errors.Is(err, tc.wantErr) || reads != tc.wantReads || receipts != tc.wantReceipts {
				t.Fatalf("reads %d receipts %d error %v", reads, receipts, err)
			}
		})
	}
	t.Run("distinct key cap", func(t *testing.T) {
		inputs := make([]rebuildInput, rebuildGoKeyLimit)
		for i := range inputs {
			inputs[i] = rebuildGoTestInput(fmt.Sprintf("d%d/f.go", i), "go", "package p")
		}
		var limit int64
		receipts := 0
		err := captureRebuildGoManifests(t.Context(), inputs, &limit,
			func(context.Context, string) (*rebuildInput, error) { return nil, nil },
			func(context.Context, rebuildInput) ([]byte, error) { t.Fatal("read a negative probe"); return nil, nil },
			func(rebuildGoManifest) error { receipts++; return nil })
		if !errors.Is(err, errRebuildGoCaptureLimit) || receipts != 0 {
			t.Fatalf("truncated receipt succeeded: count=%d err=%v", receipts, err)
		}
	})
}

func rebuildGoTestInput(rel, lang, data string) rebuildInput {
	return rebuildInput{Path: rel, Language: lang, Kind: merkle.RegularFile, Size: int64(len(data)), Content: blake3.Sum256([]byte(data))}
}

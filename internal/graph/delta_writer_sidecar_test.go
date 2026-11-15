package graph

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestDeltaWriterSidecarMissingOperations(t *testing.T) {
	cases := []struct {
		name  string
		write bool
		call  func(*DeltaWriter) error
	}{
		{"BulkSetFileMtimes", true, func(d *DeltaWriter) error { return d.BulkSetFileMtimes("repo", map[string]int64{"a.go": 1}) }},
		{"ReplaceFileMtimes", true, func(d *DeltaWriter) error { return d.ReplaceFileMtimes("repo", map[string]int64{"a.go": 1}) }},
		{"DeleteFileMtimes", true, func(d *DeltaWriter) error { return d.DeleteFileMtimes("repo", []string{"a.go"}) }},
		{"BatchUpsertSymbolFTS", true, func(d *DeltaWriter) error { return d.BatchUpsertSymbolFTS([]SymbolFTSItem{{NodeID: "a"}}) }},
		{"BatchDeleteSymbolFTS", true, func(d *DeltaWriter) error { return d.BatchDeleteSymbolFTS([]string{"a"}) }},
		{"BulkSetRefFacts", true, func(d *DeltaWriter) error { return d.BulkSetRefFacts("repo", []RefFact{{FromID: "a", ToID: "b"}}) }},
		{"DeleteRefFactsByFiles", true, func(d *DeltaWriter) error { return d.DeleteRefFactsByFiles("repo", []string{"a.go"}) }},
		{"SetFileMetas", true, func(d *DeltaWriter) error { return d.SetFileMetas("repo", []FileMetaRow{{FilePath: "a.go"}}) }},
		{"DeleteFileMetasByFiles", true, func(d *DeltaWriter) error { return d.DeleteFileMetasByFiles("repo", []string{"a.go"}) }},
		{"ReplaceFileIndexFailures", true, func(d *DeltaWriter) error {
			return d.ReplaceFileIndexFailures("repo", []FileIndexFailure{{Path: "a.go"}})
		}},
		{"ReplaceFileIndexFailures/clear", true, func(d *DeltaWriter) error { return d.ReplaceFileIndexFailures("repo", nil) }},
		{"ReplaceContentFiles", true, func(d *DeltaWriter) error {
			return d.ReplaceContentFiles("repo", []ContentFTSFileReplacement{{FilePath: "a.go"}})
		}},
		{"BulkSetCloneShingles", true, func(d *DeltaWriter) error { return d.BulkSetCloneShingles("repo", map[string][]uint64{"a": {1, 2}}) }},
		{"DeleteCloneShingles", true, func(d *DeltaWriter) error { return d.DeleteCloneShingles([]string{"a"}) }},
		{"WipeContent", true, func(d *DeltaWriter) error { return d.WipeContent("repo") }},
		{"WipeContentFile", true, func(d *DeltaWriter) error { return d.WipeContentFile("a.go") }},
		{"WipeContentFileInRepo", true, func(d *DeltaWriter) error { return d.WipeContentFileInRepo("repo", "a.go") }},
		{"ContentRepoHasRows", false, func(d *DeltaWriter) error { _, err := d.ContentRepoHasRows("repo"); return err }},
		{"AppendContent", true, func(d *DeltaWriter) error {
			return d.AppendContent("repo", []ContentFTSItem{{NodeID: "a", FilePath: "a.go", Body: "body"}})
		}},
		{"SearchContent", false, func(d *DeltaWriter) error { _, err := d.SearchContent("body", "repo", 1); return err }},
		{"BuildContentIndex", true, func(d *DeltaWriter) error { return d.BuildContentIndex() }},
		{"ScanContent", true, func(d *DeltaWriter) error {
			return d.ScanContent("repo", func(string, string, string) bool { return true })
		}},
	}
	for _, sidecar := range []string{"nil", "stripped"} {
		for _, tc := range cases {
			t.Run(sidecar+"/"+tc.name, func(t *testing.T) {
				// Given a generation that cannot provide the requested sidecar.
				var handle any
				if sidecar == "stripped" {
					handle = resolverProjectionStoreOnly{Store: New()}
				}
				dw := NewDeltaWriter(New(), handle)
				// When required nonempty work (or an authoritative reset) runs.
				err := tc.call(dw)
				// Then it is not a successful empty result; ignored writes poison publication.
				if !errors.Is(err, errors.ErrUnsupported) {
					t.Fatalf("operation returned %v, want unsupported", err)
				}
				if tc.write {
					refused := strings.Join(dw.Unsupported(), "; ")
					if !strings.Contains(refused, strings.Split(tc.name, "/")[0]) || !strings.Contains(refused, errors.ErrUnsupported.Error()) {
						t.Fatalf("refusal lacks operation/error context: %q", refused)
					}
				} else if len(dw.Unsupported()) != 0 {
					t.Fatal("an optional read probe must not poison a successful conservative fallback")
				}
			})
		}
	}
}

func TestDeltaWriterSidecarEmptyWork(t *testing.T) {
	// Given a graph-only delta with no sidecar, empty batches do no work.
	dw := NewDeltaWriter(New(), nil)
	calls := []func() error{
		func() error { return dw.BulkSetFileMtimes("repo", nil) },
		func() error { return dw.ReplaceFileMtimes("repo", nil) },
		func() error { return dw.DeleteFileMtimes("repo", nil) },
		func() error { return dw.BatchUpsertSymbolFTS(nil) },
		func() error { return dw.BatchDeleteSymbolFTS(nil) },
		func() error { return dw.BulkSetRefFacts("repo", nil) },
		func() error { return dw.DeleteRefFactsByFiles("repo", nil) },
		func() error { return dw.SetFileMetas("repo", nil) },
		func() error { return dw.DeleteFileMetasByFiles("repo", nil) },
		func() error { return dw.ReplaceContentFiles("repo", nil) },
		func() error { return dw.BulkSetCloneShingles("repo", nil) },
		func() error { return dw.DeleteCloneShingles(nil) },
		func() error { return dw.AppendContent("repo", nil) },
	}
	// When documented empty operations run, then no capability is required.
	for i, call := range calls {
		if err := call(); err != nil {
			t.Fatalf("empty call %d: %v", i, err)
		}
	}
	facts, err := dw.LoadRefFactsByTargets("repo", nil)
	if err != nil || facts == nil || len(facts) != 0 {
		t.Fatalf("empty targets = %v, %v", facts, err)
	}
	dw.AddNode(&Node{ID: "repo/a.go::A", Kind: KindFunction, FilePath: "repo/a.go"})
	payload := dw.Payload(map[string]struct{}{"repo/a.go": {}})
	if len(payload.Nodes) != 1 || len(dw.Unsupported()) != 0 {
		t.Fatalf("graph-only delta refused: %v", dw.Unsupported())
	}
}

type deltaSidecarCloneFailure struct {
	Store
	err error
}

func (s deltaSidecarCloneFailure) BulkSetCloneShingles(string, map[string][]uint64) error {
	return s.err
}
func (s deltaSidecarCloneFailure) DeleteCloneShingles([]string) error { return s.err }

func TestDeltaWriterSidecarSupportedFailureIsSticky(t *testing.T) {
	for _, failure := range []error{errors.New("injected clone sidecar failure"), context.Canceled} {
		for _, remove := range []bool{false, true} {
			// Given a supported generation writer returning an operational error.
			dw := NewDeltaWriter(New(), deltaSidecarCloneFailure{Store: New(), err: failure})
			// When the caller writes or deletes, then identity and refusal survive.
			var err error
			if remove {
				err = dw.DeleteCloneShingles([]string{"a"})
			} else {
				err = dw.BulkSetCloneShingles("repo", map[string][]uint64{"a": {1}})
			}
			if !errors.Is(err, failure) {
				t.Fatalf("returned %v, want %v", err, failure)
			}
			if refused := strings.Join(dw.Unsupported(), "; "); !strings.Contains(refused, failure.Error()) {
				t.Fatalf("missing backend failure in refusal: %q", refused)
			}
		}
	}
}

func TestDeltaWriterSidecarGraphRoundTrip(t *testing.T) {
	// Given Graph's real clone and metadata sidecars.
	sidecar := New()
	dw := NewDeltaWriter(New(), sidecar)
	for range 2 {
		// When writes repeat, then persisted results retain their native values.
		rows := map[string][]uint64{"a": {1, 2}}
		if err := dw.BulkSetCloneShingles("repo", rows); err != nil {
			t.Fatal(err)
		}
		got, err := sidecar.LoadCloneShingles("repo")
		if err != nil || !reflect.DeepEqual(rows, got) {
			t.Fatalf("clone rows = %v, %v", got, err)
		}
		if err := dw.SetFileMetas("repo", []FileMetaRow{{FilePath: "a.go", ContentHash: "hash"}}); err != nil {
			t.Fatal(err)
		}
		metas, err := sidecar.FileMetasForRepo("repo")
		if err != nil || len(metas) != 1 || metas[0].ContentHash != "hash" {
			t.Fatalf("metas = %v, %v", metas, err)
		}
		// Remove/re-add uses real sidecar mutation, not a fake successful stub.
		if err := dw.DeleteCloneShingles([]string{"a"}); err != nil {
			t.Fatal(err)
		}
		got, err = sidecar.LoadCloneShingles("repo")
		if err != nil || len(got) != 0 {
			t.Fatalf("clone delete result = %v, %v", got, err)
		}
		if err := dw.DeleteFileMetasByFiles("repo", []string{"a.go"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(dw.Unsupported()) != 0 {
		t.Fatalf("supported delta refused: %v", dw.Unsupported())
	}
}

type deltaSidecarEmptyRefFacts struct {
	Store
	err error
}

func (s deltaSidecarEmptyRefFacts) LoadRefFactsByFiles(string, []string) ([]RefFact, error) {
	return []RefFact{}, s.err
}
func (s deltaSidecarEmptyRefFacts) LoadRefFactsByTargets(string, []string) (map[string][]RefFact, error) {
	return map[string][]RefFact{}, s.err
}

func TestDeltaWriterSidecarRefFactsRequireRequestedPieces(t *testing.T) {
	for _, own := range []bool{false, true} {
		for _, below := range []bool{false, true} {
			for _, request := range []string{"own", "below", "both", "all", "targets"} {
				t.Run(fmt.Sprintf("own=%v/below=%v/%s", own, below, request), func(t *testing.T) {
					// Given independently stripped generation and below readers.
					var sidecar any = resolverProjectionStoreOnly{Store: New()}
					var base Reader = resolverProjectionStoreOnly{Store: New()}
					if own {
						sidecar = deltaSidecarEmptyRefFacts{Store: New()}
					}
					if below {
						base = deltaSidecarEmptyRefFacts{Store: New()}
					}
					dw := NewDeltaWriter(base, sidecar)
					dw.EvictFile("own.go")
					// When reading an exact subset, all files, or reverse targets.
					var err error
					switch request {
					case "own":
						_, err = dw.LoadRefFactsByFiles("repo", []string{"own.go"})
					case "below":
						_, err = dw.LoadRefFactsByFiles("repo", []string{"other.go"})
					case "both":
						_, err = dw.LoadRefFactsByFiles("repo", []string{"own.go", "other.go"})
					case "all":
						_, err = dw.LoadRefFactsByFiles("repo", nil)
					case "targets":
						_, err = dw.LoadRefFactsByTargets("repo", []string{"target"})
					}
					// Then only actually requested pieces need support; unknown is not empty.
					supported := own && below
					if request == "own" {
						supported = own
					}
					if request == "below" {
						supported = below
					}
					if supported {
						if err != nil {
							t.Fatal(err)
						}
					} else if !errors.Is(err, errors.ErrUnsupported) {
						t.Fatalf("incomplete facts returned %v", err)
					}
					if len(dw.Unsupported()) != 0 {
						t.Fatal("advisory fact reads must keep their checked error, not poison publication")
					}
				})
			}
		}
	}
}

func TestDeltaWriterSidecarRefFactsRetainBackendError(t *testing.T) {
	for _, failure := range []error{errors.New("injected reference facts failure"), context.Canceled} {
		for _, generation := range []bool{false, true} {
			// Given a supported reader whose requested piece fails.
			good := deltaSidecarEmptyRefFacts{Store: New()}
			bad := deltaSidecarEmptyRefFacts{Store: New(), err: failure}
			dw := NewDeltaWriter(good, bad)
			files := []string{"own.go"}
			if !generation {
				dw = NewDeltaWriter(bad, good)
				files = []string{"other.go"}
			}
			dw.EvictFile("own.go")
			// When files or targets are read, then the original error remains checked.
			_, err := dw.LoadRefFactsByFiles("repo", files)
			if !errors.Is(err, failure) {
				t.Fatalf("file read error = %v", err)
			}
			_, err = dw.LoadRefFactsByTargets("repo", []string{"target"})
			if !errors.Is(err, failure) {
				t.Fatalf("target read error = %v", err)
			}
		}
	}
}

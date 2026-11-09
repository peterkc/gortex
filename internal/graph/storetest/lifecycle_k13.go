package storetest

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// K13 Authored data and backend switch (design-c.md 1.8 and 5.2). Given
// notes, memories and tracking choices, when exporting, importing,
// rebuilding, rolling back and interrupting cutover, then stable authored IDs
// and relationships survive. Unknown schema, conflicts and incomplete export
// refuse cutover.
//
// D14: rebuild, rollback and remove-and-re-add run on every backend. Export,
// import and cutover run over every ordered pair in the backend list: a pair
// the table requires cuts over; every other pair refuses as unsupported.

// k13Authored is F's authored records of R, sorted by ID.
func k13Authored(f *FixtureF) []AuthoredRecord {
	out := []AuthoredRecord{f.Note, f.Memory}
	slices.SortFunc(out, func(a, b AuthoredRecord) int { return strings.Compare(a.ID, b.ID) })
	return out
}

func assertAuthored(t *testing.T, ctx context.Context, inst Instance, f *FixtureF, what string) {
	t.Helper()
	got, err := inst.Authored(ctx, "r")
	if err != nil {
		t.Fatalf("%s: authored records of r: %v", what, err)
	}
	if want := k13Authored(f); !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: authored records of r = %+v, want %+v", what, got, want)
	}
	other, err := inst.Authored(ctx, "s")
	if err != nil || len(other) != 0 {
		t.Fatalf("%s: authored records of s = %+v, %v; want none", what, other, err)
	}
}

// testK13AuthoredSurvivesRebuildAndRollback writes F's note and memory, then
// rebuilds R (E0, then E1), rolls the graph back to a new E0 build and
// recovers the files: the records keep their IDs and fields throughout.
func testK13AuthoredSurvivesRebuildAndRollback(t *testing.T, backend Backend) {
	ctx := caseContext(t)
	f := NewFixtureF(t)
	dir := t.TempDir()
	inst := newChainIn(t, ctx, backend, dir, nil, f, CheckoutR1, CheckoutS1)
	for _, record := range []AuthoredRecord{f.Note, f.Memory} {
		if err := inst.PutAuthored(ctx, "r", record); err != nil {
			t.Fatalf("write %s: %v", record.ID, err)
		}
	}
	assertAuthored(t, ctx, inst, f, "after writing")
	publishVersion(t, ctx, inst, f, CheckoutR1, E0)
	publishVersion(t, ctx, inst, f, CheckoutR1, E1)
	assertAuthored(t, ctx, inst, f, "after the E1 rebuild")
	rollback := sealedBuild(t, ctx, inst, CheckoutR1, f.Inputs(CheckoutR1, E0), "k13-rollback", f.Payload("r", E0))
	if err := rollback.Publish(ctx, current(t, ctx, inst, CheckoutR1), "k13-rollback-publish"); err != nil {
		t.Fatalf("roll back to E0: %v", err)
	}
	assertAuthored(t, ctx, inst, f, "after the rollback to E0")
	if err := inst.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	recovered := backend.Open(t, dir, nil)
	assertAuthored(t, ctx, recovered, f, "after recovery")
	assertServesF(t, ctx, openView(t, ctx, recovered, CheckoutR1), f, CheckoutR1, E0)
}

// testK13BackendPairs runs export, import and cutover over every ordered
// pair of listed backends. A pair the table does not require refuses as
// unsupported; a required pair cuts over.
func testK13BackendPairs(t *testing.T, backend Backend, all func(string) (Backend, bool)) {
	listed := Backends()
	for _, to := range listed {
		t.Run(backend.Name+"_to_"+to, func(t *testing.T) {
			target, ok := all(to)
			if !ok {
				t.Fatalf("backend %q is listed but has no adapter", to)
			}
			if cutoverRequired(backend.Name, to, listed) {
				k13Cutover(t, backend, target)
				return
			}
			k13PairRefuses(t, backend, target, listed)
		})
	}
}

// k13PairRefuses checks every absence independently: an unsupported export
// cannot hide the target's import refusal.
func k13PairRefuses(t *testing.T, from, to Backend, listed []string) {
	ctx := caseContext(t)
	f := NewFixtureF(t)
	source := newChainIn(t, ctx, from, t.TempDir(), nil, f, CheckoutR1)
	for _, record := range []AuthoredRecord{f.Note, f.Memory} {
		if err := source.PutAuthored(ctx, "r", record); err != nil {
			t.Fatalf("write %s: %v", record.ID, err)
		}
	}
	set, err := source.Export(ctx)
	if slices.Contains(lacks(from.Name, listed), CapabilityExport) {
		if !errors.Is(err, ErrUnsupported) {
			t.Fatalf("%s lacks export, but export returned %v, want an unsupported refusal", from.Name, err)
		}
		assertAuthored(t, ctx, source, f, "after the refused export")
	} else if err != nil {
		t.Fatalf("export from %s: %v", from.Name, err)
	}
	if slices.Contains(lacks(to.Name, listed), CapabilityImport) {
		// An unsupported import must refuse even without a usable source export.
		set = []byte(`{"schema_version":1,"records":[{"id":"test-local-note","kind":"note","text":"must not import"}]}`)
	} else if err != nil {
		return
	}
	target := to.Open(t, t.TempDir(), nil)
	if err := target.Import(ctx, set); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unrequired pair %s to %s: import returned %v, want an unsupported refusal", from.Name, to.Name, err)
	}
	for _, repo := range []string{"r", "s"} {
		if got, err := target.Authored(ctx, repo); err != nil || len(got) != 0 {
			t.Fatalf("the refused import left %+v, %v in %s's %s", got, err, to.Name, repo)
		}
	}
}

// k13Cutover exercises required pairs only. The source stays complete and
// available for rollback while the target imports side by side.
func k13Cutover(t *testing.T, from, to Backend) {
	ctx := caseContext(t)
	f := NewFixtureF(t)
	sourceDir := t.TempDir()
	source := newChainIn(t, ctx, from, sourceDir, nil, f, CheckoutR1)
	publishVersion(t, ctx, source, f, CheckoutR1, E0)
	for _, record := range k13Authored(f) {
		if err := source.PutAuthored(ctx, "r", record); err != nil {
			t.Fatalf("write %s: %v", record.ID, err)
		}
	}
	set, err := source.Export(ctx)
	if err != nil {
		t.Fatalf("export from %s: %v", from.Name, err)
	}
	if len(set) == 0 {
		t.Fatal("export returned an empty set")
	}
	t.Run("MalformedSetsRefuse", func(t *testing.T) {
		// FR-14 owns the interchange format. This test-local malformed set
		// claims an unknown schema without prescribing the export encoding.
		// FR-14 must supply a valid-envelope unknown-version fixture as well.
		unknownSchema := []byte(`{"schema_version":2147483647,"records":[]}`)
		for _, bad := range []struct {
			name string
			set  []byte
		}{{"Truncated", set[:len(set)/2]}, {"UnknownSchema", unknownSchema}} {
			t.Run(bad.name, func(t *testing.T) {
				target := to.Open(t, t.TempDir(), nil)
				if err := target.Import(ctx, bad.set); err == nil {
					t.Fatal("importing the malformed set succeeded")
				}
				if got, err := target.Authored(ctx, "r"); err != nil || len(got) != 0 {
					t.Fatalf("the refused import left %+v, %v", got, err)
				}
			})
		}
	})
	t.Run("CompleteAndRepeatedImport", func(t *testing.T) {
		target := to.Open(t, t.TempDir(), nil)
		for round := range 2 {
			if err := target.Import(ctx, set); err != nil {
				t.Fatalf("import %d into %s: %v", round+1, to.Name, err)
			}
			assertAuthored(t, ctx, target, f, "after import")
		}
	})
	t.Run("ConflictingIDsRefuse", func(t *testing.T) {
		target := to.Open(t, t.TempDir(), nil)
		conflict := f.Note
		conflict.Text += " conflicting text"
		if err := target.PutAuthored(ctx, "r", conflict); err != nil {
			t.Fatalf("seed the conflicting ID: %v", err)
		}
		if err := target.Import(ctx, set); err == nil {
			t.Fatal("importing a set with a conflicting authored ID succeeded")
		}
		if got, err := target.Authored(ctx, "r"); err != nil || !reflect.DeepEqual(got, []AuthoredRecord{conflict}) {
			t.Fatalf("the refused conflict changed the target to %+v, %v", got, err)
		}
	})
	t.Run("InterruptedImportAndRollback", func(t *testing.T) {
		dry := &stopper{}
		probe := to.Open(t, t.TempDir(), dry.hook)
		dry.arm(stop{})
		if err := probe.Import(ctx, set); err != nil {
			t.Fatalf("the unstopped import: %v", err)
		}
		arrivals, _ := dry.disarm()
		for _, at := range stopsOf(t, probe.Declaration().FaultPoints, arrivals) {
			t.Run(at.String(), func(t *testing.T) {
				s := &stopper{}
				dir := t.TempDir()
				target := to.Open(t, dir, s.hook)
				s.arm(at)
				err := target.Import(ctx, set)
				if crashErr := target.Crash(); crashErr != nil {
					t.Fatalf("end the interrupted target: %v", crashErr)
				}
				if _, stopped := s.disarm(); !stopped || !errors.Is(err, errStopped) {
					t.Fatalf("import at %s returned %v, stopped %v; want the stop", at, err, stopped)
				}
				recovered := to.Open(t, dir, nil)
				got, err := recovered.Authored(ctx, "r")
				if err != nil {
					t.Fatalf("authored records after recovery: %v", err)
				}
				if len(got) != 0 {
					assertAuthored(t, ctx, recovered, f, "the recovered complete import")
				}
				if other, err := recovered.Authored(ctx, "s"); err != nil || len(other) != 0 {
					t.Fatalf("recovered import changed S: %+v, %v", other, err)
				}
				// Roll back selection to the unchanged source; no import may
				// remove its old records or graph while target files exist.
				assertAuthored(t, ctx, source, f, "rollback after "+at.String())
				assertServesF(t, ctx, openView(t, ctx, source, CheckoutR1), f, CheckoutR1, E0)
			})
		}
	})
	if err := source.Close(); err != nil {
		t.Fatalf("close the rollback source: %v", err)
	}
	rollback := from.Open(t, sourceDir, nil)
	assertAuthored(t, ctx, rollback, f, "rollback after source restart")
	assertServesF(t, ctx, openView(t, ctx, rollback, CheckoutR1), f, CheckoutR1, E0)
}

// testK13TableRequiresCutover checks the table itself: once pebble is listed,
// SQLite must offer export and pebble import, so a declaration of either as
// absent fails, and every other pair is not required.
func testK13TableRequiresCutover(t *testing.T) {
	listed := []string{"sqlite", "pebble"}
	if !cutoverRequired("sqlite", "pebble", listed) {
		t.Fatal("with pebble listed, the table does not require the sqlite to pebble cutover")
	}
	for _, pair := range [][2]string{{"pebble", "sqlite"}, {"sqlite", "sqlite"}, {"pebble", "pebble"}} {
		if cutoverRequired(pair[0], pair[1], listed) {
			t.Errorf("the table requires the %s to %s cutover", pair[0], pair[1])
		}
	}
	for _, c := range []struct {
		name       string
		capability Capability
	}{{"sqlite", CapabilityExport}, {"pebble", CapabilityImport}} {
		allowed := expectations(listed)[c.name]
		declared := Declaration{
			FaultPoints:     allowed.faultPoints,
			SealedFileKinds: allowed.sealedFileKinds,
			Unsupported:     append(slices.Clone(allowed.unsupported), c.capability),
		}
		if len(declarationProblems(c.name, declared, listed)) == 0 {
			t.Errorf("%s may declare %s absent with pebble listed", c.name, c.capability)
		}
		for _, bad := range []struct {
			name        string
			declaration Declaration
		}{
			{"MissingFaultPoint", Declaration{FaultPoints: slices.Clone(allowed.faultPoints[1:]), SealedFileKinds: allowed.sealedFileKinds, Unsupported: allowed.unsupported}},
			{"WrongSealedFileKinds", Declaration{FaultPoints: allowed.faultPoints, SealedFileKinds: []string{"not-a-sealed-kind"}, Unsupported: allowed.unsupported}},
		} {
			t.Run(c.name+"/"+bad.name, func(t *testing.T) {
				if len(declarationProblems(c.name, bad.declaration, listed)) == 0 {
					t.Fatalf("the table accepted planted %s for %s", bad.name, c.name)
				}
			})
		}
		if c.name == "pebble" {
			t.Run(c.name+"/MissingSealedFileKind", func(t *testing.T) {
				declared := Declaration{FaultPoints: allowed.faultPoints, SealedFileKinds: allowed.sealedFileKinds[1:], Unsupported: allowed.unsupported}
				if len(declarationProblems(c.name, declared, listed)) == 0 {
					t.Fatal("the table accepted a missing required sealed-file kind")
				}
			})
		}
		allowedDecl := Declaration{FaultPoints: allowed.faultPoints, SealedFileKinds: allowed.sealedFileKinds, Unsupported: allowed.unsupported}
		if problems := declarationProblems(c.name, allowedDecl, listed); len(problems) > 0 {
			t.Errorf("the table rejects its own row for %s: %v", c.name, problems)
		}
	}
}

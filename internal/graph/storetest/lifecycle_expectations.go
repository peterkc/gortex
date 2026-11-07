package storetest

import (
	"fmt"
	"slices"
	"testing"
)

// The D14 expectation table: what each backend must declare, written by the
// suite and never read from a backend. A declaration the table does not allow
// fails, so a backend cannot pass a case by declaring the feature absent.

// Capability names an optional capability a backend may declare unsupported.
type Capability string

const (
	// CapabilityExport and CapabilityImport are K13's authored-data export
	// and import, which a cutover between two backends needs.
	CapabilityExport Capability = "export"
	CapabilityImport Capability = "import"
	// CapabilityPackProtocol is the signed OCI pack protocol of design-c.md
	// 4.4: pack discovery, signature trust and manifest merge. No backend
	// builds it in this plan, so K11 leaves those subcases untested.
	CapabilityPackProtocol Capability = "pack_protocol"
)

// Declaration is a backend's own inventory.
type Declaration struct {
	// FaultPoints names every durable boundary the backend's fault hook stops
	// at, in commit order.
	FaultPoints []string
	// SealedFileKinds names every kind of sealed payload file the backend
	// writes. A backend that writes none declares it empty.
	SealedFileKinds []string
	// Unsupported names each optional capability the backend lacks.
	Unsupported []Capability
}

// expectation is one backend's row of the table.
type expectation struct {
	knownGaps []knownGap
	// faultPoints must all be declared; a backend may declare more.
	faultPoints []string
	// sealedFileKinds must equal the declared inventory.
	sealedFileKinds []string
	// unsupported must equal the declared absences.
	unsupported []Capability
}

// expectations is the table for the given backend list.
func expectations(listed []string) map[string]expectation {
	return map[string]expectation{
		// FR-6: each payload write, seal, publish and catalog transition
		// commit, before and after the commit. SQLite writes no sealed files;
		// its crash points are its own durable boundaries.
		"sqlite": {
			knownGaps: []knownGap{
				{path: "K2/PublishRequiresSealedCandidate", kind: "feature", marker: "D18-K2-PublishRequiresSealedCandidate"},
				{path: "K3/FailedWriteRefusesSealAndPublish", kind: "feature", marker: "D18-K3-FailedWriteRefusesSealAndPublish"},
				{path: "K3/CancelledWriteRefusesSealAndPublish", kind: "feature", marker: "D18-K3-CancelledWriteRefusesSealAndPublish"},
				{path: "K4/LostReplyReturnsRecordedResult", kind: "feature", marker: "D18-K4-LostReplyReturnsRecordedResult"},
				{path: "K5/SealedOrphanIsAdopted", kind: "feature", marker: "D18-K5-SealedOrphanIsAdopted"},
				{path: "K5/UnsealedOrphanIsNotAdopted", kind: "feature", marker: "D18-K5-UnsealedOrphanIsNotAdopted"},
				{path: "K5/LiveOwnerRefusesAdoption", kind: "feature", marker: "D18-K5-LiveOwnerRefusesAdoption"},
				{path: "K6/RetireKeepsPreviousPublication", kind: "feature", marker: "D18-K6-RetireKeepsPreviousPublication"},
				{path: "K6/SelectionRace", kind: "driver", marker: "D18-K6-SelectionRace"},
				{path: "K9/MissingActivePayloadIsVisible", kind: "feature", marker: "D18-K9-MissingActivePayloadIsVisible"},
				{path: "K9/LinkIsAGuardedTransition", kind: "feature", marker: "D18-K9-LinkIsAGuardedTransition"},
				{path: "K10/ConflictingDuplicateRefusesSeal", kind: "feature", marker: "D18-K10-ConflictingDuplicateRefusesSeal"},
				{path: "K11/SymlinkEscapeRefusesAndKeepsFiles", kind: "feature", marker: "D18-K11-SymlinkEscapeRefusesAndKeepsFiles"},
				{path: "K11/AliasOfALiveStoreRefuses", kind: "feature", marker: "D18-K11-AliasOfALiveStoreRefuses"},
				{path: "K9/UntrackIsAllOrNone", kind: "driver", marker: "D18-K9-UntrackIsAllOrNone"},
				{path: "K9/StaleUntrackGuardChangesNothing", kind: "driver", marker: "D18-K9-StaleUntrackGuardChangesNothing"},
				{path: "K13/RemoveAndReAddKeepsAuthoredAndRevivesNothing", kind: "driver", marker: "D18-K13-RemoveAndReAddKeepsAuthoredAndRevivesNothing"},
			},
			faultPoints: []string{
				"payload_write_before_commit", "payload_write_after_commit",
				"seal_before_commit", "seal_after_commit",
				"publish_before_commit", "publish_after_commit",
				"catalog_before_commit", "catalog_after_commit",
			},
			unsupported: lacks("sqlite", listed),
		},
		// design-c.md 5.1 commit order, one point after each durable step,
		// and FR-6: each payload, seal, rename, pointer replace and directory
		// sync. The sealed-file kinds are the payload resources of 2.3, 3.1,
		// 4.2 and 4.3 that one seal binds.
		"pebble": {
			faultPoints: []string{
				"payload_write", "payload_sync",
				"seal_write", "seal_sync", "seal_dir_sync",
				"inventory_rename", "inventory_parent_sync",
				"manifest_write", "manifest_sync", "manifest_rename", "catalog_dir_sync",
				"pointer_write", "pointer_sync", "pointer_replace", "pointer_parent_sync",
			},
			sealedFileKinds: []string{
				"graph_base", "delta", "link_outputs",
				"symbol_text", "content_text", "vectors",
			},
			unsupported: lacks("pebble", listed),
		},
	}
}

// cutoverRequired reports whether K13 must cut over from one listed backend
// to another. Only sqlite to pebble is required, and only once pebble is
// listed; every other ordered pair must refuse as unsupported.
func cutoverRequired(from, to string, listed []string) bool {
	return from == "sqlite" && to == "pebble" && slices.Contains(listed, "pebble")
}

// lacks is what a backend must declare unsupported: the pack protocol always,
// export unless a required cutover starts at the backend, and import unless
// one ends at it. While sqlite is the only backend, it therefore lacks both.
func lacks(name string, listed []string) []Capability {
	exports, imports := false, false
	for _, other := range listed {
		exports = exports || cutoverRequired(name, other, listed)
		imports = imports || cutoverRequired(other, name, listed)
	}
	out := []Capability{CapabilityPackProtocol}
	if !exports {
		out = append(out, CapabilityExport)
	}
	if !imports {
		out = append(out, CapabilityImport)
	}
	return out
}

// checkDeclaration compares the backend's declaration with its table row.
func checkDeclaration(t *testing.T, backend Backend, listed []string) {
	t.Helper()
	got := backend.Open(t, t.TempDir(), nil).Declaration()
	for _, problem := range declarationProblems(backend.Name, got, listed) {
		t.Error(problem)
	}
}

// declarationProblems lists every way a declaration differs from what the
// table allows the named backend when listed is the backend list.
func declarationProblems(name string, got Declaration, listed []string) []string {
	want, ok := expectations(listed)[name]
	if !ok {
		return []string{fmt.Sprintf("the expectation table has no row for backend %q", name)}
	}
	var problems []string
	for _, point := range want.faultPoints {
		if !slices.Contains(got.FaultPoints, point) {
			problems = append(problems, fmt.Sprintf("%s declares fault points %v; missing %q", name, got.FaultPoints, point))
		}
	}
	if !sameSet(got.SealedFileKinds, want.sealedFileKinds) {
		problems = append(problems, fmt.Sprintf("%s declares sealed-file kinds %v, the table allows exactly %v",
			name, got.SealedFileKinds, want.sealedFileKinds))
	}
	if !sameSet(got.Unsupported, want.unsupported) {
		problems = append(problems, fmt.Sprintf("%s declares unsupported %v, the table allows exactly %v",
			name, got.Unsupported, want.unsupported))
	}
	return problems
}

// sameSet reports whether a and b hold the same values, each exactly once.
func sameSet[T comparable](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	for _, v := range a {
		if !slices.Contains(b, v) {
			return false
		}
	}
	for _, v := range b {
		if !slices.Contains(a, v) {
			return false
		}
	}
	return true
}

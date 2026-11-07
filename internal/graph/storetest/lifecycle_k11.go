package storetest

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// K11 Pack and path boundary (design-c.md 1.8, 4.4 and 5.2). Invalid pieces
// refuse before replacing accepted files; too-new formats refuse and remain
// untouched; aliases and symlink escapes refuse.
//
// D14: the local sealed-file boundary runs on a backend that writes sealed
// files; on SQLite the case runs the store-level refusals: a too-new format,
// a corrupt store, a store file that escapes its directory through a symlink,
// and an alias path to a store a live instance owns. The signed pack protocol
// is not built; the table makes every backend declare it unsupported.

// k11ClosedStore publishes E0 in a new directory and closes the instance.
func k11ClosedStore(t *testing.T, backend Backend) string {
	t.Helper()
	ctx := caseContext(t)
	f := NewFixtureF(t)
	dir := t.TempDir()
	inst := newChainIn(t, ctx, backend, dir, nil, f, CheckoutR1)
	publishVersion(t, ctx, inst, f, CheckoutR1, E0)
	if err := inst.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return dir
}

// testK11RefusesDamagedStore damages a closed store as kind says: opening
// it refuses, and no file it holds changes.
func testK11RefusesDamagedStore(t *testing.T, backend Backend, kind Damage) {
	dir := k11ClosedStore(t, backend)
	backend.Damage(t, dir, kind, "")
	before := fileBytes(t, dir)
	inst, err := backend.TryOpen(t, dir, nil)
	if err == nil {
		_ = inst.Close()
		if kind == DamageSymlinkEscape {
			t.Fatal("D18-K11-SymlinkEscapeRefusesAndKeepsFiles: opening a store damaged by symlink_escape succeeded")
		}
		t.Errorf("opening a store damaged by %s succeeded", kind)
	}
	assertFilesKept(t, dir, before, "opening a store damaged by "+string(kind))
}

// fileBytes reads every regular file under dir, following symlinked files
// (so an escaped target counts as the store's own bytes).
func fileBytes(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[path] = data
		return nil
	})
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	return out
}

// assertFilesKept checks that every file before held still holds the same
// bytes. A refusal may add files, such as a lock; it may not change, replace
// or remove one.
func assertFilesKept(t *testing.T, dir string, before map[string][]byte, what string) {
	t.Helper()
	for path, data := range before {
		now, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s removed %s", what, path)
			continue
		}
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !bytes.Equal(now, data) {
			t.Errorf("%s changed %s (%d bytes, was %d)", what, path, len(now), len(data))
		}
	}
}

// Given a valid native sealed resource, damage exactly that resource. First
// open/map refuses without replacing accepted bytes or changing selection.
func testK11LocalSealedFiles(t *testing.T, backend Backend) {
	for _, kind := range backend.Open(t, t.TempDir(), nil).Declaration().SealedFileKinds {
		for _, damage := range []Damage{DamageCorrupt, DamageSealedPartial, DamageSealedOversized, DamageSealedRecipe, DamageSealedVersion, DamageSealedDigest, DamageSealedSymlinkEscape} {
			t.Run(kind+"/"+string(damage), func(t *testing.T) { testSealedFileRefusal(t, backend, kind, damage) })
		}
	}
}

func testSealedFileRefusal(t *testing.T, backend Backend, kind string, damage Damage) {
	t.Helper()
	ctx := caseContext(t)
	f := NewFixtureF(t)
	dir := t.TempDir()
	inst := newChainIn(t, ctx, backend, dir, nil, f, CheckoutR1)
	base := current(t, ctx, inst, CheckoutR1)
	build, err := inst.PrepareSealedFile(ctx, kind, BuildRequest{Checkout: CheckoutR1, Inputs: f.Inputs(CheckoutR1, E0), Operation: "sealed-" + kind}, f.Payload("r", E0))
	if err != nil {
		t.Fatalf("prepare declared %s: %v", kind, err)
	}
	if err := build.Publish(ctx, base, "sealed-publish"); err != nil {
		t.Fatalf("publish valid %s: %v", kind, err)
	}
	selected := current(t, ctx, inst, CheckoutR1)
	assertServesF(t, ctx, openView(t, ctx, inst, CheckoutR1), f, CheckoutR1, E0)
	if err := inst.Close(); err != nil {
		t.Fatal(err)
	}
	if backend.DamageSealedFile == nil {
		t.Fatal("declared sealed files without a damage operation")
	}
	if err := backend.DamageSealedFile(ctx, dir, build.Version(), kind, damage); err != nil {
		t.Fatalf("damage declared %s: %v", kind, err)
	}
	before := fileBytes(t, dir)
	recovered, err := backend.TryOpen(t, dir, nil)
	if err == nil {
		view, mapErr := recovered.Open(ctx, CheckoutR1)
		if mapErr == nil {
			view.Release()
			t.Errorf("first map accepted %s damaged by %s", kind, damage)
		}
		if got := current(t, ctx, recovered, CheckoutR1); got != selected {
			t.Errorf("refusal moved R1 from %+v to %+v", selected, got)
		}
		if err := recovered.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// If opening itself refuses, byte equality includes the catalog/pointer,
	// so it also pins the old selection without opening a second owner.
	assertFilesKept(t, dir, before, "refused sealed file "+kind+"/"+string(damage))
}

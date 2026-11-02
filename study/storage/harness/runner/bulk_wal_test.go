package runner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveOnlyVerifiedEmptyBuildWALs(t *testing.T) {
	path := t.TempDir()
	identity, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{"000002.log": {0, 0, 0, 0, 0, 0, 5, 3, 0, 0, 0}, "000003.log": {}, "000004.sst": []byte("keep")} {
		if err := os.WriteFile(filepath.Join(path, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveEmptyBuildWALs(path, identity); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"000002.log", "000003.log"} {
		if _, err := os.Lstat(filepath.Join(path, name)); !os.IsNotExist(err) {
			t.Fatal("WAL not removed", name, err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(path, "000004.sst"))
	if err != nil || string(raw) != "keep" {
		t.Fatal("non-WAL modified", string(raw), err)
	}
}
func TestUnknownWALRefusesAllRemoval(t *testing.T) {
	eof := []byte{0, 0, 0, 0, 0, 0, 5, 4, 0, 0, 0}
	wrongCRC := append([]byte(nil), eof...)
	wrongCRC[0] = 1
	wrongType := append([]byte(nil), eof...)
	wrongType[6] = 1
	wrongLog := append([]byte(nil), eof...)
	wrongLog[7] = 9
	for _, raw := range [][]byte{wrongCRC, wrongType, wrongLog, append(append([]byte(nil), eof...), 0), []byte("real record")} {
		path := t.TempDir()
		identity, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		good := filepath.Join(path, "000002.log")
		bad := filepath.Join(path, "000003.log")
		if err := os.WriteFile(good, []byte{0, 0, 0, 0, 0, 0, 5, 3, 0, 0, 0}, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bad, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if err := RemoveEmptyBuildWALs(path, identity); err == nil {
			t.Fatal("unknown/record WAL accepted", raw)
		}
		for _, name := range []string{good, bad} {
			if _, err := os.Lstat(name); err != nil {
				t.Fatal("rejection removed evidence", name, err)
			}
		}
	}
}
func TestBuildWALCleanupRejectsReplacedOrAliasedOwnership(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "store")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	identity, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(path, "000002.log")
	if err := os.WriteFile(file, []byte{0, 0, 0, 0, 0, 0, 5, 3, 0, 0, 0}, 0600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveEmptyBuildWALs(path, identity); err == nil {
		t.Fatal("replaced directory accepted")
	}
	if _, err := os.Lstat(file); err != nil {
		t.Fatal("foreign file removed", err)
	}
	owned, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	if err := RemoveEmptyBuildWALs(alias, owned); err == nil {
		t.Fatal("directory alias accepted")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "outside")
	if err := os.WriteFile(outside, []byte{0, 0, 0, 0, 0, 0, 5, 3, 0, 0, 0}, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, file); err != nil {
		t.Fatal(err)
	}
	if err := RemoveEmptyBuildWALs(path, owned); err == nil {
		t.Fatal("WAL alias accepted")
	}
	if _, err := os.Lstat(outside); err != nil {
		t.Fatal("outside file removed", err)
	}
}

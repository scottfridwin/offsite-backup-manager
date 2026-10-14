package pipeline

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"

	"github.com/scottfridwin/offsite-backup-manager/internal/archive"
)

func TestLockOutputDirIsExclusive(t *testing.T) {
	dir := t.TempDir()
	unlock, err := lockOutputDir(dir)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	if _, err := lockOutputDir(dir); err == nil {
		t.Fatal("second lock should fail while the first is held")
	}
	unlock()
	unlock2, err := lockOutputDir(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	unlock2()
}

func TestWritePackageNeverOverwrites(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	staged := t.TempDir()
	writeFile(t, filepath.Join(staged, "a.txt"), "alpha")

	pkgPath := filepath.Join(t.TempDir(), "run-20260101T000000Z.tar.zst.age")
	writeFile(t, pkgPath, "published")

	_, _, _, err = writePackage(pkgPath, staged, []age.Recipient{id.Recipient()}, archive.Options{})
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("expected fs.ErrExist, got %v", err)
	}
	b, err := os.ReadFile(pkgPath)
	if err != nil || string(b) != "published" {
		t.Fatalf("existing package modified: %q, %v", b, err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(pkgPath), ".*.partial")); len(leftovers) != 0 {
		t.Fatalf("temporary package left behind: %v", leftovers)
	}
}
